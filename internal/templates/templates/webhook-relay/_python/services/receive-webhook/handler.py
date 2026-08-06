"""POST /webhooks — the webhook golden rule: ack fast, process later.

Stripe/GitHub/etc. expect a quick 2xx; anything slow or flaky belongs in
the queue, where retries are free.
"""

import json
import os
import uuid

import boto3

sqs = boto3.client("sqs")
queue_url = sqs.get_queue_url(QueueName=os.environ["QUEUE_NAME"])["QueueUrl"]


def handler(event, context):
    payload = json.loads(event.get("body") or "{}")
    delivery = {"id": str(uuid.uuid4()), **payload}

    sqs.send_message(QueueUrl=queue_url, MessageBody=json.dumps(delivery))

    print(f"webhook {delivery['id']} queued ({payload.get('type', 'unknown')})")
    return {"statusCode": 202, "body": json.dumps({"received": delivery["id"]})}
