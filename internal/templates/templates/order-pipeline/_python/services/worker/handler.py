"""worker — processes background jobs from the order-events queue.

Marks each processed order in DynamoDB, so `GET /orders/{id}` shows the
real state a moment after creation. Jobs sent with {"fail": true} report a
batch item failure on purpose so you can watch retries and the dead-letter
queue:

    curl -X POST localhost:3000/orders -d '{"sku":"X","fail":true}'
"""

import json
import os
from datetime import datetime, timezone

_table = None
try:
    import boto3

    _table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])
except ImportError:
    print("(boto3 not installed — orders will process but not be marked in the table)")


def handle(event, context):
    records = event.get("Records", [])
    failures = []
    print(f"processing {len(records)} record(s) (request {context.aws_request_id})")
    for record in records:
        job = json.loads(record.get("body") or "{}")
        attempt = record.get("attributes", {}).get("ApproximateReceiveCount", "1")
        if job.get("fail"):
            print(f"job {job.get('id', '?')} failing on purpose (attempt {attempt})")
            failures.append({"itemIdentifier": record["messageId"]})
            continue
        if _table is not None and job.get("id"):
            _table.update_item(
                Key={"id": job["id"]},
                UpdateExpression="SET #s = :s, processedAt = :t",
                ExpressionAttributeNames={"#s": "status"},
                ExpressionAttributeValues={
                    ":s": "processed",
                    ":t": datetime.now(timezone.utc).isoformat(),
                },
            )
        print(f"processed order {job.get('id', '?')} (attempt {attempt})")
    return {"batchItemFailures": failures}
