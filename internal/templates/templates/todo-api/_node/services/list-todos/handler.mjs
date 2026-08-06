// GET /todos — every todo in the table.
import { DynamoDBClient } from "@aws-sdk/client-dynamodb";
import { DynamoDBDocumentClient, ScanCommand } from "@aws-sdk/lib-dynamodb";

const db = DynamoDBDocumentClient.from(new DynamoDBClient({}));

export const handler = async () => {
  const { Items = [] } = await db.send(new ScanCommand({ TableName: process.env.TABLE_NAME }));
  return { statusCode: 200, body: JSON.stringify(Items) };
};
