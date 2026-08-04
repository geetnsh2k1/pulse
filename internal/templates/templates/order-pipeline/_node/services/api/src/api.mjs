// The API function for the order pipeline — one Lambda handling both routes.
//
//   POST /orders        create an order → save it + enqueue a background job
//   GET  /orders/{id}   fetch the real, current order
//
// Uses the plain AWS SDK v3: pulse points it at the local mocks via
// AWS_ENDPOINT_URL, so this code runs unmodified in real AWS. Run
// `npm install` at the project root to enable it (the function degrades
// gracefully until then).
import { randomUUID } from "node:crypto";

let sendJob = null;
let table = null;
try {
  const { SQSClient, GetQueueUrlCommand, SendMessageCommand } = await import("@aws-sdk/client-sqs");
  const { DynamoDBClient } = await import("@aws-sdk/client-dynamodb");
  const { DynamoDBDocumentClient, PutCommand, GetCommand } = await import("@aws-sdk/lib-dynamodb");

  const sqs = new SQSClient({});
  const doc = DynamoDBDocumentClient.from(new DynamoDBClient({}));
  let queueUrlPromise = null;
  const queueUrl = () =>
    (queueUrlPromise ??= sqs
      .send(new GetQueueUrlCommand({ QueueName: process.env.QUEUE_NAME }))
      .then((r) => r.QueueUrl));

  sendJob = async (job) => {
    await sqs.send(
      new SendMessageCommand({ QueueUrl: await queueUrl(), MessageBody: JSON.stringify(job) }),
    );
  };
  table = {
    put: (item) => doc.send(new PutCommand({ TableName: process.env.TABLE_NAME, Item: item })),
    get: (id) =>
      doc.send(new GetCommand({ TableName: process.env.TABLE_NAME, Key: { id } })).then((r) => r.Item),
  };
} catch {
  console.log("(AWS SDK not installed — run `npm install` at the project root to enable queue + database)");
}

const respond = (statusCode, body) => ({
  statusCode,
  headers: { "content-type": "application/json" },
  body: JSON.stringify(body),
});

export const handler = async (event) => {
  const method = event?.requestContext?.http?.method ?? "GET";
  const id = event?.pathParameters?.id;

  if (method === "POST") {
    const order = JSON.parse(event.body ?? "{}");
    if (!order.sku) {
      return respond(422, { error: "sku is required" });
    }
    const record = {
      id: randomUUID(),
      ...order,
      status: "pending",
      createdAt: new Date().toISOString(),
    };
    if (table) {
      await table.put(record);
    }
    if (sendJob) {
      await sendJob({ id: record.id, sku: record.sku, fail: order.fail ?? false });
      console.log(`created order ${record.id} (sku ${record.sku}) — saved + job enqueued`);
    } else {
      console.log(`created order ${record.id} (sku ${record.sku}) — SDK missing, nothing persisted`);
    }
    return respond(201, record);
  }

  if (method === "GET" && id) {
    if (!table) {
      return respond(200, { id, note: "run `npm install` to enable persistence" });
    }
    const item = await table.get(id);
    if (!item) {
      return respond(404, { error: `order ${id} not found` });
    }
    return respond(200, item);
  }

  return respond(404, { error: `no route for ${method} ${event?.rawPath ?? ""}` });
};
