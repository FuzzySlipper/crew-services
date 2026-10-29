-- Activation policies become next_turn, next_step and no_wake. SQLite cannot
-- change a CHECK constraint in place, so the messages table is rebuilt and the
-- retired names on existing rows are mapped to their current equivalents.
-- Deliveries and rounds reference messages, so this migration runs with
-- foreign keys off and is checked with foreign_key_check before commit.

CREATE TABLE messages_v8 (
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
    CHECK (activation_policy IN ('next_turn', 'next_step', 'no_wake'))
);

INSERT INTO messages_v8
SELECT message_id, producer_id, operation_id, request_fingerprint, sender_address,
       recipient_address, body, correlation_id, reply_to_message_id,
       CASE activation_policy WHEN 'never_wake' THEN 'no_wake' ELSE 'next_turn' END,
       created_at, expires_at, sender_generation, recipient_generation
FROM messages;

DROP TABLE messages;
ALTER TABLE messages_v8 RENAME TO messages;

CREATE INDEX IF NOT EXISTS messages_created_order_idx ON messages(created_at, message_id);

CREATE TRIGGER IF NOT EXISTS messages_immutable_update
BEFORE UPDATE ON messages
BEGIN
    SELECT RAISE(ABORT, 'messages are immutable');
END;
