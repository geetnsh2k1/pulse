// POST /todos — add a todo.
import { randomUUID } from "node:crypto";
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, PutCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async (event) => {
  const body = JSON.parse(event.body ?? "{}");
  if (!body.text) {
    return { statusCode: 422, body: JSON.stringify({ error: "text is required" }) };
  }

  const todo = { id: randomUUID(), text: body.text, done: false };
  await db.send(new PutCommand({ TableName: process.env.TABLE_NAME, Item: todo }));

  console.log(`created todo ${todo.id}: ${todo.text}`);
  return { statusCode: 201, body: JSON.stringify(todo) };
};
