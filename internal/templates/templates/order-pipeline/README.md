# pulse starter: order-pipeline

The golden workflow: a CRUD API with background jobs, entirely offline.

```
POST /orders ──▶ api (Node) ──▶ DynamoDB put (phase 4)
                     └────────▶ SQS send (phase 3) ──▶ worker (Python)
GET /orders/{id} ─▶ api ─────▶ DynamoDB get (phase 4)         └▶ marks order processed
```

What works at each phase:

| Phase | Try it |
|---|---|
| 1 — run Lambda | `pulse invoke worker --event events/sqs-message.json` · `pulse invoke api --event events/create-order.json` · `pulse logs worker` · edit a handler while `pulse start` runs → hot reload |
| 2 — build an API | `curl -X POST localhost:3000/orders -d '{"sku":"A1","qty":2}'` |
| 3 — background jobs | the API's SQS TODO goes live; worker consumes real batches; DLQ on repeated failure |
| 4 — persist data | DynamoDB TODOs go live; `GET /orders/{id}` returns real state |
| 5 — inspect | event history, replay, log search |

The handlers carry `TODO(phase N)` markers where the AWS SDK calls switch on —
pulse points the vanilla SDK at its local mocks via `AWS_ENDPOINT_URL`, so the
code will run unmodified in real AWS.
