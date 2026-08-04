# pulse — user guide

pulse runs AWS-style serverless apps — Lambda functions, an HTTP API, SQS
queues, DynamoDB tables — **entirely on your machine**. No AWS account, no
Docker, no deploys. Your code uses the normal AWS SDK and runs unchanged in
real AWS later.

**Contents**

1. [Get started in 2 minutes](#1-get-started-in-2-minutes)
2. [The three ideas you need](#2-the-three-ideas-you-need)
3. [Each functionality, with an example](#3-each-functionality-with-an-example)
4. [pulse.yaml reference](#4-pulseyaml-reference)
5. [Command cheat sheet](#5-command-cheat-sheet)
6. [When something goes wrong](#6-when-something-goes-wrong)
7. [Not built yet](#7-not-built-yet)

---

## 1. Get started in 2 minutes

```bash
pulse init shop --template order-pipeline --lang python
cd shop
pulse start
```

New terminal:

```bash
curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'
# → 201 {"id":"…","status":"pending",…}

curl localhost:3000/orders/<that-id>
# → {"status":"processed",…}   ← a background worker updated it
```

That's an API, a queue, a worker, and a database — all local. `init`
installed the dependencies for you; nothing to activate or configure.

---

## 2. The three ideas you need

**1. `pulse.yaml` is the blueprint.** It declares three things:
*functions* (your code), *triggers* (what runs them), *resources* (queues
and tables). Everything pulse does starts from this file.

**2. Functions run two ways.**
- **Sync** — you call and wait for the answer: an HTTP request, or `pulse invoke`.
- **Async** — you drop a message on a queue and walk away; pulse delivers it
  to the wired function, retries failures, dead-letters repeat offenders.

**3. `pulse start` is your local cloud being ON.** APIs answer, queues
deliver, tables exist — and both code edits and `pulse.yaml` edits apply
live, no restarts.

---

## 3. Each functionality, with an example

### 3.1 Create a project — `pulse init`

Creates a folder with working sample code and installs its dependencies
(npm, or a Python `.venv` that pulse finds by itself — you never activate it).

```bash
pulse init shop --template order-pipeline --lang python
```

- Templates: `order-pipeline` (API + queue + worker + table, `--lang node|python`),
  `node-api` / `python-api` (one hello function). `pulse init --list` shows them.
- `--no-install` skips dependency installation.

### 3.2 Start your local cloud — `pulse start`

```
pulse 0.1.0-dev — project shop (us-east-1)
  functions  3 (api, notifier, worker)     ← your code
  api        http://localhost:3000          ← your REST API
  routes     POST /orders → api             ← who answers what
  aws        http://127.0.0.1:50407 (sqs, dynamodb)   ← what the AWS SDK talks to
  control    http://127.0.0.1:50411         ← pulse's own plumbing
engine ready in 33ms — code & pulse.yaml changes apply live · Ctrl+C to stop
```

Leave it running. This terminal shows everything that happens (see 3.12).
Port 3000 taken? `pulse start --port 3210`.

### 3.3 HTTP APIs

A `http` trigger maps a URL to a function:

```yaml
triggers:
  - { type: http, method: GET, path: "/orders/{id}", function: api }
```

The function receives a real API Gateway event and returns the response:

```python
def handler(event, context):
    order_id = event["pathParameters"]["id"]      # from {id} in the path
    return {"statusCode": 200, "body": json.dumps({"id": order_id})}
```

```bash
curl localhost:3000/orders/42        # → 200 {"id": "42"}
```

- Paths support `{param}` and catch-all `{name+}`.
- Unknown path → pulse answers `404 {"message":"Not Found"}`. Your own
  validation (like a 422) is just your function returning that status.

### 3.4 Create your own function — `pulse add function`

One command creates a function: the `pulse.yaml` entry plus a working,
commented handler file.

```bash
pulse add function notifier
```

```
✓ added function notifier (python3.12)
  code   services/notifier/handler.py
  try    pulse invoke notifier -d '{"hello":1}'
```

Open `services/notifier/handler.py` — it's yours to edit. The starter
already handles all three ways a function can run (HTTP request, queue
batch, direct invoke), so it will work wherever you wire it later.

### 3.5 Run a function by hand — `pulse invoke`

`invoke` is your test bench. It runs **one function directly** — no route,
no queue, no setup — and immediately shows you the result and its logs. Use
it while writing a new function, when debugging one, or in CI scripts.

Try it on the function you just created (this is the `try` line pulse
printed in 3.4):

```bash
pulse invoke notifier -d '{"hello":1}'
```

```
✓ notifier · success · 3ms · request 370b791a
  22:49:50.651  stdout  invoked with: {"hello": 1}
{"ok": true}
```

The event JSON is simply what your function receives as `event` — pulse
passes it through untouched.

**Testing a trigger-wired function?** Give it an event shaped like its
trigger would send. Queue deliveries arrive as `{"Records": [...]}`, so this
runs the template's `worker` *as if* the queue had delivered — without
touching the queue. Templates ship ready-made sample events for exactly this:

```bash
pulse invoke worker -e events/sqs-message.json
```

- Rule of thumb: `curl` tests route + function · `pulse send` tests queue +
  function · `pulse invoke` tests **the function alone**.
- Works with the engine stopped. Exit code is 1 on failure — CI-safe.

### 3.6 Background jobs — queues + workers

A queue is a mailbox; a `sqs` trigger makes a function its worker. The
sender never waits.

```yaml
triggers:
  - { type: sqs, queue: order-events, function: worker }
resources:
  queues:
    order-events: {}          # that's a complete queue definition
```

Send a message — from code (`sqs.send_message(...)` with plain boto3), or by
hand:

```bash
pulse send order-events '{"id":"job-1"}'
```

Within a second or two, the engine console shows the delivery and the work:

```
⚙ sqs order-events → worker · batch of 1 · ok
  worker | processed order job-1 (attempt 1)
```

- Sending to an undeclared queue **auto-creates it** (declare it only to
  configure a DLQ or visibility).
- `pulse send` with the engine stopped parks the message; it's delivered on
  the next `pulse start`.

### 3.7 Retries and the dead-letter queue

If the worker fails a message, the queue redelivers it after
`visibilityTimeout` seconds. After `maxReceiveCount` failures it moves to
the dead-letter queue (DLQ) instead of retrying forever.

```yaml
resources:
  queues:
    order-events:
      dlq: order-events-dlq
      maxReceiveCount: 3
      visibilityTimeout: 5
    order-events-dlq: {}
```

Try it — the demo worker fails on purpose when the order has `"fail": true`:

```bash
curl -X POST localhost:3000/orders -d '{"sku":"X","fail":true}'
```

```
  worker | job … failing on purpose (attempt 1)   (then 2, then 3, 5s apart)
☠ order-events: message moved to dead-letter queue order-events-dlq after 3 receives
```

- Keep `visibilityTimeout` **larger than** the worker's `timeout` in real
  projects (the demo uses 5s so retries are fast to watch).
- A worker can fail just one message of a batch by returning its id in
  `batchItemFailures` — the sample workers show how.

### 3.8 Saving data — tables

A table's entire schema is its key. Every other field is just code — add
fields any time, no config change, exactly like real DynamoDB:

```yaml
resources:
  tables:
    orders:
      pk: id            # complete table definition (type defaults to S/string)
```

```python
table = boto3.resource("dynamodb").Table("orders")
table.put_item(Item={"id": "42", "sku": "A1", "status": "pending"})
item = table.get_item(Key={"id": "42"}).get("Item")
```

- Supported: Put/Get/Update/Delete, Query, Scan, conditions, batches,
  pagination. Unsupported things (indexes, transactions, nested paths) fail
  with a message saying exactly that — never silently wrong.
- Using an undeclared table? The error contains the yaml snippet to paste.
- Data survives restarts. `pulse list` shows item counts; the AWS CLI v2 can
  `dynamodb scan` against the `aws` URL from the banner.

**Worked example — GET an order by id (route + table together)**

The most common pattern in any CRUD app: a URL with an id, a table lookup,
a 404 when it doesn't exist.

The route (`pulse.yaml`):

```yaml
triggers:
  - { type: http, method: GET, path: "/orders/{id}", function: api }
```

The handler (`services/api/src/api.py`):

```python
import json, os
import boto3

_table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])

def handler(event, context):
    order_id = event["pathParameters"]["id"]        # the {id} from the URL
    item = _table.get_item(Key={"id": order_id}).get("Item")
    if not item:
        return {"statusCode": 404,
                "body": json.dumps({"error": f"order {order_id} not found"})}
    return {"statusCode": 200,
            "body": json.dumps(item, default=str)}  # default=str: DynamoDB numbers arrive as Decimal
```

Try it:

```bash
curl localhost:3000/orders/42
# → 404 {"error": "order 42 not found"}        (no such order yet)

curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'    # create one, note the id
curl localhost:3000/orders/<that-id>
# → 200 {"id":"…","sku":"A1","status":"processed",…}            (the real record)
```

Three things meet here: the `{id}` in the path arrives as
`event["pathParameters"]["id"]`, the table read is plain boto3, and the 404
is just your code deciding — pulse adds no magic in between.

### 3.9 Editing code — hot reload

Edit any handler file and save. Done — the next run uses the new code.

```
↻ hot reload: worker (1 change)
```

- No restarts, ever, for code changes.
- After installing a new package (`.venv/bin/pip install X` / `npm install X`),
  save any code file once so fresh workers pick it up.

### 3.10 Editing pulse.yaml — live apply

Save `pulse.yaml` and the running engine reshapes itself:

```
pulse.yaml changed — applying live…
✓ config applied — 3 function(s), 5 trigger(s), api http://localhost:3000
```

A broken save changes nothing — pulse lists the problems and keeps the old
config serving:

```
✗ pulse.yaml changed but has problems — keeping the current config:
  ✗ triggers[2].function: unknown function "workr" (did you mean "worker"?)
```

### 3.11 Scaffolding the rest — `pulse add`

You already met `pulse add function` (3.4). The same command wires
everything else, without hand-editing yaml (your comments survive; changes
apply live if the engine runs):

```bash
pulse add route POST /notify --function notifier  # wire a URL to a function
pulse add queue notifications --worker notifier   # queue + wiring (+ creates the function if missing)
pulse add table customers --pk email              # declare a table
```

- One function may serve many triggers — `notifier` above handles both the
  route and the queue.
- Hand-editing `pulse.yaml` works exactly as well; `pulse add` is just the
  shortcut.

### 3.12 Logs — what you see and where

The `pulse start` console streams everything:

| Line | Meaning |
|---|---|
| `POST /orders → api · 201 · 12ms` | An HTTP request and its outcome |
| `⚙ sqs order-events → worker · batch of 1 · ok` | A queue delivery |
| `  worker \| processed order …` | A function's `print`/`console.log` |
| `  worker ! Traceback …` | A function's stderr |
| `↻ hot reload: worker` | Code change picked up |
| `✓ config applied — …` | pulse.yaml change picked up |
| `☠ … moved to dead-letter queue` | A message gave up retrying |
| `✱ auto-created queue "x"` | You sent to an undeclared queue |

Per-function history and live tail:

```bash
pulse logs worker -n 50        # recent lines (works with engine stopped)
pulse logs worker --follow     # live stream
```

### 3.13 Seeing what exists — `pulse list` / `pulse validate`

```bash
pulse list
```

Shows every function, route, queue (with live depths), and table (with item
counts), plus whether the engine is running. `pulse validate` checks
`pulse.yaml` and reports **all** problems at once, with "did you mean…?"
suggestions.

### 3.14 Stopping, restarting, persistence

```bash
pulse stop      # or Ctrl+C in the start terminal
```

Everything survives a restart: table data, queued jobs (delivered on next
start), logs, history — it all lives in the project's `.pulse/` folder.
Delete `.pulse/` for a clean slate.

### 3.15 Environment variables — none required

No AWS credentials, no `.env`. pulse injects everything AWS needs
(`AWS_ENDPOINT_URL` pointing at the local mocks, dummy keys, region). Your
own settings go in the `env:` block per function:

```yaml
functions:
  api:
    env:
      TABLE_NAME: orders
```

Read them normally (`os.environ["TABLE_NAME"]`). In real AWS you set the
same names in your infra config and the identical code runs there.

---

## 4. pulse.yaml reference

The complete demo config, annotated:

```yaml
project: shop                    # name (lowercase, digits, hyphens)
region: us-east-1                # default region (optional)
api:
  port: 3000                     # optional; default 3000

functions:                       # ── your code ──
  api:
    runtime: python3.12          # nodejs18/20/22.x or python3.9–3.12
    handler: src.api.handler     # python: module.function · node: file.export
    codeDir: services/api        # folder with the code
    timeout: 10                  # seconds, enforced (default 3)
    memory: 256                  # MB, informational locally (default 128)
    env:                         # your app config
      TABLE_NAME: orders
  worker:
    runtime: python3.12
    handler: handler.handle
    codeDir: services/worker

triggers:                        # ── what runs them ──
  - { type: http, method: POST, path: /orders, function: api }
  - { type: http, method: GET,  path: "/orders/{id}", function: api }
  - { type: sqs,  queue: order-events, function: worker, batchSize: 10 }

resources:                       # ── only what you use ──
  tables:
    orders:
      pk: id                     # full form: pk: { name: id, type: S|N|B }, plus optional sk
  queues:
    order-events:
      dlq: order-events-dlq      # optional dead-letter wiring
      maxReceiveCount: 3
      visibilityTimeout: 5       # retry pace, seconds (default 30)
    order-events-dlq: {}
```

An app with no queues or tables simply omits `resources` entirely.

---

## 5. Command cheat sheet

| Command | Does |
|---|---|
| `pulse init <name> [-t tpl] [--lang node\|python]` | New project, dependencies included |
| `pulse start [--port N]` / `pulse stop` | Local cloud on / off |
| `pulse add function\|route\|queue\|table …` | Scaffold pieces, applied live |
| `pulse invoke <fn> [-d json \| -e file]` | Run a function synchronously |
| `pulse send <queue> <body> [--delay N]` | Queue a job |
| `pulse logs <fn> [-n N] [-f]` | History / live logs |
| `pulse list` / `pulse validate` | See everything / check config |
| `-C <dir>` on any command | Act on a project from anywhere |

---

## 6. When something goes wrong

| Symptom | Fix |
|---|---|
| `no pulse.yaml found` | `cd` into the project, or add `-C path/to/project` |
| `api port 3000 is unavailable` | Another engine is using it — `pulse start --port 3210` |
| Function logs "boto3/SDK not installed" | Run the install line from the project README (init was offline or `--no-install`) |
| `✗ pulse.yaml changed but has problems` | Read the printed list — it names each problem; old config still serving |
| `Task timed out after N seconds` | Handler exceeded its `timeout:` — raise it or fix the code |
| Jobs redelivered while still processing | Raise the queue's `visibilityTimeout` above the worker's `timeout` |
| Job never reaches the worker | Is the engine running? Delivery lives inside `pulse start`; check `pulse list` depths |
| AWS CLI errors mentioning "Query protocol" | Old CLI — install AWS CLI v2.13+ (`brew install awscli`) |
| Want a clean slate | `pulse stop`, delete the project's `.pulse/` folder |

---

## 7. Not built yet

- **Phase 5 (next):** event browser + replay, invocation history views, log
  search, `pulse doctor`.
- **Phase 6:** installers (Homebrew), cloud sync from a real AWS account,
  project sharing, Windows.
- **Backlog:** SNS, S3 + bucket events, DynamoDB streams/indexes/transactions,
  EventBridge, Step Functions, Go/Java/.NET runtimes.

Anything unsupported fails with a clear message saying so — if pulse did it
silently wrong instead, that's a bug: report it.
