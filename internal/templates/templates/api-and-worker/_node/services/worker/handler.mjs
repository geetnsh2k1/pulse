// order-events worker — marks each queued order as processed.
//
// Throwing makes the queue retry the message, and dead-letter it after 3
// tries. See it happen: POST an order with {"fail": true}.
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, UpdateCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async (event) => {
  for (const record of event.Records) {
    const job = JSON.parse(record.body);

    if (job.fail) {
      const attempt = record.attributes.ApproximateReceiveCount;
      throw new Error(`order ${job.id} failed on purpose (attempt ${attempt})`);
    }

    await db.send(new UpdateCommand({
      TableName: process.env.TABLE_NAME,
      Key: { id: job.id },
      UpdateExpression: "SET #s = :s, processedAt = :t",
      ExpressionAttributeNames: { "#s": "status" },
      ExpressionAttributeValues: { ":s": "processed", ":t": new Date().toISOString() },
    }));
    console.log(`order ${job.id} processed`);
  }
};
