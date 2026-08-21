-- Rebuild the small v3 ledger to extend its state machine without losing rows.
DROP TRIGGER IF EXISTS deliveries_terminal_immutable;
DROP INDEX IF EXISTS deliveries_recipient_generation_order_idx;

CREATE TABLE deliveries_next (
    accepted_sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id TEXT NOT NULL UNIQUE,
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(message_id),
    recipient_address TEXT NOT NULL,
    recipient_generation INTEGER NOT NULL,
    state TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    claim_owner_adapter_id TEXT NOT NULL DEFAULT '',
    claim_owner_instance_id TEXT NOT NULL DEFAULT '',
    claim_token TEXT NOT NULL DEFAULT '',
    claim_expires_at TEXT,
    claimed_at TEXT,
    dispatch_action TEXT NOT NULL DEFAULT '',
    native_attempt_ref TEXT NOT NULL DEFAULT '',
    dispatching_at TEXT,
    terminal_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    terminal_at TEXT,
    CHECK (state IN ('queued','claimed','dispatching','delivered','failed','expired','cancelled','outcome_unknown')),
    CHECK ((state IN ('queued','claimed','dispatching') AND terminal_reason = '' AND terminal_at IS NULL) OR
           (state IN ('delivered','failed','expired','cancelled','outcome_unknown') AND terminal_at IS NOT NULL))
);
INSERT INTO deliveries_next (accepted_sequence, delivery_id, message_id, recipient_address, recipient_generation, state, terminal_reason, created_at, expires_at, terminal_at)
 SELECT accepted_sequence, delivery_id, message_id, recipient_address, recipient_generation, state, terminal_reason, created_at, expires_at, terminal_at FROM deliveries;
DROP TABLE deliveries;
ALTER TABLE deliveries_next RENAME TO deliveries;
CREATE INDEX deliveries_recipient_generation_order_idx ON deliveries(recipient_address, recipient_generation, accepted_sequence);
CREATE TABLE adapter_operation_receipts (
    adapter_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(adapter_id, operation_id)
);
CREATE TRIGGER deliveries_terminal_immutable
BEFORE UPDATE ON deliveries
WHEN OLD.state IN ('delivered','failed','expired','cancelled','outcome_unknown')
BEGIN SELECT RAISE(ABORT, 'terminal deliveries are immutable'); END;
