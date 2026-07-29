"""pulse Python bootstrap — implements the AWS Lambda runtime interface loop.

Spawned by the engine (with -u for unbuffered output) and configured through
_HANDLER, LAMBDA_TASK_ROOT, AWS_LAMBDA_RUNTIME_API.
"""

import importlib
import json
import os
import sys
import time
import traceback
import urllib.error
import urllib.request

BASE = f"http://{os.environ['AWS_LAMBDA_RUNTIME_API']}/2018-06-01/runtime"
WORKER_ID = os.environ.get("PULSE_WORKER_ID", "w0")
HEADERS = {"X-Pulse-Worker-Id": WORKER_ID}

sys.path.insert(0, os.environ.get("LAMBDA_TASK_ROOT", "."))


def _post(url, body):
    req = urllib.request.Request(
        url,
        data=body.encode(),
        method="POST",
        headers={**HEADERS, "Content-Type": "application/json"},
    )
    try:
        urllib.request.urlopen(req, timeout=10).close()
    except Exception:  # noqa: BLE001 — engine gone; poll loop exits next round
        pass


def _error_body(exc):
    return json.dumps(
        {
            "errorMessage": str(exc),
            "errorType": type(exc).__name__,
            "stackTrace": traceback.format_exc().splitlines(),
        }
    )


class Context:
    def __init__(self, request_id, deadline_ms, arn):
        self.aws_request_id = request_id
        self.function_name = os.environ.get("AWS_LAMBDA_FUNCTION_NAME", "")
        self.function_version = os.environ.get("AWS_LAMBDA_FUNCTION_VERSION", "$LATEST")
        self.memory_limit_in_mb = os.environ.get("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "128")
        self.invoked_function_arn = arn
        self.log_group_name = f"/aws/lambda/{self.function_name}"
        self.log_stream_name = WORKER_ID
        self._deadline_ms = deadline_ms

    def get_remaining_time_in_millis(self):
        return max(0, self._deadline_ms - int(time.time() * 1000))


def _load_handler():
    spec = os.environ.get("_HANDLER", "")
    module_path, _, func_name = spec.rpartition(".")
    if not module_path:
        raise ValueError(f'invalid handler "{spec}" — expected module.function')
    return getattr(importlib.import_module(module_path), func_name)


try:
    _handler = _load_handler()
except BaseException as exc:  # noqa: BLE001 — report any import failure
    print(traceback.format_exc(), file=sys.stderr)
    _post(f"{BASE}/init/error", _error_body(exc))
    sys.exit(1)

while True:
    try:
        resp = urllib.request.urlopen(
            urllib.request.Request(f"{BASE}/invocation/next", headers=HEADERS)
        )
    except urllib.error.HTTPError as exc:
        if exc.code == 410:  # retired (hot reload)
            sys.exit(0)
        time.sleep(0.05)
        continue
    except Exception:  # noqa: BLE001 — engine shut down
        sys.exit(0)

    request_id = resp.headers.get("Lambda-Runtime-Aws-Request-Id", "")
    deadline_ms = int(resp.headers.get("Lambda-Runtime-Deadline-Ms", "0"))
    arn = resp.headers.get("Lambda-Runtime-Invoked-Function-Arn", "")
    raw = resp.read()
    resp.close()

    event = json.loads(raw) if raw else None
    ctx = Context(request_id, deadline_ms, arn)
    try:
        result = _handler(event, ctx)
        _post(f"{BASE}/invocation/{request_id}/response", json.dumps(result, default=str))
    except Exception as exc:  # noqa: BLE001 — any handler error goes to the engine
        print(traceback.format_exc(), file=sys.stderr)
        _post(f"{BASE}/invocation/{request_id}/error", _error_body(exc))
