// hello — your first pulse function. Edit this file and save: pulse
// hot-reloads it, no restart needed.
//
// `event` is a standard API Gateway request when it arrives via
// GET /hello (see pulse.yaml), or whatever JSON you pass to `pulse invoke`.
// Useful fields:
//   event.queryStringParameters   → ?name=you
//   event.requestContext.http     → method, path
//   event.body                    → request body (POST/PUT)
//
// Whatever you return becomes the HTTP response: set statusCode, headers,
// and a string body. Everything you console.log shows up in `pulse logs
// hello` and the engine console.
export const handler = async (event) => {
  const name = event?.queryStringParameters?.name ?? "world";
  console.log(`saying hello to ${name}`);
  return {
    statusCode: 200,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ message: `hello, ${name}` }),
  };
};
