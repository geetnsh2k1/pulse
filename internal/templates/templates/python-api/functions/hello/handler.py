"""hello — your first pulse function. Edit this file and save: pulse
hot-reloads it, no restart needed.

`event` is a standard API Gateway request when it arrives via GET /hello
(see pulse.yaml), or whatever JSON you pass to `pulse invoke`. Useful keys:

    event["queryStringParameters"]        -> ?name=you
    event["requestContext"]["http"]       -> method, path
    event["body"]                         -> request body (POST/PUT)

Whatever you return becomes the HTTP response: statusCode, headers, and a
string body. Everything you print() shows up in `pulse logs hello` and the
engine console.
"""

import json


def handler(event, context):
    name = (event.get("queryStringParameters") or {}).get("name", "world")
    print(f"saying hello to {name}")
    return {
        "statusCode": 200,
        "headers": {"content-type": "application/json"},
        "body": json.dumps({"message": f"hello, {name}"}),
    }
