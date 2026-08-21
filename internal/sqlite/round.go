package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"crew-services/internal/store"
)

const roundProjection = `round_id, producer_id, operation_id, root_message_id, sender_address, recipient_address, sender_generation, recipient_generation, correlation_id, status, reply_message_id, created_at, expires_at, terminal_at, terminal_reason, revision`

func (s *Store) LookupRoundOperation(ctx context.Context, producerID, operationID string) (store.RoundOperationLookup, error) {
	submission, found, err := readSubmission(ctx, s.db, producerID, operationID)
	if err != nil || !found {
		return store.RoundOperationLookup{}, err
	}
	round, roundFound, err := readRoundByRoot(ctx, s.db, submission.message.MessageID)
	if err != nil {
		return store.RoundOperationLookup{}, err
	}
	result := store.BeginRoundResult{Message: submission.message, Delivery: submission.delivery}
	if roundFound {
		result.Round = round
	}
	return store.RoundOperationLookup{Found: true, Fingerprint: submission.fingerprint, Result: result}, nil
}

// BeginRound accepts the root envelope, root delivery, and durable round in a
// single transaction. It intentionally does not call SubmitMessage.
func (s *Store) BeginRound(ctx context.Context, now time.Time, r store.BeginRoundRequest) (store.BeginRoundResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.BeginRoundResult{}, fmt.Errorf("begin round: %w", err)
	}
	defer tx.Rollback()
	if existing, found, err := readSubmission(ctx, tx, r.ProducerID, r.OperationID); err != nil {
		return store.BeginRoundResult{}, err
	} else if found {
		if existing.fingerprint != r.Fingerprint {
			return store.BeginRoundResult{}, store.ErrOperationConflict
		}
		round, roundFound, err := readRoundByRoot(ctx, tx, existing.message.MessageID)
		if err != nil {
			return store.BeginRoundResult{}, err
		}
		if !roundFound {
			return store.BeginRoundResult{}, store.ErrOperationConflict
		}
		return store.BeginRoundResult{Round: round, Message: existing.message, Delivery: existing.delivery, Replayed: true}, nil
	}
	message, delivery, err := acceptMessage(ctx, tx, now, r.SubmitMessageRequest)
	if err != nil {
		return store.BeginRoundResult{}, err
	}
	round := store.Round{RoundID: r.RoundID, ProducerID: r.ProducerID, OperationID: r.OperationID, RootMessageID: message.MessageID, SenderAddress: message.SenderAddress, RecipientAddress: message.RecipientAddress, SenderGeneration: message.SenderGeneration, RecipientGeneration: message.RecipientGeneration, CorrelationID: message.CorrelationID, Status: "pending", CreatedAt: now.UTC(), ExpiresAt: r.RoundExpiresAt.UTC(), Revision: 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rounds (round_id, producer_id, operation_id, root_message_id, sender_address, recipient_address, sender_generation, recipient_generation, correlation_id, status, reply_message_id, created_at, expires_at, terminal_at, terminal_reason, revision) VALUES (?,?,?,?,?,?,?,?,?,'pending',NULL,?,?,NULL,'',1)`, round.RoundID, round.ProducerID, round.OperationID, round.RootMessageID, round.SenderAddress, round.RecipientAddress, round.SenderGeneration, round.RecipientGeneration, round.CorrelationID, timestamp(round.CreatedAt), timestamp(round.ExpiresAt)); err != nil {
		return store.BeginRoundResult{}, fmt.Errorf("insert pending round: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.BeginRoundResult{}, fmt.Errorf("commit round: %w", err)
	}
	return store.BeginRoundResult{Round: round, Message: message, Delivery: delivery}, nil
}

func (s *Store) GetRound(ctx context.Context, roundID string) (store.Round, error) {
	v, found, err := readRound(ctx, s.db, roundID)
	if err != nil {
		return store.Round{}, err
	}
	if !found {
		return store.Round{}, store.ErrNotFound
	}
	return v, nil
}
func (s *Store) ListRounds(ctx context.Context) ([]store.Round, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+roundProjection+` FROM rounds ORDER BY created_at ASC, round_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list rounds: %w", err)
	}
	defer rows.Close()
	values := make([]store.Round, 0)
	for rows.Next() {
		v, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rounds: %w", err)
	}
	return values, nil
}

func (s *Store) CancelRound(ctx context.Context, now time.Time, roundID string) (store.Round, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Round{}, fmt.Errorf("begin round cancel: %w", err)
	}
	defer tx.Rollback()
	round, found, err := readRound(ctx, tx, roundID)
	if err != nil {
		return store.Round{}, err
	}
	if !found {
		return store.Round{}, store.ErrNotFound
	}
	if round.Status == "cancelled" {
		return round, nil
	}
	if round.Status != "pending" {
		return store.Round{}, store.ErrConflict
	}
	if err := terminalizeRound(ctx, tx, now, round.RoundID, "cancelled", "cancelled_by_request", ""); err != nil {
		return store.Round{}, err
	}
	round, _, err = readRound(ctx, tx, roundID)
	if err != nil {
		return store.Round{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Round{}, fmt.Errorf("commit round cancel: %w", err)
	}
	return round, nil
}

func (s *Store) SettleRounds(ctx context.Context, now time.Time) ([]store.Round, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin round settlement: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+roundProjection+` FROM rounds WHERE status='pending' AND expires_at <= ? ORDER BY created_at ASC, round_id ASC`, timestamp(now))
	if err != nil {
		return nil, fmt.Errorf("list expired rounds: %w", err)
	}
	defer rows.Close()
	settled := make([]store.Round, 0)
	for rows.Next() {
		round, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		if err := terminalizeRound(ctx, tx, now, round.RoundID, "expired", "ttl_expired", ""); err != nil {
			return nil, err
		}
		round, _, err = readRound(ctx, tx, round.RoundID)
		if err != nil {
			return nil, err
		}
		settled = append(settled, round)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit round settlement: %w", err)
	}
	return settled, nil
}

func (s *Store) Traffic(ctx context.Context) (store.Traffic, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Traffic{}, fmt.Errorf("begin traffic snapshot: %w", err)
	}
	defer tx.Rollback()
	messages, err := listMessagesQuery(ctx, tx)
	if err != nil {
		return store.Traffic{}, err
	}
	deliveries, err := listDeliveriesQuery(ctx, tx)
	if err != nil {
		return store.Traffic{}, err
	}
	rounds, err := listRoundsQuery(ctx, tx)
	if err != nil {
		return store.Traffic{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.Traffic{}, fmt.Errorf("commit traffic snapshot: %w", err)
	}
	return store.Traffic{Messages: messages, Deliveries: deliveries, Rounds: rounds}, nil
}

func validateReply(ctx context.Context, q queryer, message store.Message) error {
	if message.ReplyToMessageID == "" {
		return nil
	}
	original, found, err := readMessage(ctx, q, message.ReplyToMessageID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrReplyOriginalNotFound
	}
	if message.SenderAddress != original.RecipientAddress {
		return store.ErrReplySenderMismatch
	}
	if message.RecipientAddress != original.SenderAddress {
		return store.ErrReplyRecipientMismatch
	}
	if message.SenderGeneration != original.RecipientGeneration || message.RecipientGeneration != original.SenderGeneration {
		return store.ErrReplyGenerationMismatch
	}
	return nil
}

func resolveRoundReply(ctx context.Context, tx *sql.Tx, now time.Time, message store.Message) error {
	if message.ReplyToMessageID == "" {
		return nil
	}
	round, found, err := readRoundByRoot(ctx, tx, message.ReplyToMessageID)
	if err != nil || !found || round.Status != "pending" {
		return err
	}
	if !round.ExpiresAt.After(now.UTC()) {
		return terminalizeRound(ctx, tx, now, round.RoundID, "expired", "ttl_expired", "")
	}
	return terminalizeRound(ctx, tx, now, round.RoundID, "replied", "reply_received", message.MessageID)
}

func bridgeRoundDeliveryTerminal(ctx context.Context, tx *sql.Tx, now time.Time, deliveryID, state, reason string) error {
	if state != "failed" && state != "expired" && state != "cancelled" {
		return nil
	}
	var messageID string
	err := tx.QueryRowContext(ctx, `SELECT message_id FROM deliveries WHERE delivery_id=?`, deliveryID).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	round, found, err := readRoundByRoot(ctx, tx, messageID)
	if err != nil || !found || round.Status != "pending" {
		return err
	}
	return terminalizeRound(ctx, tx, now, round.RoundID, state, reason, "")
}

func terminalizeRound(ctx context.Context, tx *sql.Tx, now time.Time, id, status, reason, replyID string) error {
	result, err := tx.ExecContext(ctx, `UPDATE rounds SET status=?, reply_message_id=NULLIF(?, ''), terminal_at=?, terminal_reason=?, revision=revision+1 WHERE round_id=? AND status='pending'`, status, replyID, timestamp(now), reason, id)
	if err != nil {
		return fmt.Errorf("terminalize round: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil
	}
	return nil
}

func readRound(ctx context.Context, q queryer, id string) (store.Round, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+roundProjection+` FROM rounds WHERE round_id=?`, id)
	v, err := scanRound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Round{}, false, nil
	}
	return v, err == nil, err
}
func readRoundByRoot(ctx context.Context, q queryer, rootID string) (store.Round, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+roundProjection+` FROM rounds WHERE root_message_id=?`, rootID)
	v, err := scanRound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Round{}, false, nil
	}
	return v, err == nil, err
}
func listMessagesQuery(ctx context.Context, q queryer) ([]store.Message, error) {
	rows, err := q.QueryContext(ctx, `SELECT message_id, producer_id, operation_id, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation FROM messages ORDER BY created_at, message_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]store.Message, 0)
	for rows.Next() {
		v, e := scanMessage(rows)
		if e != nil {
			return nil, e
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
func listDeliveriesQuery(ctx context.Context, q queryer) ([]store.Delivery, error) {
	rows, err := q.QueryContext(ctx, deliveryProjection+` FROM deliveries ORDER BY accepted_sequence`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]store.Delivery, 0)
	for rows.Next() {
		v, e := scanDelivery(rows)
		if e != nil {
			return nil, e
		}
		values = append(values, publicDelivery(v))
	}
	return values, rows.Err()
}
func listRoundsQuery(ctx context.Context, q queryer) ([]store.Round, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+roundProjection+` FROM rounds ORDER BY created_at,round_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]store.Round, 0)
	for rows.Next() {
		v, e := scanRound(rows)
		if e != nil {
			return nil, e
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func scanRound(row interface{ Scan(...any) error }) (store.Round, error) {
	var v store.Round
	var created, expires string
	var terminal sql.NullString
	var reply sql.NullString
	if err := row.Scan(&v.RoundID, &v.ProducerID, &v.OperationID, &v.RootMessageID, &v.SenderAddress, &v.RecipientAddress, &v.SenderGeneration, &v.RecipientGeneration, &v.CorrelationID, &v.Status, &reply, &created, &expires, &terminal, &v.TerminalReason, &v.Revision); err != nil {
		return store.Round{}, err
	}
	var err error
	if v.CreatedAt, err = parseTimestamp(created); err != nil {
		return store.Round{}, err
	}
	if v.ExpiresAt, err = parseTimestamp(expires); err != nil {
		return store.Round{}, err
	}
	if reply.Valid {
		v.ReplyMessageID = reply.String
	}
	if terminal.Valid {
		t, e := parseTimestamp(terminal.String)
		if e != nil {
			return store.Round{}, e
		}
		v.TerminalAt = &t
	}
	return v, nil
}
