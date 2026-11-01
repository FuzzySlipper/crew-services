package sqlite

import (
	"database/sql"
	"testing"
	"time"
)

// legacyMessage is a message row in the shape every schema before v10 shares.
// Upgrade tests seed old databases with plain SQL because the current store
// code reads and writes columns those schemas do not have yet.
type legacyMessage struct {
	producer, operation, fingerprint, messageID, deliveryID string
	sender, recipient, body, policy                         string
	created, expires                                        time.Time
}

func insertLegacyMessage(t *testing.T, db *sql.DB, m legacyMessage) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages (message_id, producer_id, operation_id, request_fingerprint, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation) VALUES (?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, 1, 1)`, m.messageID, m.producer, m.operation, m.fingerprint, m.sender, m.recipient, m.body, m.policy, timestamp(m.created), timestamp(m.expires)); err != nil {
		t.Fatalf("seed legacy message %s: %v", m.operation, err)
	}
	if _, err := db.Exec(`INSERT INTO deliveries (delivery_id, message_id, recipient_address, recipient_generation, state, created_at, expires_at) VALUES (?, ?, ?, 1, 'queued', ?, ?)`, m.deliveryID, m.messageID, m.recipient, timestamp(m.created), timestamp(m.expires)); err != nil {
		t.Fatalf("seed legacy delivery %s: %v", m.operation, err)
	}
}

func insertLegacyRound(t *testing.T, db *sql.DB, m legacyMessage, roundID string, expires time.Time) {
	t.Helper()
	insertLegacyMessage(t, db, m)
	if _, err := db.Exec(`INSERT INTO rounds (round_id, producer_id, operation_id, root_message_id, sender_address, recipient_address, sender_generation, recipient_generation, correlation_id, status, reply_message_id, created_at, expires_at, terminal_at, terminal_reason, revision) VALUES (?, ?, ?, ?, ?, ?, 1, 1, '', 'pending', NULL, ?, ?, NULL, '', 1)`, roundID, m.producer, m.operation, m.messageID, m.sender, m.recipient, timestamp(m.created), timestamp(expires)); err != nil {
		t.Fatalf("seed legacy round %s: %v", m.operation, err)
	}
}
