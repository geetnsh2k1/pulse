"""POST /todos/{id}/done — mark one todo done (404 if it doesn't exist)."""

import json
import os

import boto3
from botocore.exceptions import ClientError

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    todo_id = event["pathParameters"]["id"]
    try:
        resp = table.update_item(
            Key={"id": todo_id},
            UpdateExpression="SET done = :d",
            ExpressionAttributeValues={":d": True},
            ConditionExpression="attribute_exists(id)",  # 404 instead of upsert
            ReturnValues="ALL_NEW",
        )
    except ClientError as e:
        if e.response["Error"]["Code"] == "ConditionalCheckFailedException":
            return {"statusCode": 404, "body": json.dumps({"error": f"todo {todo_id} not found"})}
        raise

    return {"statusCode": 200, "body": json.dumps(resp["Attributes"], default=str)}
