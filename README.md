<p align="center">
  <br>
  <code>&nbsp;─╮ ╭─╮ ╭──&nbsp;</code><br>
  <code>&nbsp;&nbsp;╰─╯ ╰─╯&nbsp;&nbsp;&nbsp;</code><br>
  <h1 align="center">pulse</h1>
</p>

<p align="center">
  <b>The dev server AWS Lambda never had.</b><br>
  Run your whole app — API, queues, workers, DynamoDB — natively on your laptop in milliseconds.<br>
  No Docker. No AWS account. No deploys.
</p>

<p align="center">
  <a href="https://github.com/geetnsh2k1/pulse/actions/workflows/ci.yml"><img src="https://github.com/geetnsh2k1/pulse/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <a href="https://github.com/geetnsh2k1/pulse/releases/latest"><img src="https://img.shields.io/github/v/release/geetnsh2k1/pulse" alt="release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license"></a>
</p>

<p align="center">
  <img src="docs/assets/demo.gif" alt="pulse demo — init, start, a live request through the queue to the worker, and the event history" width="820">
</p>

---

Every stack has a dev server — Rails has `rails server`, frontend has Vite.
Serverless never got one: you either deploy-and-pray, or boot a
multi-gigabyte cloud emulator in Docker. **pulse is that missing dev
server.** Your code uses the vanilla AWS SDK and runs unchanged in
production; pulse gives it a local cloud with a sub-second inner loop.

```
⚡ pulse 0.1.0 — shop (us-east-1)
  functions  createOrder · getOrder · worker
  api        http://localhost:3000
  routes     POST /orders → createOrder
             GET /orders/{id} → getOrder
  try        curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'
  aws        http://127.0.0.1:62552 (sqs, dynamodb)
ready in 99ms — code & pulse.yaml changes apply live · Ctrl+C to stop
```

## Install

**Homebrew** (macOS / Linux):

```bash
brew install --cask geetnsh2k1/pulse/pulse
```

**Script** (macOS / Linux):

```bash
curl -fsSL https://raw.githubusercontent.com/geetnsh2k1/pulse/master/scripts/install.sh | sh
```

**Go:**

```bash
go install github.com/geetnsh2k1/pulse/cmd/pulse@latest
```

Or grab a binary from the [releases page](https://github.com/geetnsh2k1/pulse/releases)
(Windows builds are there too — Windows support is beta).

> **Update check:** at most once a day, pulse fetches a static
> `version.json` from getpulse.run to tell you when a newer release
> exists — a plain GET, no payload, no identifiers, silent offline.
> Opt out any time with `PULSE_NO_UPDATE_CHECK=1`.

**Secrets:** `pulse init` scaffolds a gitignored `.env` (real values) and a
committed `.env.example` (the names). Every function receives `.env`; a
function's own `env:` in pulse.yaml overrides it. Reserved Lambda variables
are refused with a clear message, exactly as AWS refuses them.

**Supported runtimes:** Python 3.10+ and Node 18+, on macOS and Linux
(Windows via WSL2). Every push runs the full golden path — init → start →
request → queue → worker → replay → deploy-ready — across that matrix in
CI (`scripts/e2e.sh`), so the support claim is tested, not asserted.

## Two minutes to a running app

New to pulse? `pulse tour` teaches the whole loop hands-on in five minutes.
Or go manual:

```bash
pulse init shop --template api-and-worker --lang python
cd shop
pulse start
```

New terminal — paste the banner's `try` line:

```bash
curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'
# → 201 {"id":"…","status":"pending",…}

curl localhost:3000/orders/<that-id>
# → {"status":"processed",…}   ← a background worker updated it
```

That round trip was an API, a queue, a worker with retries, and a database —
all local, all real SDK calls. `init` installed the dependencies; nothing to
activate or configure.

## Already deployed? Import it

If your app is already in AWS, you don't start from a template:

```bash
pulse import aws                 # asks which profile, region and function
pulse import aws createOrder     # or name it
```

pulse reads the function's configuration, its API Gateway routes and SQS
triggers, the queues and tables it appears to use, and your real handler
code — then writes a project you can `pulse start`. It uses the same
credentials the `aws` CLI does, and prints the account before reading
anything.

**Read-only, always.** Import calls only `List*`, `Get*` and `Describe*` —
no mutating AWS API is reachable from the command, so it cannot affect
production. Environment *values* are not copied unless you pass
`--with-values`; every value lands in `.env` as `CHANGE_ME`. Anything AWS
has that pulse can't represent — layers, VPC config, secondary indexes,
S3/SNS/EventBridge triggers — is printed and written to `IMPORT-NOTES.md`
beside the project. Nothing is dropped silently.

It finishes the job, too: dependencies are installed the way `pulse init`
installs them, so the next step is `pulse start`.

```bash
pulse import aws createOrder --dry-run   # show the plan, write nothing
pulse import aws --policy                # the read-only IAM policy it needs
```

Details and limits in [the guide](docs/GUIDE.md#313-already-deployed--pulse-import-aws).

## What you get

- **A sub-second inner loop.** Engine ready in ~100ms, warm invokes in
  ~17ms — [enforced by CI](internal/perf/perf_test.go), not just claimed.
  Edit a handler or `pulse.yaml` and it applies live; no restarts exist.
- **The async loop, actually local.** Queues deliver to workers with
  visibility timeouts, automatic retries, and dead-letter queues — the part
  `sam local` can't do at all, in one console instead of a deploy cycle.
- **Time travel.** Every trigger is recorded with its exact payload.
  `pulse events replay <id>` re-fires yesterday's crashing request against
  today's fix; `pulse logs --request <id>` tells one request's whole story.
- **A live dashboard.** `pulse monitor` — functions with ✓/✗ counts, live
  queue depths, streaming filtered logs, and Enter-to-replay history.
- **A CLI that teaches.** Run any command bare and it asks instead of
  erroring. Errors ship their fix — an `AccessDenied` names the exact IAM
  action and prints the policy to request. `pulse doctor` checks your setup.
  Tab completion knows *your* functions and queues.
- **Honesty by design.** pulse does one workflow completely — CRUD +
  background jobs. Everything outside the subset fails loudly with a
  message saying so, never silently wrong.

## Templates are a learning path

| Template | What it teaches |
|---|---|
| `hello` | One function behind `GET /hello` — the smallest start |
| `todo-api` | Real CRUD on one table |
| `webhook-relay` | Ack-fast webhooks with retries + a dead-letter queue |
| `api-and-worker` ★ | Everything together: API + queue + worker + table |

All templates come in Python and Node (`--lang`), use the plain AWS SDK, and
run unchanged in real AWS.

## How it compares

|  | pulse | sam local | LocalStack |
|---|---|---|---|
| Cold start to working | **~100 ms** | container per invoke | 10–30 s container |
| Code change | save → done | mostly re-invoke | redeploy / hot-reload config |
| Queue → worker → DLQ locally | **yes, out of the box** | no | yes, via deploy cycle |
| Requirements | one binary, no daemon | Docker | Docker (GB-scale image) |
| Persistence across restarts | free, default | n/a | paid tier |

Different tools for different jobs: LocalStack emulates ~100 AWS services
and tests your IaC; SAM deploys. pulse owns the **inner loop** — the five
hundred iterations before staging — and pairs with either at deploy time,
since your code is vanilla SDK throughout.

## Docs

- **[The guide](docs/GUIDE.md)** — step-by-step with checkpoints: build,
  inspect, reference, troubleshooting.
- `pulse tour` — the interactive 5-minute version.
- Every command answers `--help` with copy-paste examples.

## What pulse doesn't do yet

S3, SNS, EventBridge, Step Functions, DynamoDB indexes/transactions, Go/Java
runtimes — see the [roadmap](docs/GUIDE.md#9-what-pulse-doesnt-do-yet).
Anything unsupported fails with a clear message saying so. If pulse ever
does something *silently wrong* instead, that's a bug: please open an issue.

## License

[Apache-2.0](LICENSE) © Geetansh Garg
