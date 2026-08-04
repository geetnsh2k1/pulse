// worker — processes background jobs from the order-events queue.
//
// Marks each processed order in DynamoDB, so GET /orders/{id} shows the
// real state a moment after creation. Jobs sent with {"fail": true} report
// a batch item failure on purpose so you can watch retries and the DLQ:
//
//   curl -X POST localhost:3000/orders -d '{"sku":"X","fail":true}'

let markProcessed = null;
try {
  const { DynamoDBClient } = await import("@aws-sdk/client-dynamodb");
  const { DynamoDBDocumentClient, UpdateCommand } = await import("@aws-sdk/lib-dynamodb");
  const doc = DynamoDBDocumentClient.from(new DynamoDBClient({}));
  markProcessed = (id) =>
    doc.send(
      new UpdateCommand({
        TableName: process.env.TABLE_NAME,
        Key: { id },
        UpdateExpression: "SET #s = :s, processedAt = :t",
        ExpressionAttributeNames: { "#s": "status" },
        ExpressionAttributeValues: { ":s": "processed", ":t": new Date().toISOString() },
      }),
    );
} catch {
  console.log("(AWS SDK not installed — orders will process but not be marked in the table)");
}

export const handle = async (event, context) => {
  const records = event.Records ?? [];
  const failures = [];
  console.log(`processing ${records.length} record(s) (request ${context.awsRequestId})`);
  for (const record of records) {
    const job = JSON.parse(record.body || "{}");
    const attempt = record.attributes?.ApproximateReceiveCount ?? "1";
    if (job.fail) {
      console.log(`job ${job.id ?? "?"} failing on purpose (attempt ${attempt})`);
      failures.push({ itemIdentifier: record.messageId });
      continue;
    }
    if (markProcessed && job.id) {
      await markProcessed(job.id);
    }
    console.log(`processed order ${job.id ?? "?"} (attempt ${attempt})`);
  }
  return { batchItemFailures: failures };
};
