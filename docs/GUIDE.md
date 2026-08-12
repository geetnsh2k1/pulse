# ⚡ pulse — the guide

pulse runs AWS-style serverless apps — Lambda functions, an HTTP API, SQS
queues, DynamoDB tables — **entirely on your machine**. No AWS account, no
Docker, no deploys. Your code uses the normal AWS SDK and runs unchanged in
real AWS later.

**Contents**

1. [Start here](#1-start-here) — tour or two-minute manual start
2. [The three ideas you need](#2-the-three-ideas-you-need)
3. [Build](#3-build) — every piece, step by step
4. [Inspect](#4-inspect) — logs, history, replay, the live dashboard
5. [Everyday things](#5-everyday-things)
6. [pulse.yaml reference](#6-pulseyaml-reference)
7. [Command cheat sheet](#7-command-cheat-sheet)
8. [When something goes wrong](#8-when-something-goes-wrong)
9. [What pulse doesn't do yet](#9-what-pulse-doesnt-do-yet)

Throughout the guide: commands you type are in `bash` blocks, and the block
right after shows what you should see — treat those as **checkpoints**.

---

## 1. Start here

Already have a Lambda deployed in AWS? `pulse import aws` builds the project
from it — see [§3.13](#313-already-deployed--pulse-import-aws). Otherwise
start below; the guide teaches the pieces the import output refers to.

### The guided way (recommended, 5 minutes)

```bash
pulse tour
```

The tour builds a real project in `./pulse-tour` and walks the whole loop —
create, start, call over HTTP, add a background worker, send it a job,
replay history — one Enter press per step. Nothing is simulated: every step
runs the exact command it shows you. Press `q` anytime to stop; the folder
is yours to keep or delete.

### The manual way (2 minutes)

```bash
pulse init shop --template api-and-worker --lang python
cd shop
pulse start
```

**Checkpoint** — the banner appears, ending with:

```
ready in 33ms — code & pulse.yaml changes apply live · Ctrl+C to stop
```

Leave that terminal running (it's your local cloud being ON) and open a new
one. The banner printed a `try` line — paste it, or type:

```bash
curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'
```

**Checkpoint** — a `201` with a fresh order id:

```
{"id":"e9b4e51a-…","sku":"A1","qty":2,"status":"pending","createdAt":"…"}
```

Watch the first terminal: within a second a background worker picks the
order off a queue and processes it. Now read it back (your id, not this one):

```bash
curl localhost:3000/orders/e9b4e51a-…
```

**Checkpoint** — `"status": "processed"`. That round trip was an API, a
queue, a worker, and a database — all local, all real SDK calls. `init`
installed the dependencies for you; nothing to activate or configure.

New to any command? Run it **bare** — `pulse init`, `pulse add`,
`pulse logs`, `pulse remove` all ask you questions on a terminal instead of
demanding flags. Flags exist for scripts and muscle memory.

---

## 2. The three ideas you need

**1. `pulse.yaml` is the blueprint.** It declares three things:
*functions* (your code), *triggers* (what runs them), *resources* (queues
and tables). Everything pulse does starts from this file — and `pulse add`
/ `pulse remove` edit it for you.

**2. Functions run two ways.**
- **Sync** — you call and wait for the answer: an HTTP request, or `pulse invoke`.
- **Async** — you drop a message on a queue and walk away; pulse delivers it
  to the wired function, retries failures, dead-letters repeat offenders.

**3. `pulse start` is your local cloud being ON.** APIs answer, queues
deliver, tables exist — and both code edits and `pulse.yaml` edits apply
live, no restarts. Everything that happens is recorded, so you can search
it, inspect it, and replay it later (section 4).

---

## 3. Build

Each section answers *why you'd use the thing*, then walks it with real
commands and the output to expect.

### 3.1 Create a project — `pulse init`

Creates a folder with working sample code and installs its dependencies
(npm, or a Python `.venv` that pulse finds by itself — you never activate it).

The interactive way — three questions, Enter picks the default:

```bash
pulse init
```

```
? project name (my-app) › shop
? template — what should it start with?
    1. api-and-worker   ★ CRUD API + background worker + table — the full offline demo
    2. hello              One function behind GET /hello — the smallest start
    3. todo-api           Real CRUD on one table — create, list, complete, delete
    4. webhook-relay      Receive webhooks, ack fast, process with retries + DLQ
  pick 1-4 (1) ›
? language  1. node  2. python  (1) › 2
✓ created project shop from template api-and-worker (python) (9 files)
  ✓ creating .venv and installing python dependencies — done (6.6s)
```

Or fully scripted: `pulse init shop --template api-and-worker --lang python`
(`--no-install` skips dependencies; `pulse init --list` shows templates).

The templates are a **learning path** — each adds exactly one concept:

| Template | What it teaches |
|---|---|
| `hello` | One function behind `GET /hello` — the smallest start |
| `todo-api` | Real CRUD on one table (create, list, complete, delete) |
| `webhook-relay` | Ack-fast webhook handling with retries + a dead-letter queue |
| `api-and-worker` ★ | Everything together: API + queue + worker + table |

### 3.2 Start your local cloud — `pulse start`

```bash
pulse start
```

```
⚡ pulse 0.1.0-dev — shop (us-east-1)
  functions  createOrder · getOrder · worker
  api        http://localhost:3000
  routes     POST /orders → createOrder
             GET /orders/{id} → getOrder
  try        curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'
             curl localhost:3000/orders/123
  aws        http://127.0.0.1:62552 (sqs, dynamodb)
  control    http://127.0.0.1:62556
ready in 99ms — code & pulse.yaml changes apply live · Ctrl+C to stop
```

Reading it top to bottom: your **functions**, your **api** URL, which route
calls which function, ready-to-paste **try** commands (their bodies come
from the project's `events/` samples, so they actually succeed), the local
**aws** endpoint the SDK talks to, and pulse's own **control** port.

Leave it running — this terminal streams everything that happens (the full
vocabulary is in 4.1). Port 3000 taken? `pulse start --port 3210`.

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
commented handler file. (Bare `pulse add` asks what you want to add and
walks you through it — no flags to remember.)

```bash
pulse add function notifier
```

```
✓ added function notifier (python3.12)
  code   services/notifier/handler.py
  try    pulse invoke notifier -d '{"hello":1}'
  wire   pulse add route GET /notifier --function notifier · pulse add queue notifier-jobs --worker notifier
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
`pulse invoke notifier -e event.json`. No function name? Bare
`pulse invoke` lets you pick one.

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
is each record's `body` — as a string. (The very first job a project ever
completes also earns a one-time 🎉 line.)

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
  the next `pulse start`. `pulse peek emails` shows what's waiting (4.5).

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
- Between attempts, `pulse peek order-events` shows the message as
  `retried ×2`; after the ☠, `pulse list` shows it sitting in the DLQ.

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
- Data survives restarts. Look inside anytime with `pulse tables customers`
  (4.5); the AWS CLI v2 can also `dynamodb scan` against the banner's
  `aws` URL.

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
wires env only. Reading names from `env:` instead of hardcoding `"orders"`
is a deploy habit, not a pulse rule — in real AWS, table names usually carry
the stage (`orders-dev`, `orders-prod`), so code takes them from env and
runs unchanged everywhere. Locally either style works. (One real-AWS
difference: there, each function also needs IAM permission per table —
locally everything is allowed.)

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
  pulse.yaml: 1 problem found
  ✗ triggers[2].function: unknown function "workr" (did you mean "worker"?)
```

### 3.11 Secrets and local values — `.env`

`pulse.yaml` is committed, so it is the wrong home for an API key. Every
project pulse creates therefore ships two more files:

| file | committed? | what belongs in it |
|---|---|---|
| `.env` | **no** (gitignored) | real values for this machine — secrets, local URLs |
| `.env.example` | yes | the variable *names*, so teammates know what to set |

Every function receives everything in `.env`. When the same variable
appears in both places, the more specific one wins:

```
.env  (shared, uncommitted)  →  functions.<name>.env  (per-function)  →  pulse's own AWS_* wiring
```

So `.env` is your base layer, a function's `env:` overrides it for that
function, and the variables that make the local cloud work
(`AWS_ENDPOINT_URL`, `AWS_LAMBDA_RUNTIME_API`, …) always win.

```bash
cp .env.example .env     # then fill in values
pulse doctor             # confirms pulse sees the file and how many vars
```

Two deliberate behaviors worth knowing:

- **Your shell is not inherited.** In AWS a function sees only its
  configured variables, and pulse matches that — so a variable exported in
  your terminal will *not* appear inside a handler. Put it in `.env`.
- **Reserved names are refused, loudly.** AWS rejects variables like
  `AWS_REGION` and `AWS_ACCESS_KEY_ID` in function configuration; pulse
  does the same, because letting a project file override
  `AWS_ENDPOINT_URL` would quietly point your code away from the local
  cloud:

```
✗ .env: "AWS_ENDPOINT_URL" is reserved by the Lambda runtime and cannot be
  set (AWS rejects it too) — remove it; pulse sets it for you
```

`.env` files support comments, `export KEY=value`, quoted values with
`\n`/`\t` escapes, and `#` comments after unquoted values. Variable
expansion (`${OTHER}`) and multi-line values are intentionally not
supported, so a value never means something different than it looks.

### 3.12 Growing and shrinking — `pulse add` / `pulse remove`

You've met the add family — this is the recap, plus its inverse. Both edit
`pulse.yaml` surgically (your comments survive, the result is validated or
nothing changes, and changes apply live). Both work **bare** on a terminal:
they ask what and which, no flags needed.

```bash
pulse add function notifier                       # code + yaml entry        (3.4)
pulse add route POST /notify --function notifier  # wire a URL to a function (3.5)
pulse add queue emails --worker send-email        # queue + worker + wiring  (3.6)
pulse add table customers --pk email              # declare a table          (3.8)
```

```bash
pulse remove function notifier    # also drops triggers pointing at it
pulse remove route POST /notify
pulse remove queue emails         # also drops its sqs trigger
pulse remove table customers      # also cleans env vars pointing at it
```

`remove` is deliberately conservative: **code folders and stored data are
never deleted** — only the wiring. Each removal says what it kept (the code
path, the rows in `.pulse/`) so nothing disappears silently. A queue that's
another queue's dead-letter target refuses removal until you rewire it.

- One function may serve many triggers — `notifier` above could handle a
  route *and* a queue.
- Hand-editing `pulse.yaml` works exactly as well; `add`/`remove` are just
  the shortcuts.

### 3.13 Already deployed? — `pulse import aws`

Everything so far started from `pulse init`. If the function you care about
is already running in AWS, pulse can read it and build the project for you:

```bash
pulse import aws                 # asks: which profile, which region, which function
pulse import aws createOrder     # or name it outright
```

pulse uses the same credentials the `aws` CLI does — your profiles,
`AWS_PROFILE`, environment variables, an instance role, whatever you already
have. It prints the account before it reads anything, so you always know
whose account is in play.

**It only ever reads.** Import calls `List*`, `Get*` and `Describe*` and
nothing else: no AWS API that creates, changes or deletes is reachable from
this command. Running it cannot affect production.

What arrives, and how sure pulse is about each piece:

| Piece | Where it comes from | Certainty |
|---|---|---|
| runtime, handler, timeout, memory | the function's own configuration | fact |
| your handler code | the deployment package, unzipped into `functions/<name>/` | fact |
| `POST /orders` style routes | the function's resource policy, cross-checked with API Gateway | fact |
| queue triggers, batch size | its event source mappings | fact |
| a queue's visibility timeout and DLQ | `GetQueueAttributes` on the real queue | fact |
| a table's partition and sort keys | `DescribeTable` on the real table | fact |
| **which tables/queues your code uses** | the execution role's policy, environment variable values, then a scan of your code | **a guess you confirm** |

That last row is the one to understand. AWS records what *triggers* a
function, but nothing anywhere records that your code calls DynamoDB at
runtime — that fact lives only in your code. So pulse proposes the resources
it found evidence for, shows you the evidence, and lets you tick them off:

```
? include these? (checked ones have strong evidence)
    [x] 1. orders     table · env ORDERS_TABLE + iam policy
    [ ] 2. audit-log  table · name appears in the code
  toggle 1-2 · all · none · Enter accepts ›
```

Two independent signals (or one deliberate one, like an environment variable
holding the name) arrive pre-checked. Enter accepts what's shown. Everything
you tick is then read from AWS properly, so a table lands in `pulse.yaml`
with the same keys it has in production — not an approximation.

**Your environment values do not travel by default.** Lambda variables
routinely hold live API keys, so pulse writes the *names* to `.env` with
`CHANGE_ME` for every value:

```bash
pulse import aws createOrder --with-values   # opt in, if you really want the real ones
```

Four files come with the project: `pulse.yaml`, `.env` (gitignored, as in
§3.11), `.env.example`, and **`IMPORT-NOTES.md`** — the honest record of
everything AWS has that pulse can't represent. Layers, VPC configuration,
provisioned concurrency, secondary indexes, S3/SNS/EventBridge triggers: each
is printed on screen *and* written to that file, so the gap lives in your
repo instead of in a terminal that scrolls away. Read it first; the layer
warning in particular explains why an import may be missing dependencies.

pulse then installs the function's dependencies the same way `pulse init`
does — a root `.venv` for Python, `npm install` for Node — so the next step is
`pulse start` and not a copy-paste chore. Deployment packages usually ship
their dependencies already, in which case there is nothing to install and
pulse says nothing. `--no-install` skips it and prints the command instead.

Useful flags:

```bash
pulse import aws createOrder --dry-run    # show the plan and the pulse.yaml, write nothing (asks nothing)
pulse import aws createOrder --yes        # no prompts: take the pre-checked defaults (CI)
pulse import aws --name shop-api          # choose the project name and directory
pulse import aws --no-install             # don't install dependencies
pulse import aws --policy                 # print the read-only IAM policy this needs
```

Got the region wrong? pulse looks for the same name in the regions people
actually deploy to and tells you where it found it:

```
✗ "createOrder" isn't in us-east-1 — it's in eu-west-1
    fix: pulse import aws createOrder --region eu-west-1
```

`--policy` is the answer to an `AccessDenied`. It needs no credentials at
all, lists every action with the reason pulse needs it, and prints a policy
document you can hand to whoever administers the account — redirect it and
it's a file:

```bash
pulse import aws --policy > pulse-read-only.json
```

Limits, all of which refuse loudly rather than importing something broken:
zip-packaged functions only (not container images), Node 18+ and Python
3.10+, deployment packages under 250 MB, and one function per import into a
**new** directory — pulse will not write into an existing project.

Afterwards it's an ordinary pulse project: `pulse start`, edit, replay,
`pulse add` more pieces. Your handler code is untouched, still plain AWS SDK
code that runs the same in real AWS.

---

## 4. Inspect

Everything that happens in pulse is recorded — every trigger with its exact
payload, every log line, every outcome. This section is how you look at it.

### 4.1 The console — what you see and where

The `pulse start` terminal streams everything, color-coded:

| Line | Meaning |
|---|---|
| `POST /orders → createOrder · 201 · 12ms` | An HTTP request and its outcome |
| `⚙ sqs order-events → worker · batch of 1 · ok` | A queue delivery |
| `  worker \| processed order …` | A function's `print`/`console.log` |
| `  worker ! Traceback …` | A function's stderr |
| `↻ hot reload: worker` | Code change picked up |
| `✓ config applied — …` | pulse.yaml change picked up |
| `☠ … moved to dead-letter queue` | A message gave up retrying |
| `✱ auto-created queue "x"` | You sent to an undeclared queue |
| `🎉 first background job processed` | Once per project, on the first async win |

Every function gets its own stable color, so interleaved logs stay
readable. (No colors in pipes, CI, or with `NO_COLOR`/`--no-color`.)

### 4.2 Logs — tail, search, and one request's story

```bash
pulse logs worker -n 50               # recent lines (works with engine stopped)
pulse logs worker --follow            # live stream
pulse logs worker --grep "order-17"   # search the last 1000 lines (case-insensitive)
```

The 8-character **request id** shown everywhere (invoke results, events,
the console) unlocks the deepest view — everything about one request:

```bash
pulse logs --request d90e5295
```

```
⚡ request d90e5295 sqs → processWebhook · error · 2ms · 00:08

event
  {
    "Records": [
      { …the exact payload that arrived, pretty-printed… }
  … 6 more line(s)

logs
  00:08:15.903  stderr  Traceback (most recent call last): …

error
  RuntimeError: webhook 3625d493 failed on purpose (attempt 3)

re-run it against your current code: `pulse events replay d90e5295`
```

One screen: what arrived, what the function said, how it ended — and the
exact command to re-run it.

### 4.3 History & replay — time travel for debugging

Every trigger that ever hit a function is recorded with its **exact event
payload** and outcome:

```bash
pulse events          # `pulse history` works too
```

```
  8931cf5b  Aug  5 01:01   sqs    → processWebhook · error   · 1ms
  7275f6ee  Aug  5 01:01   http   → receiveWebhook · success · 1ms

replay any: `pulse events replay <id>` · narrow: `--function <fn>` · more: `-n 50`
```

**Replay** fires a recorded event again, byte for byte, through the code
you have **now**. That's the debugging loop: a weird payload crashed a
worker yesterday → fix the handler → replay the *actual* event → watch it
pass. No reconstructing inputs from log fragments.

```bash
pulse events replay 8931cf5b     # a unique id prefix is enough; bare = pick from a list
```

```
↻ replaying 8931cf5b — sqs → processWebhook, originally Aug  5 01:01

✓ processWebhook · success · 0ms · request 94aacd31
```

- Replay invokes the function **directly** with the stored event (like the
  Lambda console's "test" button) — it doesn't re-queue or re-send anything.
- Replays are recorded too (type `replay`), so history stays truthful.
- Exit code follows the outcome (CI-friendly); works with the engine stopped.

### 4.4 The live dashboard — `pulse monitor`

One full-screen view of the running project (start the engine first):

```bash
pulse monitor
```

```
⚡ pulse shop · ● live · api http://localhost:3000
functions                      logs — / filters
 createOrder        12✓        14:02:11 createOrder | order 9de0… saved
 worker             11✓ 1✗     14:02:11 ⚙ order-events → worker · ok
 getOrder           30✓        14:02:12 worker | processed order 9de0…
queues
 order-events       0·0·0
 order-events-dlq   1·0·0 !

events — tab to focus
▸ 8931cf5b 14:01 sqs → worker · error
q quit · tab focus events · ↑↓ scroll/select · Enter replay · / filter
```

- Functions with their success/failure counts; queue depths refresh every
  second (a DLQ holding messages turns red).
- The log pane streams live — press `/` and type to filter it as it flows.
- `tab` onto the events strip, arrow to one, **Enter replays it** — the
  outcome appears in the footer.

### 4.5 Look inside your data — `pulse tables` / `pulse peek`

No aws-cli needed to answer "what's in there right now?":

```bash
pulse tables                   # every table with item counts
pulse tables orders            # the items themselves, decoded for humans
```

```
orders — 2 item(s) shown
  e9b4e51a-…  createdAt="…" · qty="2" · sku="A1" · status="processed"
  parked-1    processedAt="…" · status="processed"
```

```bash
pulse tables orders --delete e9b4e51a-…   # remove one item by key (--sk for sort-key tables)
```

```bash
pulse peek order-events        # a queue's waiting messages — WITHOUT consuming them
```

```
order-events — 1 message(s), oldest first (peeking doesn't consume)
  473b4539  visible  {"id":"parked-1"}
```

Each message shows its state: `visible`, `hidden 4s` (delayed or mid-retry
backoff), or `retried ×2`.

### 4.6 Something's off? — `pulse doctor`

The "why isn't this working?" command — every environment assumption
checked, with the exact fix when one fails:

```bash
pulse doctor
```

```
⚡ pulse doctor — checking your setup

  ✓ pulse.yaml valid — 3 function(s), 3 trigger(s), 3 resource(s)
  ✓ Python 3.12.9 (.venv/bin/python)
  ✓ .venv present (pulse finds it automatically)
  ✓ engine stopped · port 3000 is free
  ✓ project state (.pulse/) healthy

✓ everything looks good — `pulse start` away
```

Warnings (✱) don't block you — a Node/Python version outside the certified
range still runs, with a note. Real problems (✗) come with their fix and a
non-zero exit for CI.

---

## 5. Everyday things

### 5.1 Seeing what exists — `pulse list` / `pulse validate`

```bash
pulse list
```

Shows the project at a glance: engine status (`● running` / `○ stopped`),
every function, every trigger, queues with live depths, tables with item
counts. `pulse validate` checks `pulse.yaml` and reports **all** problems at
once, with "did you mean…?" suggestions.

### 5.2 Stopping, restarting, persistence

```bash
pulse stop      # or Ctrl+C in the start terminal
```

```
✓ stopped — data, queues, and history are safe in .pulse/
```

Everything survives a restart: table data, queued jobs (delivered on next
start), logs, history. Delete the project's `.pulse/` folder for a clean
slate.

### 5.3 Environment variables — none required

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

## 6. pulse.yaml reference

The complete demo config, annotated:

```yaml
project: shop                    # name (lowercase, digits, hyphens)
region: us-east-1                # default region (optional)
api:
  port: 3000                     # optional; default 3000

functions:                       # ── your code ──
  createOrder:
    runtime: python3.12          # nodejs18/20/22.x or python3.10–3.13
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

## 7. Command cheat sheet

| Command | Does |
|---|---|
| `pulse tour` | Hands-on 5-minute walkthrough of the whole loop |
| `pulse init` | New project — no arguments asks three quick questions |
| `pulse init <name> [-t tpl] [--lang node\|python]` | Same, fully scripted (CI-safe) |
| `pulse import aws [fn]` | Build a project from a deployed Lambda (read-only) — §3.13 |
| `pulse aws profiles` / `pulse aws whoami` | Which AWS accounts pulse can see / which one it would read |
| `pulse start [--port N]` / `pulse stop` | Local cloud on / off |
| `pulse add function\|route\|queue\|table …` | Scaffold pieces, applied live (bare = wizard) |
| `pulse remove …` | The inverse — unwire pieces; code and data stay |
| `pulse invoke <fn> [-d json \| -e file]` | Run a function synchronously |
| `pulse send <queue> <body> [--delay N]` | Queue a job |
| `pulse logs <fn> [-f] [--grep x] [--request id]` | Logs: tail, search, one request's story |
| `pulse events` / `pulse events replay <id>` | Trigger history (`history` works too) / re-run an event |
| `pulse monitor` | Live dashboard: logs, queues, Enter-to-replay |
| `pulse tables [name]` / `pulse peek [queue]` | Look inside your data / waiting messages |
| `pulse doctor` | Check your setup, with fixes |
| `pulse list` / `pulse validate` | See everything / check config |
| `-C <dir>` on any command | Act on a project from anywhere |

Every command answers `--help` with copy-paste examples, and every command
that needs a name will **ask** on a terminal instead of erroring.
**Tab completion** (function, queue, event, and template names complete from
*your* project):

```bash
echo 'source <(pulse completion zsh)' >> ~/.zshrc   # bash/fish/powershell work too
```

---

## 8. When something goes wrong

First stop: `pulse doctor` — it checks the usual suspects and prints fixes.

| Symptom | Fix |
|---|---|
| `no pulse.yaml found` | `cd` into the project, or add `-C path/to/project` |
| `api port 3000 is unavailable` | Another engine is using it — `pulse start --port 3210` |
| Function logs "boto3/SDK not installed" | Run the install line from the project README (init was offline or `--no-install`) |
| `✗ pulse.yaml changed but has problems` | Read the printed list — it names each problem; old config still serving |
| `Task timed out after N seconds` | Handler exceeded its `timeout:` — raise it or fix the code |
| Jobs redelivered while still processing | Raise the queue's `visibilityTimeout` above the worker's `timeout` |
| Job never reaches the worker | Is the engine running? Delivery lives inside `pulse start`; `pulse peek <queue>` shows what's waiting |
| A function keeps failing on one input | `pulse logs --request <id>` for the full story, fix, then `pulse events replay <id>` |
| AWS CLI errors mentioning "Query protocol" | Old CLI — install AWS CLI v2.13+ (`brew install awscli`) |
| Want a clean slate | `pulse stop`, delete the project's `.pulse/` folder |

---

## 9. What pulse doesn't do yet

- **Next:** two-way sync with a real AWS account (import exists — §3.13 —
  export doesn't yet), project sharing, Windows support.
- **Backlog:** SNS, S3 + bucket events, DynamoDB streams/indexes/transactions,
  EventBridge, Step Functions, Go/Java/.NET runtimes, IAM enforcement.

Anything unsupported fails with a clear message saying so — if pulse did it
silently wrong instead, that's a bug: report it.
