// POST /todos/{id}/done — mark one todo done (404 if it doesn't exist).
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, UpdateCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async (event) => {
  const id = event.pathParameters.id;
  try {
    const { Attributes } = await db.send(new UpdateCommand({
      TableName: process.env.TABLE_NAME,
      Key: { id },
      UpdateExpression: "SET done = :d",
      ExpressionAttributeValues: { ":d": true },
      ConditionExpression: "attribute_exists(id)", // 404 instead of upsert
      ReturnValues: "ALL_NEW",
    }));
    return { statusCode: 200, body: JSON.stringify(Attributes) };
  } catch (err) {
    if (err.name === "ConditionalCheckFailedException") {
      return { statusCode: 404, body: JSON.stringify({ error: `todo ${id} not found` }) };
    }
    throw err;
  }
};
