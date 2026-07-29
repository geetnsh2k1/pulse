// GET /hello — the smallest possible pulse function.
export const handler = async (event) => {
  const name = event?.queryStringParameters?.name ?? "world";
  console.log(`saying hello to ${name}`);
  return {
    statusCode: 200,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ message: `hello, ${name}` }),
  };
};
