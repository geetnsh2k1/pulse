# pulse — what exists today, and how to use it

*Updated: 2026-07-29 · reflects Phase 0 (foundations) + Phase 1 (run Lambda)*

## What pulse is

pulse runs AWS-Lambda-style applications **entirely on your machine** — no AWS
account, no Docker, no deploys. You describe your app in one file
(`pulse.yaml`), and pulse runs your functions with the same events, environment
variables, and error behavior they'd have in real AWS.

## What's built so far

| Piece | What it does |
|---|---|
| `pulse` CLI | One binary, 9 commands: `init`, `validate`, `list`, `invoke`, `logs`, `start`, `stop`, `version`, `--help` on everything |
| Project config | `pulse.yaml` — your app's blueprint: functions, triggers, resources. Strictly checked; typos get "did you mean…?" fixes |
| Function runner | Executes Node.js and Python handlers using AWS's real runtime protocol — request IDs, context object, timeouts, AWS-shaped errors |
| The engine | `pulse start` — a background program that keeps warm workers, watches your files, and answers on a local control API |
| Hot reload | Save a code file while the engine runs → next invoke runs the new code. No restarts |
| Logs | Every `console.log` / `print` is captured, timestamped, tagged with its request, stored in `.pulse/state.db`, and streamable live |
| Starter templates | `node-api`, `python-api`, and `order-pipeline` (the demo app the whole MVP is built around) |

**Not built yet** (this is by design — one phase at a time):
- ❌ `curl localhost:3000/...` → your API — **Phase 2 (next)**
- ❌ Real local queues (SQS) — Phase 3
- ❌ Real local database (DynamoDB) — Phase 4
- ❌ Event replay UX, richer inspection — Phase 5

So today, functions are triggered with `pulse invoke` + a JSON event file. The
HTTP/queue/database triggers declared in `pulse.yaml` are wiring that lights up
in the coming phases.

---

## One-time setup

Build the binary (already done if `bin/pulse` exists) and put it on your PATH:

```bash
cd ~/Desktop/pulse && make build
```

```bash
ln -sf ~/Desktop/pulse/bin/pulse /opt/homebrew/bin/pulse
```

```bash
pulse version
```

The symlink means every future `make build` updates the `pulse` command
automatically.

---

## Step 1 — Create a project

Projects live anywhere **outside** the pulse repo. See the available starters:

```bash
pulse init --list
```

Create one (this uses the simplest starter — one Node function):

```bash
cd ~/Desktop && pulse init my-app --template node-api
```

What you get:

```
my-app/
  pulse.yaml              ← the blueprint: functions + triggers
  functions/hello/
    index.mjs             ← the actual code (export const handler = ...)
  events/hello.json       ← a sample event to invoke with
  README.md
```

## Step 2 — Check and explore the project

```bash
cd ~/Desktop/my-app && pulse validate
```

```bash
cd ~/Desktop/my-app && pulse list
```

`validate` reports every config problem at once, with suggestions. `list`
shows functions, triggers, resources, and whether an engine is running.

## Step 3 — Run a function (the Phase 1 headline)

```bash
cd ~/Desktop/my-app && pulse invoke hello --event events/hello.json
```

You'll see three parts in the output:

```
✓ hello · success · 2ms · request 1f86c117     ← status · duration · request id
  18:47:02.276  stdout  saying hello to pulse  ← every log line, timestamped
{ "statusCode": 200, ... }                     ← what the function returned
```

Variations worth trying:

```bash
cd ~/Desktop/my-app && pulse invoke hello -d '{"queryStringParameters":{"name":"geetansh"}}'
```

No engine needs to be running — `invoke` boots a throwaway worker if needed.
If the function throws, you get the error type, message, and a stack trace
pointing at your file, and the command exits 1 (script/CI friendly).

## Step 4 — Start the engine and feel hot reload

Terminal 1:

```bash
cd ~/Desktop/my-app && pulse start
```

Leave it running. Terminal 2 — follow the logs live:

```bash
cd ~/Desktop/my-app && pulse logs hello --follow
```

Terminal 3 (or your editor): open `functions/hello/index.mjs`, change the
message text, and save. Terminal 1 prints:

```
↻ hot reload: hello (1 change)
```

Invoke again from any terminal — the response has your new text, served warm
in a few milliseconds:

```bash
cd ~/Desktop/my-app && pulse invoke hello --event events/hello.json
```

## Step 5 — Logs and history

Recent lines (works even when the engine is stopped — history lives in
`.pulse/state.db`):

```bash
cd ~/Desktop/my-app && pulse logs hello -n 50
```

## Step 6 — Stop

`Ctrl+C` in the engine terminal, or from anywhere:

```bash
cd ~/Desktop/my-app && pulse stop
```

State survives: restart later and your logs/history are still there.

---

## The golden-workflow demo app

The `order-pipeline` template is the app the whole MVP is being built around —
an orders API plus a background worker:

```bash
cd ~/Desktop && pulse init order-demo --template order-pipeline
```

Today (Phase 1) you drive both of its functions with sample events:

```bash
cd ~/Desktop/order-demo && pulse invoke api --event events/create-order.json
```

```bash
cd ~/Desktop/order-demo && pulse invoke worker --event events/sqs-message.json
```

Its README explains which parts light up in which phase — in Phase 2 the same
`api` function answers real `curl` requests on `localhost:3000`.

---

## Troubleshooting

| Symptom | Meaning / fix |
|---|---|
| `no pulse.yaml found in ...` | You're not inside a project — `cd` into one, or pass `-C path/to/project` |
| `note: using node v23...` | Harmless warning: your machine's Node/Python is newer than the runtime declared in `pulse.yaml`. Code still runs |
| `an engine for this project is already running` | `pulse stop` first (or use the running one — `invoke` does automatically) |
| `--follow needs a running engine` | `pulse start` in another terminal, then retry |
| Function hangs then fails with `Task timed out` | Your handler exceeded `timeout:` in `pulse.yaml` — raise it or fix the code |
| Weird state, want a clean slate | Stop the engine, delete the project's `.pulse/` folder (it's only local state) |

## Command reference

| Command | Purpose |
|---|---|
| `pulse init <name> [--template t] [--list]` | New project from a starter |
| `pulse validate` | Strict config check, all problems at once |
| `pulse list` | Functions, triggers, resources, engine status |
| `pulse invoke <fn> [-e file \| -d json]` | Run a function with an event |
| `pulse logs <fn> [-n N] [-f]` | Recent or live logs |
| `pulse start` / `pulse stop` | Run / stop the local environment |
| `pulse version` | Build info |
| `-C <dir>` (any command) | Act on a project you're not `cd`'d into |
