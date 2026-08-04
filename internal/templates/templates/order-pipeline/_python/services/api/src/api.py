"""The API function for the order pipeline — one Lambda handling both routes.

    POST /orders        create an order -> save it + enqueue a background job
    GET  /orders/{id}   fetch the real, current order

Uses plain boto3: pulse points it at the local mocks via AWS_ENDPOINT_URL,
so this code runs unmodified in real AWS. boto3 lives in the project .venv
(created automatically by `pulse init`; pulse finds it on its own) — the
function degrades gracefully if it's missing.
"""

import json
import os
import uuid
from datetime import datetime, timezone

_send_job = None
_table = None
try:
    import boto3

    _sqs = boto3.client("sqs")
    _table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])
    _queue_url = None

    def _send_job(job):  # noqa: F811 — deliberate rebind when boto3 exists
        global _queue_url
        if _queue_url is None:
            _queue_url = _sqs.get_queue_url(QueueName=os.environ["QUEUE_NAME"])["QueueUrl"]
        _sqs.send_message(QueueUrl=_queue_url, MessageBody=json.dumps(job))
except ImportError:
    print("(boto3 not installed — `pip install boto3` in the project venv to enable queue + database)")


def _json_default(o):
    # DynamoDB numbers come back as Decimal; render 2 as 2, not "2".
    try:
        return int(o) if o == int(o) else float(o)
    except (TypeError, ValueError):
        return str(o)


def _respond(status, body):
    return {
        "statusCode": status,
        "headers": {"content-type": "application/json"},
        "body": json.dumps(body, default=_json_default),
    }


def handler(event, context):
    method = ((event.get("requestContext") or {}).get("http") or {}).get("method", "GET")
    order_id = (event.get("pathParameters") or {}).get("id")

    if method == "POST":
        order = json.loads(event.get("body") or "{}")
        if not order.get("sku"):
            return _respond(422, {"error": "sku is required"})
        record = {
            "id": str(uuid.uuid4()),
            **order,
            "status": "pending",
            "createdAt": datetime.now(timezone.utc).isoformat(),
        }
        if _table is not None:
            _table.put_item(Item=record)
        if _send_job:
            _send_job({"id": record["id"], "sku": record["sku"], "fail": order.get("fail", False)})
            print(f"created order {record['id']} (sku {record['sku']}) — saved + job enqueued")
        else:
            print(f"created order {record['id']} (sku {record['sku']}) — boto3 missing, nothing persisted")
        return _respond(201, record)

    if method == "GET" and order_id:
        if _table is None:
            return _respond(200, {"id": order_id, "note": "install boto3 to enable persistence"})
        item = _table.get_item(Key={"id": order_id}).get("Item")
        if not item:
            return _respond(404, {"error": f"order {order_id} not found"})
        return _respond(200, item)

    return _respond(404, {"error": f"no route for {method} {event.get('rawPath', '')}"})
