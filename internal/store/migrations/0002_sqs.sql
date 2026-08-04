-- Phase 3: local SQS message storage. Messages persist across engine
-- restarts so queued background jobs survive a `pulse stop`.

CREATE TABLE sqs_messages (
    id                TEXT PRIMARY KEY,      -- MessageId
    queue             TEXT NOT NULL,
    body              TEXT NOT NULL,
    attributes        TEXT,                  -- message attributes, JSON
    sent_at           INTEGER NOT NULL,      -- unix ms
    visible_at        INTEGER NOT NULL,      -- unix ms: delay / visibility gate
    receive_count     INTEGER NOT NULL DEFAULT 0,
    first_received_at INTEGER,               -- unix ms
    receipt           TEXT                   -- current receipt handle while in flight
);
CREATE INDEX idx_sqs_queue_visible ON sqs_messages (queue, visible_at);
