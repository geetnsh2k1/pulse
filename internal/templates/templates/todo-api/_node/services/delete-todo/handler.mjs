// DELETE /todos/{id} — remove one todo.
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, DeleteCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async (event) => {
  // Deleting something absent is fine — DELETE is idempotent in REST.
  await db.send(new DeleteCommand({
    TableName: process.env.TABLE_NAME,
    Key: { id: event.pathParameters.id },
  }));
  return { statusCode: 204, body: "" };
};
