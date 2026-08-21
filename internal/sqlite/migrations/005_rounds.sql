CREATE TABLE rounds (
    round_id TEXT PRIMARY KEY,
    producer_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    root_message_id TEXT NOT NULL UNIQUE REFERENCES messages(message_id),
    sender_address TEXT NOT NULL,
    recipient_address TEXT NOT NULL,
    sender_generation INTEGER NOT NULL,
    recipient_generation INTEGER NOT NULL,
    correlation_id TEXT NOT NULL,
    status TEXT NOT NULL,
    reply_message_id TEXT REFERENCES messages(message_id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    terminal_at TEXT,
    terminal_reason TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1,
    CHECK (status IN ('pending','replied','expired','cancelled','failed')),
    CHECK ((status = 'pending' AND reply_message_id IS NULL AND terminal_at IS NULL AND terminal_reason = '') OR
           (status = 'replied' AND reply_message_id IS NOT NULL AND terminal_at IS NOT NULL) OR
           (status IN ('expired','cancelled','failed') AND terminal_at IS NOT NULL)),
    UNIQUE (producer_id, operation_id)
);

CREATE INDEX rounds_status_expiry_idx ON rounds(status, expires_at);
CREATE INDEX rounds_root_message_idx ON rounds(root_message_id);

CREATE TRIGGER rounds_terminal_immutable
BEFORE UPDATE ON rounds
WHEN OLD.status IN ('replied','expired','cancelled','failed')
BEGIN SELECT RAISE(ABORT, 'terminal rounds are immutable'); END;
