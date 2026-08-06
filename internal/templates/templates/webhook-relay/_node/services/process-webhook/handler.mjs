// webhooks queue — handle each delivery. Throwing = "retry me"; after 3
// attempts the message moves to webhooks-dlq.
export const handler = async (event) => {
  for (const record of event.Records) {
    const hook = JSON.parse(record.body);
    const attempt = Number(record.attributes.ApproximateReceiveCount);

    if (hook.fail) { // demo: {"fail": true} exercises retries + DLQ
      throw new Error(`webhook ${hook.id} failed on purpose (attempt ${attempt})`);
    }

    // Real work goes here: verify signature, update your systems, call APIs.
    console.log(`processed webhook ${hook.id} · type ${hook.type ?? "unknown"} (attempt ${attempt})`);
  }
};
