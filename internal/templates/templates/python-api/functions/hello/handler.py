"""GET /hello — the smallest possible pulse function."""

import json


def handler(event, context):
    name = (event.get("queryStringParameters") or {}).get("name", "world")
    print(f"saying hello to {name}")
    return {
        "statusCode": 200,
        "headers": {"content-type": "application/json"},
        "body": json.dumps({"message": f"hello, {name}"}),
    }
