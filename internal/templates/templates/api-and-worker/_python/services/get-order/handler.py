"""GET /orders/{id} — fetch one order from the table."""

import json
import os

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    order_id = event["pathParameters"]["id"]  # the {id} from the URL
    item = table.get_item(Key={"id": order_id}).get("Item")
    if not item:
        return {"statusCode": 404, "body": json.dumps({"error": f"order {order_id} not found"})}
    # default=str: DynamoDB numbers come back as Decimal
    return {"statusCode": 200, "body": json.dumps(item, default=str)}
