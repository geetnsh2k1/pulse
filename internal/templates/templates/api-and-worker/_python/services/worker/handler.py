"""order-events worker — marks each queued order as processed.

Raising an exception makes the queue retry the message, and dead-letter it
after 3 tries. See it happen: POST an order with {"fail": true}.
"""

import json
import os
from datetime import datetime, timezone

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    for record in event["Records"]:
        job = json.loads(record["body"])

        if job.get("fail"):
            attempt = record["attributes"]["ApproximateReceiveCount"]
            raise RuntimeError(f"order {job['id']} failed on purpose (attempt {attempt})")

        table.update_item(
            Key={"id": job["id"]},
            UpdateExpression="SET #s = :s, processedAt = :t",
            ExpressionAttributeNames={"#s": "status"},
            ExpressionAttributeValues={
                ":s": "processed",
                ":t": datetime.now(timezone.utc).isoformat(),
            },
        )
        print(f"order {job['id']} processed")
