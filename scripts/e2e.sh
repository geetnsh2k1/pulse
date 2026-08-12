#!/usr/bin/env bash
# pulse end-to-end acceptance test — drives the real binary exactly the way
# a user does, and asserts every step of the golden path:
#
#   init → start → HTTP request → queue → worker → table → events → replay
#   → invoke → tables → doctor → stop
#
# Usage:
#   scripts/e2e.sh                       # python, auto-picked port
#   scripts/e2e.sh --lang node           # node variant
#   scripts/e2e.sh --lang node --port 3210 --keep
#
# Quiet on success (one ✓ per step); on failure it prints the assertion,
# the console log, and exits non-zero. Designed for CI matrices: every
# runtime combination runs this same script.
set -uo pipefail

LANG_VARIANT="python"
PORT=""
KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --lang) LANG_VARIANT="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PULSE="${PULSE_BIN:-$ROOT/bin/pulse}"
[ -x "$PULSE" ] || { echo "✗ no pulse binary at $PULSE (run: go build -o bin/pulse ./cmd/pulse)" >&2; exit 2; }

# A free port, unless one was pinned. Ports are the #1 source of flaky
# parallel CI runs, so we probe rather than assume.
if [ -z "$PORT" ]; then
  PORT=$(awk 'BEGIN{srand();print int(3200+rand()*400)}')
  if command -v nc >/dev/null 2>&1; then
    while nc -z 127.0.0.1 "$PORT" 2>/dev/null; do PORT=$((PORT+1)); done
  fi
fi

WORK="$(mktemp -d)"
PROJECT="$WORK/shop"
CONSOLE="$WORK/console.log"
STEP=0

cleanup() {
  [ -d "$PROJECT" ] && (cd "$PROJECT" && "$PULSE" stop >/dev/null 2>&1 || true)
  [ -n "${ENGINE_PID:-}" ] && kill "$ENGINE_PID" 2>/dev/null || true
  if [ "$KEEP" = "1" ]; then echo "  workdir kept: $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

fail() {
  echo "✗ $1" >&2
  [ -n "${2:-}" ] && { echo "--- got ---" >&2; echo "$2" >&2; }
  if [ -f "$CONSOLE" ]; then echo "--- console ---" >&2; tail -40 "$CONSOLE" >&2; fi
  exit 1
}
ok() { STEP=$((STEP+1)); printf '  ✓ %s\n' "$1"; }

# assert <description> <haystack> <needle>
# Whitespace is squeezed out of both sides: Python's json.dumps writes
# `"status": "pending"` while Node's JSON.stringify writes
# `"status":"pending"` — the same fact, spelled differently.
assert() {
  hay=$(printf '%s' "$2" | tr -d ' \t')
  needle=$(printf '%s' "$3" | tr -d ' \t')
  case "$hay" in *"$needle"*) ok "$1" ;; *) fail "$1 — expected to contain: $3" "$2" ;; esac
}

echo "⚡ pulse e2e — lang=$LANG_VARIANT port=$PORT"

# ---------------------------------------------------------------- init
cd "$WORK"
out=$("$PULSE" init shop -t api-and-worker --lang "$LANG_VARIANT" 2>&1) \
  || fail "init exited non-zero" "$out"
assert "init created the project" "$out" "created project"
[ -f "$PROJECT/pulse.yaml" ] || fail "pulse.yaml missing after init"

cd "$PROJECT"
out=$("$PULSE" validate 2>&1) || fail "validate exited non-zero" "$out"
ok "pulse.yaml validates"

# ---------------------------------------------------------------- start
"$PULSE" start --port "$PORT" >"$CONSOLE" 2>&1 &
ENGINE_PID=$!

# Wait for the banner — the engine's own definitive "I'm up" signal.
ready=0
for _ in $(seq 1 90); do
  if grep -q "ready in" "$CONSOLE" 2>/dev/null; then ready=1; break; fi
  kill -0 "$ENGINE_PID" 2>/dev/null || fail "engine exited during startup"
  sleep 1
done
[ "$ready" = "1" ] || fail "no ready banner after 90s"
ok "start printed the ready banner"

# ...then confirm the gateway actually answers (any HTTP status will do).
code=000
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/orders/ping" 2>/dev/null || echo 000)
  [ "$code" != "000" ] && break
  sleep 1
done
[ "$code" = "000" ] && fail "gateway never answered on port $PORT"
ok "gateway answering on :$PORT"

# ------------------------------------------------- request → queue → worker
# The very first request cold-starts a worker process. On a loaded box (CI
# runners, a laptop mid-build) that can lose a race against the runtime's
# own startup, so give it three tries before calling it broken — three
# consecutive failures is a real fault, one is weather.
body=""
for attempt in 1 2 3; do
  body=$(curl -fsS -X POST "http://127.0.0.1:$PORT/orders" \
    -H 'content-type: application/json' -d '{"sku":"A1","qty":2}' 2>&1)
  case "$body" in *'"pending"'*) break ;; esac
  [ "$attempt" = "3" ] && fail "POST /orders never succeeded (3 attempts)" "$body"
  sleep 2
done
assert "POST /orders created an order" "$body" '"status": "pending"'

id=$(printf '%s' "$body" | sed -n 's/.*"id"[ ]*:[ ]*"\([^"]*\)".*/\1/p')
[ -n "$id" ] || fail "no order id in response" "$body"
ok "order id: ${id%%-*}…"

# the worker is async: poll the read side until it flips to processed
for i in $(seq 1 30); do
  got=$(curl -fsS "http://127.0.0.1:$PORT/orders/$id" 2>/dev/null || true)
  case "$got" in *'"processed"'*) break ;; esac
  sleep 1
done
assert "worker processed the queued job" "$got" '"processed"'
assert "console narrated the delivery" "$(cat "$CONSOLE")" "→ worker"

# ---------------------------------------------------------------- events
out=$("$PULSE" events 2>&1) || fail "events exited non-zero" "$out"
assert "events recorded the http trigger" "$out" "http"
assert "events recorded the sqs trigger" "$out" "sqs"

event_id=$(printf '%s' "$out" | awk '/success|error/{print $1; exit}')
[ -n "$event_id" ] || fail "no event id parsed from events output" "$out"
out=$("$PULSE" events replay "$event_id" 2>&1) || fail "replay exited non-zero" "$out"
assert "replay re-ran the recorded event" "$out" "success"

# ---------------------------------------------------------------- inspect
out=$("$PULSE" invoke worker -d '{"Records":[{"body":"{\"id\":\"e2e-direct\"}"}]}' 2>&1) \
  || fail "invoke exited non-zero" "$out"
ok "invoke ran the worker directly"

out=$("$PULSE" tables 2>&1) || fail "tables exited non-zero" "$out"
assert "tables lists the project table" "$out" "orders"

out=$("$PULSE" logs worker -n 5 2>&1) || fail "logs exited non-zero" "$out"
ok "logs readable"

# doctor exits non-zero only for real problems; warnings (e.g. a local
# runtime newer than the one the project declares) are an expected pass —
# CI runs several node/python versions against one declared runtime.
out=$("$PULSE" doctor 2>&1) || fail "doctor reported problems" "$out"
case "$out" in
  *"everything looks good"*|*"nothing blocking"*) ok "doctor passes (no blocking problems)" ;;
  *) fail "doctor output unrecognized" "$out" ;;
esac

# ------------------------------------------------------- import (offline)
# `pulse import aws --policy` is a document, not a call: no credentials, no
# network. Asserting it here gives the read-only policy printer real-binary
# coverage on every OS in the matrix, which unit tests can't do.
out=$("$PULSE" import aws --policy 2>&1) || fail "import aws --policy exited non-zero" "$out"
assert "import --policy prints the read-only policy" "$out" "PulseImportReadOnly"
assert "import --policy names a read action" "$out" "lambda:GetFunction"
case "$out" in
  *Delete*|*PutItem*|*CreateFunction*) fail "the printed policy contains a mutating action" "$out" ;;
  *) ok "import --policy stays read-only" ;;
esac

# ---------------------------------------------------------------- stop
out=$("$PULSE" stop 2>&1) || fail "stop exited non-zero" "$out"
assert "engine stopped cleanly" "$out" "stopped"
for i in $(seq 1 10); do kill -0 "$ENGINE_PID" 2>/dev/null || break; sleep 1; done
kill -0 "$ENGINE_PID" 2>/dev/null && fail "engine process still alive after stop"
ENGINE_PID=""

# data must survive the engine going away
out=$("$PULSE" events 2>&1) || fail "events unavailable after stop" "$out"
assert "history persists across restarts" "$out" "http"

echo "✓ e2e passed — $STEP assertions · $LANG_VARIANT · port $PORT"
