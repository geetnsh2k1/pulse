"""DELETE /todos/{id} — remove one todo."""

import os

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    # Deleting something absent is fine — DELETE is idempotent in REST.
    table.delete_item(Key={"id": event["pathParameters"]["id"]})
    return {"statusCode": 204, "body": ""}
