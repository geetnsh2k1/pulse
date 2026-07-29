// pulse Node.js bootstrap — implements the AWS Lambda runtime interface loop.
// Spawned by the engine with _HANDLER, LAMBDA_TASK_ROOT, AWS_LAMBDA_RUNTIME_API.
import { pathToFileURL } from "node:url";
import { join } from "node:path";
import { access } from "node:fs/promises";

const base = `http://${process.env.AWS_LAMBDA_RUNTIME_API}/2018-06-01/runtime`;
const workerId = process.env.PULSE_WORKER_ID ?? "w0";
const headers = { "X-Pulse-Worker-Id": workerId };

async function post(url, body) {
  try {
    await fetch(url, { method: "POST", headers, body });
  } catch {
    // Engine is gone; the poll loop will exit on its next round.
  }
}

function errorBody(err) {
  const e = err instanceof Error ? err : new Error(String(err));
  return JSON.stringify({
    errorMessage: e.message,
    errorType: e.name || "Error",
    stackTrace: (e.stack || "").split("\n"),
  });
}

async function loadHandler() {
  const spec = process.env._HANDLER ?? "";
  const dot = spec.lastIndexOf(".");
  if (dot <= 0) throw new Error(`invalid handler "${spec}" — expected file.export`);
  const file = spec.slice(0, dot);
  const exportName = spec.slice(dot + 1);
  const root = process.env.LAMBDA_TASK_ROOT ?? process.cwd();

  let resolved = null;
  for (const ext of [".mjs", ".js", ".cjs"]) {
    const candidate = join(root, file + ext);
    try {
      await access(candidate);
      resolved = candidate;
      break;
    } catch {}
  }
  if (!resolved) throw new Error(`handler file not found: ${file}.{mjs,js,cjs} in ${root}`);

  const mod = await import(pathToFileURL(resolved).href);
  const fn = mod[exportName] ?? mod.default?.[exportName];
  if (typeof fn !== "function") {
    throw new Error(`handler export "${exportName}" is not a function in ${resolved}`);
  }
  return fn;
}

function run(handler, event, context) {
  if (handler.length >= 3) {
    return new Promise((resolve, reject) => {
      try {
        handler(event, context, (err, res) => (err ? reject(err) : resolve(res)));
      } catch (err) {
        reject(err);
      }
    });
  }
  return Promise.resolve(handler(event, context));
}

let handler;
try {
  handler = await loadHandler();
} catch (err) {
  console.error(String(err?.stack ?? err));
  await post(`${base}/init/error`, errorBody(err));
  process.exit(1);
}

while (true) {
  let res;
  try {
    res = await fetch(`${base}/invocation/next`, { headers });
  } catch {
    process.exit(0); // engine shut down
  }
  if (res.status === 410) process.exit(0); // retired (hot reload)
  if (!res.ok) {
    await new Promise((r) => setTimeout(r, 50));
    continue;
  }

  const requestId = res.headers.get("lambda-runtime-aws-request-id");
  const deadlineMs = Number(res.headers.get("lambda-runtime-deadline-ms") ?? 0);
  const event = await res.json().catch(() => null);
  const context = {
    awsRequestId: requestId,
    functionName: process.env.AWS_LAMBDA_FUNCTION_NAME,
    functionVersion: process.env.AWS_LAMBDA_FUNCTION_VERSION ?? "$LATEST",
    memoryLimitInMB: process.env.AWS_LAMBDA_FUNCTION_MEMORY_SIZE ?? "128",
    invokedFunctionArn: res.headers.get("lambda-runtime-invoked-function-arn"),
    logGroupName: `/aws/lambda/${process.env.AWS_LAMBDA_FUNCTION_NAME}`,
    logStreamName: workerId,
    callbackWaitsForEmptyEventLoop: true,
    getRemainingTimeInMillis: () => Math.max(0, deadlineMs - Date.now()),
  };

  try {
    const result = await run(handler, event, context);
    await post(`${base}/invocation/${requestId}/response`, JSON.stringify(result ?? null));
  } catch (err) {
    console.error(String(err?.stack ?? err));
    await post(`${base}/invocation/${requestId}/error`, errorBody(err));
  }
}
