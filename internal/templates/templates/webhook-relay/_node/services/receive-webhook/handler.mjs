// POST /webhooks — the webhook golden rule: ack fast, process later.
//
// Stripe/GitHub/etc. expect a quick 2xx; anything slow or flaky belongs in
// the queue, where retries are free.
import { randomUUID } from "node:crypto";
import { SQSClient, GetQueueUrlCommand, SendMessageCommand } from "@aws-sdk/client-sqs";

const sqs = new SQSClient({});
const { QueueUrl } = await sqs.send(new GetQueueUrlCommand({ QueueName: process.env.QUEUE_NAME }));

export const handler = async (event) => {
  const payload = JSON.parse(event.body ?? "{}");
  const delivery = { id: randomUUID(), ...payload };

  await sqs.send(new SendMessageCommand({ QueueUrl, MessageBody: JSON.stringify(delivery) }));

  console.log(`webhook ${delivery.id} queued (${payload.type ?? "unknown"})`);
  return { statusCode: 202, body: JSON.stringify({ received: delivery.id }) };
};
