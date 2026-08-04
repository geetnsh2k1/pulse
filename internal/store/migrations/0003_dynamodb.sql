-- Phase 4: local DynamoDB. Table definitions (declared in pulse.yaml or
-- created at runtime) plus items keyed by their encoded primary key.

CREATE TABLE ddb_tables (
    name    TEXT PRIMARY KEY,
    pk_name TEXT NOT NULL,
    pk_type TEXT NOT NULL,          -- S | N | B
    sk_name TEXT,
    sk_type TEXT
);

CREATE TABLE ddb_items (
    tbl  TEXT NOT NULL,
    pk   TEXT NOT NULL,             -- type-prefixed encoded partition key
    sk   TEXT NOT NULL DEFAULT '',  -- encoded sort key ('' when table has none)
    item TEXT NOT NULL,             -- full AttributeValue-map JSON
    PRIMARY KEY (tbl, pk, sk)
);
