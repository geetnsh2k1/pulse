"""POST /orders — save a new order and queue it for processing."""

import json
import os
import uuid
from datetime import datetime, timezone

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])
sqs = boto3.client("sqs")
queue_url = sqs.get_queue_url(QueueName=os.environ["QUEUE_NAME"])["QueueUrl"]


def handler(event, context):
    order = json.loads(event.get("body") or "{}")
    if not order.get("sku"):
        return {"statusCode": 422, "body": json.dumps({"error": "sku is required"})}

    order["id"] = str(uuid.uuid4())
    order["status"] = "pending"
    order["createdAt"] = datetime.now(timezone.utc).isoformat()

    table.put_item(Item=order)
    sqs.send_message(
        QueueUrl=queue_url,
        MessageBody=json.dumps({"id": order["id"], "fail": order.get("fail", False)}),
    )

    print(f"order {order['id']} saved and queued")
    return {"statusCode": 201, "body": json.dumps(order)}
