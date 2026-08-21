// Package sqlite provides the service's local SQLite implementation.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"crew-services/internal/store"
	_ "modernc.org/sqlite"
)

// Store owns a SQLite connection pool for one local service process.
type Store struct {
	db *sql.DB
}

// Open opens path and applies every known migration before returning a store.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Ready verifies that the local database remains usable.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite database: %w", err)
	}
	return nil
}

// Close releases database resources.
func (s *Store) Close() error {
	return s.db.Close()
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp: %w", err)
	}
	return parsed.UTC(), nil
}

// RegisterAdapter atomically fences a different instance while allowing a
// same-instance retry (including one after expiry) to reuse its token.
func (s *Store) RegisterAdapter(ctx context.Context, now time.Time, request store.RegisterAdapterRequest) (store.AdapterLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.AdapterLease{}, fmt.Errorf("begin adapter registration: %w", err)
	}
	defer tx.Rollback()

	var instanceID, token, expires string
	err = tx.QueryRowContext(ctx, `SELECT instance_id, lease_token, expires_at FROM adapter_leases WHERE adapter_id = ?`, request.AdapterID).Scan(&instanceID, &token, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.AdapterLease{}, fmt.Errorf("read adapter lease: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		token = request.LeaseToken
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_leases (adapter_id, instance_id, lease_token, expires_at, updated_at) VALUES (?, ?, ?, ?, ?)`, request.AdapterID, request.InstanceID, token, timestamp(request.ExpiresAt), timestamp(now)); err != nil {
			return store.AdapterLease{}, fmt.Errorf("insert adapter lease: %w", err)
		}
	} else if instanceID == request.InstanceID {
		if _, err := tx.ExecContext(ctx, `UPDATE adapter_leases SET expires_at = ?, updated_at = ? WHERE adapter_id = ?`, timestamp(request.ExpiresAt), timestamp(now), request.AdapterID); err != nil {
			return store.AdapterLease{}, fmt.Errorf("extend adapter registration: %w", err)
		}
	} else {
		currentExpiry, err := parseTimestamp(expires)
		if err != nil {
			return store.AdapterLease{}, err
		}
		if currentExpiry.After(now.UTC()) && request.PreviousLeaseToken != token {
			return store.AdapterLease{}, store.ErrLeaseFenced
		}
		token = request.LeaseToken
		if _, err := tx.ExecContext(ctx, `UPDATE adapter_leases SET instance_id = ?, lease_token = ?, expires_at = ?, updated_at = ? WHERE adapter_id = ?`, request.InstanceID, token, timestamp(request.ExpiresAt), timestamp(now), request.AdapterID); err != nil {
			return store.AdapterLease{}, fmt.Errorf("fence adapter lease: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return store.AdapterLease{}, fmt.Errorf("commit adapter registration: %w", err)
	}
	return store.AdapterLease{AdapterID: request.AdapterID, InstanceID: request.InstanceID, LeaseToken: token, ExpiresAt: request.ExpiresAt.UTC()}, nil
}

func (s *Store) RenewAdapter(ctx context.Context, now time.Time, request store.RenewAdapterRequest) (store.AdapterLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.AdapterLease{}, fmt.Errorf("begin adapter renewal: %w", err)
	}
	defer tx.Rollback()
	lease, err := currentLease(ctx, tx, now, request.AdapterID, request.LeaseToken)
	if err != nil {
		return store.AdapterLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE adapter_leases SET expires_at = ?, updated_at = ? WHERE adapter_id = ?`, timestamp(request.ExpiresAt), timestamp(now), request.AdapterID); err != nil {
		return store.AdapterLease{}, fmt.Errorf("renew adapter lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.AdapterLease{}, fmt.Errorf("commit adapter renewal: %w", err)
	}
	lease.ExpiresAt = request.ExpiresAt.UTC()
	return lease, nil
}

func currentLease(ctx context.Context, tx *sql.Tx, now time.Time, adapterID, token string) (store.AdapterLease, error) {
	var lease store.AdapterLease
	var expires string
	err := tx.QueryRowContext(ctx, `SELECT adapter_id, instance_id, lease_token, expires_at FROM adapter_leases WHERE adapter_id = ?`, adapterID).Scan(&lease.AdapterID, &lease.InstanceID, &lease.LeaseToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AdapterLease{}, store.ErrLeaseFenced
	}
	if err != nil {
		return store.AdapterLease{}, fmt.Errorf("read current adapter lease: %w", err)
	}
	if lease.LeaseToken != token {
		return store.AdapterLease{}, store.ErrLeaseFenced
	}
	lease.ExpiresAt, err = parseTimestamp(expires)
	if err != nil {
		return store.AdapterLease{}, err
	}
	if !lease.ExpiresAt.After(now.UTC()) {
		return store.AdapterLease{}, store.ErrLeaseExpired
	}
	return lease, nil
}

func (s *Store) PutBinding(ctx context.Context, now time.Time, request store.PutBindingRequest) (store.Binding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Binding{}, fmt.Errorf("begin binding write: %w", err)
	}
	defer tx.Rollback()
	if _, err := currentLease(ctx, tx, now, request.ActorAdapterID, request.LeaseToken); err != nil {
		return store.Binding{}, err
	}

	existing, found, err := readBinding(ctx, tx, request.Address)
	if err != nil {
		return store.Binding{}, err
	}
	capabilities, err := json.Marshal(request.Capabilities)
	if err != nil {
		return store.Binding{}, fmt.Errorf("marshal capabilities: %w", err)
	}
	if !found {
		if request.ExpectedRevision != nil {
			return store.Binding{}, store.ErrStaleRevision
		}
		if request.ActorAdapterID != request.AdapterID {
			return store.Binding{}, store.ErrAdapterMismatch
		}
		created := store.Binding{Address: request.Address, Bound: true, AdapterID: request.AdapterID, TargetRef: request.TargetRef, Capabilities: request.Capabilities, Revision: 1, Generation: 1}
		if _, err := tx.ExecContext(ctx, `INSERT INTO address_bindings (address, adapter_id, target_ref, capabilities_json, revision, generation, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, created.Address, created.AdapterID, created.TargetRef, string(capabilities), created.Revision, created.Generation, timestamp(now)); err != nil {
			return store.Binding{}, fmt.Errorf("create binding: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return store.Binding{}, fmt.Errorf("commit binding create: %w", err)
		}
		return created, nil
	}
	if request.ExpectedRevision == nil {
		return store.Binding{}, store.ErrConflict
	}
	if *request.ExpectedRevision != existing.Revision {
		return store.Binding{}, store.ErrStaleRevision
	}
	if existing.Bound && existing.AdapterID != request.ActorAdapterID {
		return store.Binding{}, store.ErrAdapterMismatch
	}
	next := store.Binding{Address: existing.Address, Bound: true, AdapterID: request.AdapterID, TargetRef: request.TargetRef, Capabilities: request.Capabilities, Revision: existing.Revision + 1, Generation: existing.Generation}
	if !existing.Bound || existing.AdapterID != next.AdapterID || existing.TargetRef != next.TargetRef {
		next.Generation++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE address_bindings SET adapter_id = ?, target_ref = ?, capabilities_json = ?, revision = ?, generation = ?, updated_at = ? WHERE address = ?`, next.AdapterID, next.TargetRef, string(capabilities), next.Revision, next.Generation, timestamp(now), next.Address); err != nil {
		return store.Binding{}, fmt.Errorf("update binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Binding{}, fmt.Errorf("commit binding update: %w", err)
	}
	return next, nil
}

func (s *Store) Unbind(ctx context.Context, now time.Time, request store.UnbindRequest) (store.Binding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Binding{}, fmt.Errorf("begin binding unbind: %w", err)
	}
	defer tx.Rollback()
	if _, err := currentLease(ctx, tx, now, request.ActorAdapterID, request.LeaseToken); err != nil {
		return store.Binding{}, err
	}
	existing, found, err := readBinding(ctx, tx, request.Address)
	if err != nil {
		return store.Binding{}, err
	}
	if !found {
		return store.Binding{}, store.ErrNotFound
	}
	if !existing.Bound {
		return store.Binding{}, store.ErrNotBound
	}
	if existing.Revision != request.ExpectedRevision {
		return store.Binding{}, store.ErrStaleRevision
	}
	if existing.AdapterID != request.ActorAdapterID {
		return store.Binding{}, store.ErrAdapterMismatch
	}
	next := store.Binding{Address: existing.Address, Bound: false, Capabilities: []string{}, Revision: existing.Revision + 1, Generation: existing.Generation + 1}
	if _, err := tx.ExecContext(ctx, `UPDATE address_bindings SET adapter_id = NULL, target_ref = NULL, capabilities_json = ?, revision = ?, generation = ?, updated_at = ? WHERE address = ?`, "[]", next.Revision, next.Generation, timestamp(now), next.Address); err != nil {
		return store.Binding{}, fmt.Errorf("unbind binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Binding{}, fmt.Errorf("commit binding unbind: %w", err)
	}
	return next, nil
}

func (s *Store) Resolve(ctx context.Context, address string) (store.Binding, error) {
	binding, found, err := readBinding(ctx, s.db, address)
	if err != nil {
		return store.Binding{}, err
	}
	if !found {
		return store.Binding{}, store.ErrNotFound
	}
	return binding, nil
}

func (s *Store) List(ctx context.Context) ([]store.Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT address, adapter_id, target_ref, capabilities_json, revision, generation FROM address_bindings ORDER BY address ASC`)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]store.Binding, 0)
	for rows.Next() {
		binding, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bindings: %w", err)
	}
	return bindings, nil
}

func (s *Store) FenceSender(ctx context.Context, now time.Time, request store.SenderFenceRequest) (store.Binding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Binding{}, fmt.Errorf("begin sender fence: %w", err)
	}
	defer tx.Rollback()
	if _, err := currentLease(ctx, tx, now, request.AdapterID, request.LeaseToken); err != nil {
		return store.Binding{}, err
	}
	binding, found, err := readBinding(ctx, tx, request.SenderAddress)
	if err != nil {
		return store.Binding{}, err
	}
	if !found {
		return store.Binding{}, store.ErrNotFound
	}
	if !binding.Bound {
		return store.Binding{}, store.ErrNotBound
	}
	if binding.AdapterID != request.AdapterID {
		return store.Binding{}, store.ErrAdapterMismatch
	}
	if err := tx.Commit(); err != nil {
		return store.Binding{}, fmt.Errorf("commit sender fence: %w", err)
	}
	return binding, nil
}

// LookupMessageOperation is a read-only fast path so an exact replay does not
// depend on fresh service-generated IDs. SubmitMessage repeats this lookup in
// its transaction before validating authority or inserting new rows.
func (s *Store) LookupMessageOperation(ctx context.Context, producerID, operationID string) (store.OperationLookup, error) {
	existing, found, err := readSubmission(ctx, s.db, producerID, operationID)
	if err != nil {
		return store.OperationLookup{}, err
	}
	if !found {
		return store.OperationLookup{}, nil
	}
	return store.OperationLookup{Found: true, Fingerprint: existing.fingerprint, Message: existing.message, Delivery: existing.delivery}, nil
}

// SubmitMessage is the only acceptance write in this slice. It deliberately
// checks producer operation idempotency before current lease and routing
// authority, making exact retries durable across later authority drift.
func (s *Store) SubmitMessage(ctx context.Context, now time.Time, request store.SubmitMessageRequest) (store.SubmitMessageResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.SubmitMessageResult{}, fmt.Errorf("begin message submit: %w", err)
	}
	defer tx.Rollback()

	if existing, found, err := readSubmission(ctx, tx, request.ProducerID, request.OperationID); err != nil {
		return store.SubmitMessageResult{}, err
	} else if found {
		if existing.fingerprint != request.Fingerprint {
			return store.SubmitMessageResult{}, store.ErrOperationConflict
		}
		return store.SubmitMessageResult{Message: existing.message, Delivery: existing.delivery, Replayed: true}, nil
	}

	message, delivery, err := acceptMessage(ctx, tx, now, request)
	if err != nil {
		return store.SubmitMessageResult{}, err
	}
	if err := resolveRoundReply(ctx, tx, now, message); err != nil {
		return store.SubmitMessageResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.SubmitMessageResult{}, fmt.Errorf("commit message submit: %w", err)
	}
	return store.SubmitMessageResult{Message: message, Delivery: delivery}, nil
}

// acceptMessage is shared by ordinary submission and round-root creation. The
// caller owns the surrounding transaction and idempotency check.
func acceptMessage(ctx context.Context, tx *sql.Tx, now time.Time, request store.SubmitMessageRequest) (store.Message, store.Delivery, error) {
	if _, err := currentLease(ctx, tx, now, request.ProducerID, request.LeaseToken); err != nil {
		return store.Message{}, store.Delivery{}, err
	}
	sender, found, err := readBinding(ctx, tx, request.SenderAddress)
	if err != nil {
		return store.Message{}, store.Delivery{}, err
	}
	if !found {
		return store.Message{}, store.Delivery{}, store.ErrNotFound
	}
	if !sender.Bound {
		return store.Message{}, store.Delivery{}, store.ErrNotBound
	}
	if sender.AdapterID != request.ProducerID {
		return store.Message{}, store.Delivery{}, store.ErrAdapterMismatch
	}
	recipient, found, err := readBinding(ctx, tx, request.RecipientAddress)
	if err != nil {
		return store.Message{}, store.Delivery{}, err
	}
	if !found {
		return store.Message{}, store.Delivery{}, store.ErrNotFound
	}
	if !recipient.Bound {
		return store.Message{}, store.Delivery{}, store.ErrNotBound
	}
	message := store.Message{ProducerID: request.ProducerID, OperationID: request.OperationID, MessageID: request.MessageID, SenderAddress: request.SenderAddress, RecipientAddress: request.RecipientAddress, Body: request.Body, CorrelationID: request.CorrelationID, ReplyToMessageID: request.ReplyToMessageID, ActivationPolicy: request.ActivationPolicy, CreatedAt: now.UTC(), ExpiresAt: request.ExpiresAt.UTC(), SenderGeneration: sender.Generation, RecipientGeneration: recipient.Generation}
	if err := validateReply(ctx, tx, message); err != nil {
		return store.Message{}, store.Delivery{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (message_id, producer_id, operation_id, request_fingerprint, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, message.MessageID, message.ProducerID, message.OperationID, request.Fingerprint, message.SenderAddress, message.RecipientAddress, message.Body, message.CorrelationID, message.ReplyToMessageID, message.ActivationPolicy, timestamp(message.CreatedAt), timestamp(message.ExpiresAt), message.SenderGeneration, message.RecipientGeneration); err != nil {
		return store.Message{}, store.Delivery{}, fmt.Errorf("insert immutable message: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO deliveries (delivery_id, message_id, recipient_address, recipient_generation, state, created_at, expires_at) VALUES (?, ?, ?, ?, 'queued', ?, ?)`, request.DeliveryID, message.MessageID, message.RecipientAddress, message.RecipientGeneration, timestamp(now), timestamp(message.ExpiresAt))
	if err != nil {
		return store.Message{}, store.Delivery{}, fmt.Errorf("insert queued delivery: %w", err)
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return store.Message{}, store.Delivery{}, fmt.Errorf("read delivery sequence: %w", err)
	}
	return message, store.Delivery{DeliveryID: request.DeliveryID, MessageID: message.MessageID, RecipientAddress: message.RecipientAddress, RecipientGeneration: message.RecipientGeneration, AcceptedSequence: sequence, State: "queued", CreatedAt: now.UTC(), ExpiresAt: message.ExpiresAt}, nil
}

func (s *Store) GetMessage(ctx context.Context, messageID string) (store.Message, error) {
	message, found, err := readMessage(ctx, s.db, messageID)
	if err != nil {
		return store.Message{}, err
	}
	if !found {
		return store.Message{}, store.ErrNotFound
	}
	return message, nil
}

func (s *Store) ListMessages(ctx context.Context) ([]store.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, producer_id, operation_id, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation FROM messages ORDER BY created_at ASC, message_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	values := make([]store.Message, 0)
	for rows.Next() {
		value, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return values, nil
}

func (s *Store) GetDelivery(ctx context.Context, deliveryID string) (store.Delivery, error) {
	delivery, found, err := readDelivery(ctx, s.db, deliveryID)
	if err != nil {
		return store.Delivery{}, err
	}
	if !found {
		return store.Delivery{}, store.ErrNotFound
	}
	return publicDelivery(delivery), nil
}

func (s *Store) ListDeliveries(ctx context.Context) ([]store.Delivery, error) {
	return s.listDeliveries(ctx, deliveryProjection+` FROM deliveries ORDER BY accepted_sequence ASC`)
}
func (s *Store) Mailbox(ctx context.Context, address string) ([]store.Delivery, error) {
	return s.listDeliveries(ctx, deliveryProjection+` FROM deliveries WHERE recipient_address = ? ORDER BY accepted_sequence ASC`, address)
}

// HeadDelivery is strictly observational. Terminal rows are skipped, while
// the first nonterminal item prevents selection of later queued work.
func (s *Store) HeadDelivery(ctx context.Context, address string, generation int64) (*store.Delivery, error) {
	row := s.db.QueryRowContext(ctx, deliveryProjection+` FROM deliveries WHERE recipient_address = ? AND recipient_generation = ? AND state NOT IN ('delivered','failed','expired','cancelled','outcome_unknown') ORDER BY accepted_sequence ASC LIMIT 1`, address, generation)
	delivery, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	delivery = publicDelivery(delivery)
	return &delivery, nil
}

func (s *Store) listDeliveries(ctx context.Context, query string, args ...any) ([]store.Delivery, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	values := make([]store.Delivery, 0)
	for rows.Next() {
		value, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, publicDelivery(value))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return values, nil
}

func (s *Store) CancelDelivery(ctx context.Context, now time.Time, deliveryID string) (store.Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Delivery{}, fmt.Errorf("begin delivery cancel: %w", err)
	}
	defer tx.Rollback()
	delivery, found, err := readDelivery(ctx, tx, deliveryID)
	if err != nil {
		return store.Delivery{}, err
	}
	if !found {
		return store.Delivery{}, store.ErrNotFound
	}
	if delivery.State == "cancelled" {
		return publicDelivery(delivery), nil
	}
	if delivery.State != "queued" && delivery.State != "claimed" {
		return store.Delivery{}, store.ErrConflict
	}
	terminal := now.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state = 'cancelled', terminal_reason = 'cancelled_by_request', terminal_at = ?, claim_owner_adapter_id='', claim_owner_instance_id='', claim_token='', claim_expires_at=NULL, claimed_at=NULL, dispatch_action='' WHERE delivery_id = ? AND state IN ('queued','claimed')`, timestamp(terminal), deliveryID); err != nil {
		return store.Delivery{}, fmt.Errorf("cancel delivery: %w", err)
	}
	if err := bridgeRoundDeliveryTerminal(ctx, tx, now, deliveryID, "cancelled", "cancelled_by_request"); err != nil {
		return store.Delivery{}, err
	}
	updated, found, err := readDelivery(ctx, tx, deliveryID)
	if err != nil {
		return store.Delivery{}, err
	}
	if !found {
		return store.Delivery{}, store.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return store.Delivery{}, fmt.Errorf("commit delivery cancel: %w", err)
	}
	return publicDelivery(updated), nil
}

// SettlePending is explicit write-side maintenance. It is intentionally not
// invoked by reads or startup and it never changes terminal ledger entries.
func (s *Store) SettlePending(ctx context.Context, now time.Time) ([]store.Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin pending settlement: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, deliveryProjection+` FROM deliveries WHERE state IN ('queued','claimed','dispatching') ORDER BY accepted_sequence ASC`)
	if err != nil {
		return nil, fmt.Errorf("list queued deliveries for settlement: %w", err)
	}
	defer rows.Close()
	settled := make([]store.Delivery, 0)
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		reason, state := "", ""
		if delivery.State == "dispatching" {
			if !adapterLeaseLive(ctx, tx, now, delivery.ClaimOwnerAdapterID) {
				reason, state = "dispatch_lost", "outcome_unknown"
			}
		} else if !delivery.ExpiresAt.After(now.UTC()) {
			reason, state = "ttl_expired", "expired"
		} else {
			binding, found, err := readBinding(ctx, tx, delivery.RecipientAddress)
			if err != nil {
				return nil, err
			}
			if !found || !binding.Bound {
				reason, state = "recipient_unbound", "failed"
			} else if binding.Generation != delivery.RecipientGeneration {
				reason, state = "binding_generation_changed", "failed"
			}
			if reason == "" && delivery.State == "claimed" && delivery.ClaimExpiresAt != nil && !delivery.ClaimExpiresAt.After(now.UTC()) {
				if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='queued', claim_owner_adapter_id='', claim_owner_instance_id='', claim_token='', claim_expires_at=NULL, claimed_at=NULL, dispatch_action='' WHERE delivery_id=? AND state='claimed'`, delivery.DeliveryID); err != nil {
					return nil, fmt.Errorf("release expired claim: %w", err)
				}
				updated, _, err := readDelivery(ctx, tx, delivery.DeliveryID)
				if err != nil {
					return nil, err
				}
				settled = append(settled, publicDelivery(updated))
				continue
			}
		}
		if reason == "" {
			continue
		}
		terminal := now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state = ?, terminal_reason = ?, terminal_at = ? WHERE delivery_id = ? AND state IN ('queued','claimed','dispatching')`, state, reason, timestamp(terminal), delivery.DeliveryID); err != nil {
			return nil, fmt.Errorf("settle delivery: %w", err)
		}
		if err := bridgeRoundDeliveryTerminal(ctx, tx, now, delivery.DeliveryID, state, reason); err != nil {
			return nil, err
		}
		delivery.State, delivery.TerminalReason, delivery.TerminalAt = state, reason, &terminal
		settled = append(settled, publicDelivery(delivery))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued settlement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit pending settlement: %w", err)
	}
	return settled, nil
}

func publicDelivery(value store.Delivery) store.Delivery {
	value.ClaimToken = ""
	return value
}

func adapterLeaseLive(ctx context.Context, q queryer, now time.Time, adapterID string) bool {
	if adapterID == "" {
		return false
	}
	var value string
	err := q.QueryRowContext(ctx, `SELECT expires_at FROM adapter_leases WHERE adapter_id = ?`, adapterID).Scan(&value)
	if err != nil {
		return false
	}
	expires, err := parseTimestamp(value)
	return err == nil && expires.After(now.UTC())
}

type submission struct {
	message     store.Message
	delivery    store.Delivery
	fingerprint string
}

func readSubmission(ctx context.Context, query queryer, producerID, operationID string) (submission, bool, error) {
	row := query.QueryRowContext(ctx, `SELECT message_id, producer_id, operation_id, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation, request_fingerprint FROM messages WHERE producer_id = ? AND operation_id = ?`, producerID, operationID)
	var result submission
	var created, expires string
	err := row.Scan(&result.message.MessageID, &result.message.ProducerID, &result.message.OperationID, &result.message.SenderAddress, &result.message.RecipientAddress, &result.message.Body, &result.message.CorrelationID, &result.message.ReplyToMessageID, &result.message.ActivationPolicy, &created, &expires, &result.message.SenderGeneration, &result.message.RecipientGeneration, &result.fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return submission{}, false, nil
	}
	if err != nil {
		return submission{}, false, fmt.Errorf("read existing operation: %w", err)
	}
	if result.message.CreatedAt, err = parseTimestamp(created); err != nil {
		return submission{}, false, err
	}
	if result.message.ExpiresAt, err = parseTimestamp(expires); err != nil {
		return submission{}, false, err
	}
	delivery, found, err := readDeliveryByMessage(ctx, query, result.message.MessageID)
	if err != nil {
		return submission{}, false, err
	}
	if !found {
		return submission{}, false, fmt.Errorf("message operation missing delivery")
	}
	result.delivery = delivery
	result.delivery = publicDelivery(result.delivery)
	return result, true, nil
}

func readDeliveryByMessage(ctx context.Context, query queryer, messageID string) (store.Delivery, bool, error) {
	row := query.QueryRowContext(ctx, deliveryProjection+` FROM deliveries WHERE message_id = ?`, messageID)
	value, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Delivery{}, false, nil
	}
	if err != nil {
		return store.Delivery{}, false, err
	}
	return value, true, nil
}

func readMessage(ctx context.Context, query queryer, messageID string) (store.Message, bool, error) {
	row := query.QueryRowContext(ctx, `SELECT message_id, producer_id, operation_id, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation FROM messages WHERE message_id = ?`, messageID)
	message, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Message{}, false, nil
	}
	if err != nil {
		return store.Message{}, false, err
	}
	return message, true, nil
}
func readDelivery(ctx context.Context, query queryer, deliveryID string) (store.Delivery, bool, error) {
	row := query.QueryRowContext(ctx, deliveryProjection+` FROM deliveries WHERE delivery_id = ?`, deliveryID)
	delivery, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Delivery{}, false, nil
	}
	if err != nil {
		return store.Delivery{}, false, err
	}
	return delivery, true, nil
}
func scanMessage(row scanner) (store.Message, error) {
	var value store.Message
	var created, expires string
	if err := row.Scan(&value.MessageID, &value.ProducerID, &value.OperationID, &value.SenderAddress, &value.RecipientAddress, &value.Body, &value.CorrelationID, &value.ReplyToMessageID, &value.ActivationPolicy, &created, &expires, &value.SenderGeneration, &value.RecipientGeneration); err != nil {
		return store.Message{}, err
	}
	createdAt, err := parseTimestamp(created)
	if err != nil {
		return store.Message{}, err
	}
	expiresAt, err := parseTimestamp(expires)
	if err != nil {
		return store.Message{}, err
	}
	value.CreatedAt, value.ExpiresAt = createdAt, expiresAt
	return value, nil
}

const deliveryProjection = `SELECT delivery_id, message_id, recipient_address, recipient_generation, accepted_sequence, state, attempt_count, claim_owner_adapter_id, claim_owner_instance_id, claim_token, claim_expires_at, claimed_at, dispatch_action, native_attempt_ref, dispatching_at, terminal_reason, created_at, expires_at, terminal_at`

func scanDelivery(row scanner) (store.Delivery, error) {
	var value store.Delivery
	var created, expires string
	var terminal, claimExpires, claimed, dispatching sql.NullString
	if err := row.Scan(&value.DeliveryID, &value.MessageID, &value.RecipientAddress, &value.RecipientGeneration, &value.AcceptedSequence, &value.State, &value.AttemptCount, &value.ClaimOwnerAdapterID, &value.ClaimOwnerInstanceID, &value.ClaimToken, &claimExpires, &claimed, &value.DispatchAction, &value.NativeAttemptRef, &dispatching, &value.TerminalReason, &created, &expires, &terminal); err != nil {
		return store.Delivery{}, err
	}
	if err := populateTimes(nil, &value, "", "", created, expires, terminal); err != nil {
		return store.Delivery{}, err
	}
	for _, candidate := range []struct {
		raw    sql.NullString
		target **time.Time
	}{{claimExpires, &value.ClaimExpiresAt}, {claimed, &value.ClaimedAt}, {dispatching, &value.DispatchingAt}} {
		if candidate.raw.Valid {
			parsed, err := parseTimestamp(candidate.raw.String)
			if err != nil {
				return store.Delivery{}, err
			}
			*candidate.target = &parsed
		}
	}
	return value, nil
}
func populateTimes(message *store.Message, delivery *store.Delivery, created, expires, deliveryCreated, deliveryExpires string, terminal sql.NullString) error {
	if message != nil {
		var err error
		if message.CreatedAt, err = parseTimestamp(created); err != nil {
			return err
		}
		if message.ExpiresAt, err = parseTimestamp(expires); err != nil {
			return err
		}
	}
	if delivery != nil {
		var err error
		if delivery.CreatedAt, err = parseTimestamp(deliveryCreated); err != nil {
			return err
		}
		if delivery.ExpiresAt, err = parseTimestamp(deliveryExpires); err != nil {
			return err
		}
		if terminal.Valid {
			parsed, err := parseTimestamp(terminal.String)
			if err != nil {
				return err
			}
			delivery.TerminalAt = &parsed
		}
	}
	return nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readBinding(ctx context.Context, query queryer, address string) (store.Binding, bool, error) {
	row := query.QueryRowContext(ctx, `SELECT address, adapter_id, target_ref, capabilities_json, revision, generation FROM address_bindings WHERE address = ?`, address)
	binding, err := scanBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Binding{}, false, nil
	}
	if err != nil {
		return store.Binding{}, false, fmt.Errorf("read binding: %w", err)
	}
	return binding, true, nil
}

type scanner interface{ Scan(...any) error }

func scanBinding(row scanner) (store.Binding, error) {
	var binding store.Binding
	var adapterID, targetRef sql.NullString
	var capabilities string
	if err := row.Scan(&binding.Address, &adapterID, &targetRef, &capabilities, &binding.Revision, &binding.Generation); err != nil {
		return store.Binding{}, err
	}
	binding.Bound = adapterID.Valid
	binding.AdapterID = adapterID.String
	binding.TargetRef = targetRef.String
	if err := json.Unmarshal([]byte(capabilities), &binding.Capabilities); err != nil {
		return store.Binding{}, fmt.Errorf("decode capabilities: %w", err)
	}
	if binding.Capabilities == nil {
		binding.Capabilities = []string{}
	}
	return binding, nil
}
