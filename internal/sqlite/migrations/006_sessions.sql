CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT PRIMARY KEY,
    adapter_id TEXT NOT NULL,
    adapter_key TEXT NOT NULL,
    label TEXT NOT NULL,
    location TEXT NOT NULL,
    status TEXT NOT NULL,
    capabilities_json TEXT NOT NULL,
    revision INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(adapter_id, adapter_key)
);

CREATE INDEX IF NOT EXISTS sessions_updated_order_idx ON sessions(updated_at DESC, session_id ASC);

CREATE TABLE IF NOT EXISTS session_events (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    sequence INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    UNIQUE(session_id, sequence)
);

CREATE INDEX IF NOT EXISTS session_events_session_sequence_idx ON session_events(session_id, sequence);

CREATE TABLE IF NOT EXISTS session_event_operation_receipts (
    adapter_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    event_id TEXT NOT NULL REFERENCES session_events(event_id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(adapter_id, operation_id)
);
