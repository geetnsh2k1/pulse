"""worker — processes background jobs from the order-events queue.

Wired to the queue via the sqs trigger in pulse.yaml. From phase 3 the engine
delivers real SQS batches; until then, invoke it directly with the sample
event:

    pulse invoke worker --event events/sqs-message.json
"""

import json


def process(event, context):
    records = event.get("Records", [])
    print(f"processing {len(records)} record(s) (request {context.aws_request_id})")
    for record in records:
        body = record.get("body", "")
        try:
            job = json.loads(body)
        except (TypeError, ValueError):
            job = {"raw": body}
        print(f"working on order {job.get('id', '?')}")
        # TODO(phase 4): DynamoDB UpdateItem — mark the order "processed"
    return {"handled": len(records)}
