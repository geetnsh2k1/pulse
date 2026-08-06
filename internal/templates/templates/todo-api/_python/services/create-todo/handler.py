"""POST /todos — add a todo."""

import json
import os
import uuid

import boto3

table = boto3.resource("dynamodb").Table(os.environ["TABLE_NAME"])


def handler(event, context):
    body = json.loads(event.get("body") or "{}")
    if not body.get("text"):
        return {"statusCode": 422, "body": json.dumps({"error": "text is required"})}

    todo = {"id": str(uuid.uuid4()), "text": body["text"], "done": False}
    table.put_item(Item=todo)

    print(f"created todo {todo['id']}: {todo['text']}")
    return {"statusCode": 201, "body": json.dumps(todo)}
