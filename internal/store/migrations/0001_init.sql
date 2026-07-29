-- Core M0 schema. Timestamps are unix milliseconds (INTEGER) throughout.

-- One row per Lambda invocation, however it was triggered.
CREATE TABLE invocations (
    id           TEXT PRIMARY KEY,          -- ulid/uuid
    function     TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT 'manual',  -- manual|http|sqs|sns|s3|dynamodb-stream
    status       TEXT NOT NULL,             -- pending|running|success|error|timeout
    request_id   TEXT,                      -- AWS-style request id handed to the worker
    payload      BLOB,                      -- event JSON
    result       BLOB,                      -- response JSON (or null)
    error        TEXT,
    started_at   INTEGER NOT NULL,
    completed_at INTEGER,
    duration_ms  INTEGER
);
CREATE INDEX idx_invocations_function ON invocations (function, started_at DESC);

-- Recorded inbound events, kept for inspection and one-click replay.
CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    type            TEXT NOT NULL,          -- http|sqs|sns|s3|dynamodb-stream|manual
    source          TEXT,                   -- e.g. "POST /orders", topic/queue name
    target_function TEXT,
    payload         BLOB NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_events_created ON events (created_at DESC);

-- Captured stdout/stderr lines from workers, tagged per invocation.
CREATE TABLE logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    function   TEXT NOT NULL,
    request_id TEXT,
    stream     TEXT NOT NULL DEFAULT 'stdout',   -- stdout|stderr|system
    ts         INTEGER NOT NULL,
    line       TEXT NOT NULL
);
CREATE INDEX idx_logs_function_ts ON logs (function, ts);

-- Small engine state (schema-less odds and ends).
CREATE TABLE kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
