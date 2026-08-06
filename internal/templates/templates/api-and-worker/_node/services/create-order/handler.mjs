// POST /orders — save a new order and queue it for processing.
import { randomUUID } from "node:crypto";
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, PutCommand } from "@aws-sdk/lib-dynamodb";
import { SQSClient, GetQueueUrlCommand, SendMessageCommand } from "@aws-sdk/client-sqs";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));
const sqs = new SQSClient({});
const { QueueUrl } = await sqs.send(new GetQueueUrlCommand({ QueueName: process.env.QUEUE_NAME }));

export const handler = async (event) => {
  const order = JSON.parse(event.body ?? "{}");
  if (!order.sku) {
    return { statusCode: 422, body: JSON.stringify({ error: "sku is required" }) };
  }

  order.id = randomUUID();
  order.status = "pending";
  order.createdAt = new Date().toISOString();

  await db.send(new PutCommand({ TableName: process.env.TABLE_NAME, Item: order }));
  await sqs.send(new SendMessageCommand({
    QueueUrl,
    MessageBody: JSON.stringify({ id: order.id, fail: order.fail ?? false }),
  }));

  console.log(`order ${order.id} saved and queued`);
  return { statusCode: 201, body: JSON.stringify(order) };
};
