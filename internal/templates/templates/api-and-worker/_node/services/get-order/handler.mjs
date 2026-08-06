// GET /orders/{id} — fetch one order from the table.
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, GetCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async (event) => {
  const id = event.pathParameters.id; // the {id} from the URL
  const { Item } = await db.send(
    new GetCommand({ TableName: process.env.TABLE_NAME, Key: { id } }),
  );
  if (!Item) {
    return { statusCode: 404, body: JSON.stringify({ error: `order ${id} not found` }) };
  }
  return { statusCode: 200, body: JSON.stringify(Item) };
};
