# pulse

Local AWS serverless development platform — run, trigger, and inspect Lambda-based
apps entirely on your laptop, with high-fidelity service mocks and instant feedback.
No Docker, no AWS account.

> Status: **pre-alpha**, built milestone by milestone. See [PLAN.md](PLAN.md) for the
> full MVP plan and architecture.

## Roadmap

One golden workflow — a CRUD app with background jobs, fully offline — built
exceptionally well before anything else. Each phase ships something
demonstrably useful.

| Phase | Story | Status |
|---|---|---|
| 0 | Foundations: CLI, config, store, engine skeleton, templates | ✅ |
| 1 | Run Lambda — execute a function locally (invoke, logs, hot reload) | ✅ |
| 2 | Build an API — a REST API works entirely offline | ✅ |
| 3 | Process background jobs — SQS + worker Lambda | ✅ |
| 4 | Persist data — DynamoDB | ✅ |
| 5 | Inspect everything — logs, events, replay | ⏳ next |
| 6 | Team ready — cloud sync, packaging, sharing | – |

Natural evolution after that: SNS, S3, EventBridge, Step Functions, more
runtimes, desktop app.

## Quickstart

```bash
make build                                  # → bin/pulse
bin/pulse init shop --template api-and-worker --lang python   # deps auto-install
cd shop
pulse start                                 # API + queues + tables + live apply
curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'  # → 201 "pending"
curl localhost:3000/orders/<id>             # → "processed", via queue + worker + table
pulse add function notifier                 # scaffold more — applies live
```

## Project config

A pulse project is a folder with a `pulse.yaml` describing functions, triggers, and
resources. Run `pulse init --list` to see starter templates. Engine state lives in
`.pulse/` (SQLite + files) inside the project — gitignore it.

## Development

```bash
make build   # build the CLI
make test    # go test -race ./...
make lint    # go vet + gofmt check
```

- Go module path is the placeholder `pulse` until the repo gets a remote home;
  it will be renamed in one pass at git setup time.
- Runtimes certified at MVP: Node.js 18/20/22, Python 3.9–3.12.
- macOS/Linux are first-class; Windows is kept compiling in CI and becomes fully
  supported at beta.
