"""webhooks queue — handle each delivery. Raising = "retry me"; after 3
attempts the message moves to webhooks-dlq."""

import json


def handler(event, context):
    for record in event["Records"]:
        hook = json.loads(record["body"])
        attempt = int(record["attributes"]["ApproximateReceiveCount"])

        if hook.get("fail"):  # demo: {"fail": true} exercises retries + DLQ
            raise RuntimeError(f"webhook {hook['id']} failed on purpose (attempt {attempt})")

        # Real work goes here: verify signature, update your systems, call APIs.
        print(f"processed webhook {hook['id']} · type {hook.get('type', 'unknown')} (attempt {attempt})")
