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
pulse init shop --template api-and-worker --lang python
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
pulse init shop --template api-and-worker --lang python
```

Or just `pulse init` with no arguments — it asks three quick questions
(name, template, language; Enter picks the default) and does the same thing.

- Templates (all take `--lang node|python`): `hello` — one function, the
  smallest start · `todo-api` — real CRUD on one table · `webhook-relay` —
  ack-fast webhook handling with retries + DLQ · `api-and-worker` — the full
  demo (API + queue + worker + table). `pulse init --list` shows them.
- `--no-install` skips dependency installation.

### 3.2 Start your local cloud — `pulse start`

```
pulse 0.1.0-dev — project shop (us-east-1)
  functions  3 (createOrder, getOrder, worker)   ← your code
  api        http://localhost:3000                ← your REST API
  routes     POST /orders → createOrder           ← who answers what
             GET /orders/{id} → getOrder
  aws        http://127.0.0.1:50407 (sqs, dynamodb)   ← what the AWS SDK talks to
  control    http://127.0.0.1:50411                ← pulse's own plumbing
engine ready in 33ms — code & pulse.yaml changes apply live · Ctrl+C to stop
```

Leave it running. This terminal shows everything that happens (see 3.12).
Port 3000 taken? `pulse start --port 3210`.

### 3.3 HTTP APIs

A `http` trigger maps a URL to a function:

```yaml
triggers:
  - { type: http, method: GET, path: "/orders/{id}", function: getOrder }
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

Open `services/notifier/handler.py` — it's the classic Lambda shape, yours
to edit:

```python
def handler(event, context):
    print("received:", json.dumps(event))
    return {"ok": True}
```

It works wherever you wire it later — `event` is simply whatever triggers
it (an HTTP request, a queue batch, or your invoke JSON).

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
  22:49:50.651  stdout  received: {"hello": 1}
{"ok": true}
```

The event JSON is simply what your function receives as `event` — pulse
passes it through untouched. Longer event? Put it in a file:
`pulse invoke notifier -e event.json`.

- `invoke` skips URLs and queues on purpose — it tests **the function
  alone**. (Wiring the function up comes next.)
- Works with the engine stopped. Exit code is 1 on failure — CI-safe.

**Next: give it a URL.** A function and its URL are separate on purpose —
same as real AWS, where a Lambda exists on its own and an API Gateway route
pointing at it is a second thing (a function might be queue-only, or serve
several routes). One command wires it, applied live:

```bash
pulse add route POST /notify --function notifier
```

```bash
curl -X POST localhost:3000/notify -d '{"msg":"hi"}'    # → {"ok": true}
```

The starter returns a bare object, so pulse auto-wraps it as a 200 JSON
response; the console shows the request line plus `notifier | received: …`
with the full HTTP event your function saw. A URL is one way to trigger a
function — the other is a queue, and that's next.

### 3.6 Background jobs — queues + workers

Some work shouldn't happen while a caller waits: sending an email, resizing
an image, calling a slow third party. So your code drops a **message** on a
**queue** and replies immediately; a **worker** — an ordinary function —
picks the message up a moment later. If it fails, it retries automatically
(3.7).

The one rule: **you never call a worker yourself.** pulse watches the queue
and calls the worker whenever messages arrive:

```
sender ──▶ queue (mailbox) ──▶ pulse delivers ──▶ worker function
```

**Step 1 — create the whole chain with one command** (queue + worker
function + wiring):

```bash
pulse add queue emails --worker send-email
```

```
✓ added queue emails → send-email
  also created function send-email — its handler is services/send-email/handler.py
  try    pulse send emails '{"hello":1}'   (needs `pulse start` running to deliver)
  watch  pulse logs send-email -f
```

Behind the scenes that's two entries in `pulse.yaml` (hand-writing them
works exactly as well):

```yaml
triggers:
  - { type: sqs, queue: emails, function: send-email }
resources:
  queues:
    emails: {}               # that's a complete queue definition
```

**Step 2 — send a job and watch the console:**

```bash
pulse send emails '{"to":"ana@example.com"}'
```

```
⚙ sqs emails → send-email · batch of 1 · ok
  send-email | received: {"Records": [{…, "body": "{\"to\":\"ana@example.com\"}", …}]}
```

That log line shows the one thing to know about workers: queue messages
arrive wrapped in a `Records` **batch** (usually of one), and your message
is each record's `body` — as a string.

**Step 3 — make it a real worker.** Edit
`services/send-email/handler.py` to the standard three-line pattern and
save (hot reload does the rest):

```python
import json

def handler(event, context):
    for record in event["Records"]:        # deliveries arrive in batches
        job = json.loads(record["body"])   # your message, exactly as you sent it
        print("emailing", job["to"])
```

Send the same message again — `send-email | emailing ana@example.com`.
That's a working background job.

**Different jobs, different workers.** One queue per job type, each with
its own small function — the same command every time:

```bash
pulse add queue thumbnails --worker make-thumbnail
pulse add queue reports    --worker report-builder
```

- Each queue delivers only to its own worker (the template's pair is
  `order-events → worker`).
- `--worker some-existing-fn` attaches a function you already have — no new
  file; the output names the handler file that will receive the messages.
- Sending **from code** is plain SDK — see `services/create-order/handler.py`
  in the template: it queues a job for every new order in two lines.

Good to know:

- Rule of thumb: `curl` tests route + function · `pulse send` tests queue +
  function · `pulse invoke` tests the function alone. To test a worker
  *alone*, invoke it with a `Records`-shaped event — the template ships one:
  `pulse invoke worker -e events/sqs-message.json`.
- Sending to an undeclared queue **auto-creates it** (declare it only to
  configure retries/DLQ — next section).
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

For your own queues, `--dlq` at creation time writes exactly this shape:
`pulse add queue payments --worker charge --dlq`.

Try it — the demo worker raises on purpose when the order has `"fail": true`:

```bash
curl -X POST localhost:3000/orders -d '{"sku":"X","fail":true}'
```

```
  worker ! RuntimeError: order … failed on purpose (attempt 1)   (then 2, then 3, 5s apart)
☠ order-events: message moved to dead-letter queue order-events-dlq after 3 receives
```

- **Raising/throwing is how a worker says "retry me"** — the whole batch is
  redelivered. That's what the template worker does, and it's standard
  Lambda behavior.
- To fail just *one* message of a batch, return its id in
  `batchItemFailures` instead of raising (see the AWS docs pattern).
- Keep `visibilityTimeout` **larger than** the worker's `timeout` in real
  projects (the demo uses 5s so retries are fast to watch).

### 3.8 Saving data — tables

A table's entire schema is **its key** — every other field is just code.
Add fields any time, no config change, no migrations, exactly like real
DynamoDB.

```bash
pulse add table customers --pk email
```

```
✓ added table customers (pk email)
  your code can use it right away — no schema for the other columns: just write items
```

Behind the scenes, one entry in `pulse.yaml`:

```yaml
resources:
  tables:
    customers:
      pk: email         # complete table definition (type defaults to S/string)
```

Use it with the plain SDK — pulse points it at the local table automatically:

```python
customers = boto3.resource("dynamodb").Table("customers")

customers.put_item(Item={"email": "ana@x.com", "name": "Ana", "tier": "gold"})
item = customers.get_item(Key={"email": "ana@x.com"}).get("Item")
```

- Need to fetch *groups* of rows, not single ids? Add a **sort key**:
  `pulse add table events --pk userId --sk createdAt:N` — then Query returns
  "all events for user X, in time order". Key types: `S` string (default),
  `N` number, `B` binary.
- Supported: Put/Get/Update/Delete, Query, Scan, conditions, batches,
  pagination. Unsupported things (indexes, transactions, nested paths) fail
  with a message saying exactly that — never silently wrong.
- Tables don't auto-create (queues do) — a table needs *you* to choose its
  key. Use an undeclared one and the error hands you the exact yaml snippet
  to paste.
- Data survives restarts. `pulse list` shows item counts; the AWS CLI v2 can
  `dynamodb scan` against the `aws` URL from the banner.

**One function, many tables?** Nothing to wire — triggers declare *who
calls* a function; tables are just data your code opens by name, as many as
you like:

```python
ddb = boto3.resource("dynamodb")
orders    = ddb.Table(os.environ["ORDERS_TABLE"])
customers = ddb.Table(os.environ["CUSTOMERS_TABLE"])
```

```yaml
functions:
  createOrder:
    env:
      ORDERS_TABLE: orders
      CUSTOMERS_TABLE: customers
```

`pulse add table` writes that env line for you with `--function`:

```bash
pulse add table customers --pk email --function createOrder
```

Repeat `--function` for several functions; on an already-declared table it
wires env only. Reading names from `env:` instead of hardcoding
`"orders"` is a deploy habit, not a pulse rule — in real AWS, table names usually carry the stage
(`orders-dev`, `orders-prod`), so code takes them from env and runs
unchanged everywhere. Locally either style works. (One real-AWS difference:
there, each function also needs IAM permission per table — locally
everything is allowed.)

**Worked example — GET an order by id (route + table together)**

The most common pattern in any CRUD app: a URL with an id, a table lookup,
a 404 when it doesn't exist.

The route (`pulse.yaml`):

```yaml
triggers:
  - { type: http, method: GET, path: "/orders/{id}", function: getOrder }
```

The handler — this is literally `services/get-order/handler.py` in the
template:

```python
import json
import os

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    order_id = event["pathParameters"]["id"]  # the {id} from the URL
    item = table.get_item(Key={"id": order_id}).get("Item")
    if not item:
        return {"statusCode": 404, "body": json.dumps({"error": f"order {order_id} not found"})}
    # default=str: DynamoDB numbers come back as Decimal
    return {"statusCode": 200, "body": json.dumps(item, default=str)}
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

You've met the whole family now — this is the recap. `pulse add` edits
`pulse.yaml` for you (your comments survive; changes apply live if the
engine runs):

```bash
pulse add function notifier                       # code + yaml entry        (3.4)
pulse add route POST /notify --function notifier  # wire a URL to a function (3.5)
pulse add queue emails --worker send-email        # queue + worker + wiring  (3.6)
pulse add table customers --pk email              # declare a table          (3.8)
```

- One function may serve many triggers — `notifier` above could handle a
  route *and* a queue.
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
  createOrder:
    runtime: python3.12          # nodejs18/20/22.x or python3.9–3.12
    handler: handler.handler     # python: module.function · node: file.export
    codeDir: services/create-order   # folder with the code
    timeout: 10                  # seconds, enforced (default 3)
    memory: 256                  # MB, informational locally (default 128)
    env:                         # your app config
      TABLE_NAME: orders
      QUEUE_NAME: order-events
  getOrder:
    runtime: python3.12
    handler: handler.handler
    codeDir: services/get-order
    env: { TABLE_NAME: orders }
  worker:
    runtime: python3.12
    handler: handler.handler
    codeDir: services/worker
    env: { TABLE_NAME: orders }

triggers:                        # ── what runs them ──
  - { type: http, method: POST, path: /orders, function: createOrder }
  - { type: http, method: GET,  path: "/orders/{id}", function: getOrder }
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
| `pulse init` | New project — no arguments asks three quick questions |
| `pulse init <name> [-t tpl] [--lang node\|python]` | Same, fully scripted (CI-safe) |
| `pulse start [--port N]` / `pulse stop` | Local cloud on / off |
| `pulse add function\|route\|queue\|table …` | Scaffold pieces, applied live |
| `pulse add table <name> --function <fn>` | Declare table + wire its name into a function's env |
| `pulse invoke <fn> [-d json \| -e file]` | Run a function synchronously |
| `pulse send <queue> <body> [--delay N]` | Queue a job |
| `pulse logs <fn> [-n N] [-f]` | History / live logs |
| `pulse list` / `pulse validate` | See everything / check config |
| `-C <dir>` on any command | Act on a project from anywhere |

Every command answers `--help` with examples. **Tab completion** (function,
queue, and template names complete from *your* project):

```bash
echo 'source <(pulse completion zsh)' >> ~/.zshrc   # bash/fish/powershell work too
```

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
