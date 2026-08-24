package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"crew-services/internal/store"
)

const sessionProjection = `session_id, adapter_id, adapter_key, label, location, status, capabilities_json, revision, created_at, updated_at`
const sessionEventProjection = `event_id, session_id, sequence, cursor, event_type, payload_json, occurred_at, recorded_at`
const sessionEventProjectionQualified = `e.event_id, e.session_id, e.sequence, e.cursor, e.event_type, e.payload_json, e.occurred_at, e.recorded_at`

func (s *Store) AdoptSession(ctx context.Context, now time.Time, r store.AdoptSessionRequest) (store.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Session{}, fmt.Errorf("begin session adoption: %w", err)
	}
	defer tx.Rollback()
	if _, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken); err != nil {
		return store.Session{}, err
	}
	existing, found, err := readSessionByAdapterKey(ctx, tx, r.AdapterID, r.AdapterKey)
	if err != nil {
		return store.Session{}, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return store.Session{}, fmt.Errorf("commit session adoption replay: %w", err)
		}
		return existing, nil
	}
	encoded, err := json.Marshal(r.Capabilities)
	if err != nil {
		return store.Session{}, fmt.Errorf("encode session capabilities: %w", err)
	}
	v := store.Session{SessionID: r.SessionID, AdapterID: r.AdapterID, AdapterKey: r.AdapterKey, Label: r.Label, Location: r.Location, Status: r.Status, Capabilities: r.Capabilities, Revision: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (`+sessionProjection+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, v.SessionID, v.AdapterID, v.AdapterKey, v.Label, v.Location, v.Status, string(encoded), v.Revision, timestamp(v.CreatedAt), timestamp(v.UpdatedAt)); err != nil {
		return store.Session{}, fmt.Errorf("insert session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Session{}, fmt.Errorf("commit session adoption: %w", err)
	}
	return v, nil
}

func (s *Store) UpdateSession(ctx context.Context, now time.Time, r store.UpdateSessionRequest) (store.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Session{}, fmt.Errorf("begin session update: %w", err)
	}
	defer tx.Rollback()
	if _, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken); err != nil {
		return store.Session{}, err
	}
	existing, found, err := readSession(ctx, tx, r.SessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !found {
		return store.Session{}, store.ErrNotFound
	}
	if existing.AdapterID != r.AdapterID {
		return store.Session{}, store.ErrAdapterMismatch
	}
	if existing.Revision != r.ExpectedRevision {
		return store.Session{}, store.ErrStaleRevision
	}
	encoded, err := json.Marshal(r.Capabilities)
	if err != nil {
		return store.Session{}, fmt.Errorf("encode session capabilities: %w", err)
	}
	next := existing
	next.Label, next.Location, next.Status, next.Capabilities = r.Label, r.Location, r.Status, r.Capabilities
	next.Revision++
	next.UpdatedAt = now.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET label=?, location=?, status=?, capabilities_json=?, revision=?, updated_at=? WHERE session_id=?`, next.Label, next.Location, next.Status, string(encoded), next.Revision, timestamp(next.UpdatedAt), next.SessionID); err != nil {
		return store.Session{}, fmt.Errorf("update session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Session{}, fmt.Errorf("commit session update: %w", err)
	}
	return next, nil
}

func (s *Store) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	v, found, err := readSession(ctx, s.db, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !found {
		return store.Session{}, store.ErrNotFound
	}
	return v, nil
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]store.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionProjection+` FROM sessions ORDER BY updated_at DESC, session_id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	values := make([]store.Session, 0)
	for rows.Next() {
		v, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return values, nil
}

func (s *Store) LookupSessionEventOperation(ctx context.Context, adapterID, operationID string) (store.SessionEventOperationLookup, error) {
	row := s.db.QueryRowContext(ctx, `SELECT r.fingerprint, `+sessionEventProjectionQualified+` FROM session_event_operation_receipts r JOIN session_events e ON e.event_id=r.event_id WHERE r.adapter_id=? AND r.operation_id=?`, adapterID, operationID)
	var fingerprint string
	v, err := scanSessionEvent(row, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SessionEventOperationLookup{}, nil
	}
	if err != nil {
		return store.SessionEventOperationLookup{}, fmt.Errorf("read session event receipt: %w", err)
	}
	return store.SessionEventOperationLookup{Found: true, Fingerprint: fingerprint, Event: v}, nil
}

func (s *Store) AppendSessionEvent(ctx context.Context, now time.Time, r store.AppendSessionEventRequest) (store.SessionEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.SessionEvent{}, fmt.Errorf("begin session event append: %w", err)
	}
	defer tx.Rollback()
	if old, err := lookupSessionEventOperation(ctx, tx, r.AdapterID, r.OperationID); err != nil {
		return store.SessionEvent{}, err
	} else if old.Found {
		if old.Fingerprint != r.Fingerprint {
			return store.SessionEvent{}, store.ErrOperationConflict
		}
		return old.Event, nil
	}
	if _, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken); err != nil {
		return store.SessionEvent{}, err
	}
	session, found, err := readSession(ctx, tx, r.SessionID)
	if err != nil {
		return store.SessionEvent{}, err
	}
	if !found {
		return store.SessionEvent{}, store.ErrNotFound
	}
	if session.AdapterID != r.AdapterID {
		return store.SessionEvent{}, store.ErrAdapterMismatch
	}
	if session.Revision != r.ExpectedRevision {
		return store.SessionEvent{}, store.ErrStaleRevision
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM session_events WHERE session_id=?`, r.SessionID).Scan(&sequence); err != nil {
		return store.SessionEvent{}, fmt.Errorf("allocate session event sequence: %w", err)
	}
	v := store.SessionEvent{EventID: r.EventID, SessionID: r.SessionID, Sequence: sequence, EventType: r.EventType, Payload: r.Payload, OccurredAt: r.OccurredAt.UTC(), RecordedAt: now.UTC()}
	result, err := tx.ExecContext(ctx, `INSERT INTO session_events(event_id,session_id,sequence,event_type,payload_json,occurred_at,recorded_at) VALUES(?,?,?,?,?,?,?)`, v.EventID, v.SessionID, v.Sequence, v.EventType, string(v.Payload), timestamp(v.OccurredAt), timestamp(v.RecordedAt))
	if err != nil {
		return store.SessionEvent{}, fmt.Errorf("insert session event: %w", err)
	}
	cursor, err := result.LastInsertId()
	if err != nil {
		return store.SessionEvent{}, fmt.Errorf("read event cursor: %w", err)
	}
	v.Cursor = cursor
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_event_operation_receipts(adapter_id,operation_id,fingerprint,event_id,created_at) VALUES(?,?,?,?,?)`, r.AdapterID, r.OperationID, r.Fingerprint, r.EventID, timestamp(now)); err != nil {
		return store.SessionEvent{}, fmt.Errorf("record session event receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.SessionEvent{}, fmt.Errorf("commit session event append: %w", err)
	}
	return v, nil
}

func (s *Store) ListSessionEvents(ctx context.Context, sessionID string, cursor int64, limit int) ([]store.SessionEvent, error) {
	query, args := `SELECT `+sessionEventProjection+` FROM session_events WHERE cursor>?`, []any{cursor}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	query += ` ORDER BY cursor ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	defer rows.Close()
	values := make([]store.SessionEvent, 0)
	for rows.Next() {
		v, err := scanSessionEvent(rows, nil)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session events: %w", err)
	}
	return values, nil
}

func lookupSessionEventOperation(ctx context.Context, q queryer, adapterID, operationID string) (store.SessionEventOperationLookup, error) {
	row := q.QueryRowContext(ctx, `SELECT r.fingerprint, `+sessionEventProjectionQualified+` FROM session_event_operation_receipts r JOIN session_events e ON e.event_id=r.event_id WHERE r.adapter_id=? AND r.operation_id=?`, adapterID, operationID)
	var fingerprint string
	v, err := scanSessionEvent(row, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SessionEventOperationLookup{}, nil
	}
	if err != nil {
		return store.SessionEventOperationLookup{}, fmt.Errorf("read session event receipt: %w", err)
	}
	return store.SessionEventOperationLookup{Found: true, Fingerprint: fingerprint, Event: v}, nil
}

func readSession(ctx context.Context, q queryer, sessionID string) (store.Session, bool, error) {
	v, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sessionProjection+` FROM sessions WHERE session_id=?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, fmt.Errorf("read session: %w", err)
	}
	return v, true, nil
}
func readSessionByAdapterKey(ctx context.Context, q queryer, adapterID, key string) (store.Session, bool, error) {
	v, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sessionProjection+` FROM sessions WHERE adapter_id=? AND adapter_key=?`, adapterID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, fmt.Errorf("read adopted session: %w", err)
	}
	return v, true, nil
}
func scanSession(row scanner) (store.Session, error) {
	var v store.Session
	var capabilities, created, updated string
	if err := row.Scan(&v.SessionID, &v.AdapterID, &v.AdapterKey, &v.Label, &v.Location, &v.Status, &capabilities, &v.Revision, &created, &updated); err != nil {
		return store.Session{}, err
	}
	if err := json.Unmarshal([]byte(capabilities), &v.Capabilities); err != nil {
		return store.Session{}, fmt.Errorf("decode session capabilities: %w", err)
	}
	if v.Capabilities == nil {
		v.Capabilities = []string{}
	}
	var err error
	if v.CreatedAt, err = parseTimestamp(created); err != nil {
		return store.Session{}, err
	}
	if v.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return store.Session{}, err
	}
	return v, nil
}
func scanSessionEvent(row scanner, fingerprint *string) (store.SessionEvent, error) {
	var v store.SessionEvent
	var payload, occurred, recorded string
	fields := []any{&v.EventID, &v.SessionID, &v.Sequence, &v.Cursor, &v.EventType, &payload, &occurred, &recorded}
	if fingerprint != nil {
		fields = append([]any{fingerprint}, fields...)
	}
	if err := row.Scan(fields...); err != nil {
		return store.SessionEvent{}, err
	}
	v.Payload = json.RawMessage(payload)
	var err error
	if v.OccurredAt, err = parseTimestamp(occurred); err != nil {
		return store.SessionEvent{}, err
	}
	if v.RecordedAt, err = parseTimestamp(recorded); err != nil {
		return store.SessionEvent{}, err
	}
	return v, nil
}
