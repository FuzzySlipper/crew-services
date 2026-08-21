CREATE TABLE IF NOT EXISTS messages (
    message_id TEXT PRIMARY KEY,
    producer_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    sender_address TEXT NOT NULL,
    recipient_address TEXT NOT NULL,
    body TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    reply_to_message_id TEXT NOT NULL,
    activation_policy TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    sender_generation INTEGER NOT NULL,
    recipient_generation INTEGER NOT NULL,
    UNIQUE (producer_id, operation_id),
    CHECK (activation_policy IN ('wake_when_idle', 'never_wake'))
);

CREATE TABLE IF NOT EXISTS deliveries (
    accepted_sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id TEXT NOT NULL UNIQUE,
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(message_id),
    recipient_address TEXT NOT NULL,
    recipient_generation INTEGER NOT NULL,
    state TEXT NOT NULL,
    terminal_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    terminal_at TEXT,
    CHECK (state IN ('queued', 'failed', 'expired', 'cancelled')),
    CHECK ((state = 'queued' AND terminal_reason = '' AND terminal_at IS NULL) OR
           (state <> 'queued' AND terminal_reason <> '' AND terminal_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS deliveries_recipient_generation_order_idx
    ON deliveries(recipient_address, recipient_generation, accepted_sequence);
CREATE INDEX IF NOT EXISTS messages_created_order_idx ON messages(created_at, message_id);

CREATE TRIGGER IF NOT EXISTS messages_immutable_update
BEFORE UPDATE ON messages
BEGIN
    SELECT RAISE(ABORT, 'messages are immutable');
END;

CREATE TRIGGER IF NOT EXISTS deliveries_terminal_immutable
BEFORE UPDATE ON deliveries
WHEN OLD.state IN ('failed', 'expired', 'cancelled')
BEGIN
    SELECT RAISE(ABORT, 'terminal deliveries are immutable');
END;
