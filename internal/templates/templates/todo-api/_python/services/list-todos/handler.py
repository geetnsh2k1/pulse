"""GET /todos — every todo in the table."""

import json
import os

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    items = table.scan().get("Items", [])
    # default=str: DynamoDB numbers come back as Decimal
    return {"statusCode": 200, "body": json.dumps(items, default=str)}
