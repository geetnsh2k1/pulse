// The API function for the order pipeline — one Lambda handling both routes
// (a common pattern: branch on the proxy event's method and path).
//
//   POST /orders        create an order
//   GET  /orders/{id}   fetch an order
//
// Queueing (SQS) goes live in phase 3 and persistence (DynamoDB) in phase 4 —
// the marked TODOs flip on then. Until that, this function validates, logs,
// and responds, so every earlier phase has something real to run.
import { randomUUID } from "node:crypto";

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
    // TODO(phase 3): SQS SendMessage { QueueName: env.QUEUE_NAME, body: { id } }
    // TODO(phase 4): DynamoDB PutItem { TableName: env.TABLE_NAME, Item: record }
    console.log(`created order ${record.id} (sku ${record.sku})`);
    return respond(201, record);
  }

  if (method === "GET" && id) {
    // TODO(phase 4): DynamoDB GetItem by id — until then, a placeholder echo
    console.log(`fetching order ${id}`);
    return respond(200, { id, status: "pending", note: "persistence arrives in phase 4" });
  }

  return respond(404, { error: `no route for ${method} ${event?.rawPath ?? ""}` });
};
