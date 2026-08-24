package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/store"
)

type Session = store.Session
type SessionEvent = store.SessionEvent

type AdoptSessionRequest struct {
	AdapterID    string
	LeaseToken   string
	AdapterKey   string
	Label        string
	Location     string
	Status       string
	Capabilities []string
}

type UpdateSessionRequest struct {
	AdapterID        string
	LeaseToken       string
	SessionID        string
	ExpectedRevision int64
	Label            string
	Location         string
	Status           string
	Capabilities     []string
}

type AppendSessionEventRequest struct {
	AdapterID        string
	LeaseToken       string
	SessionID        string
	ExpectedRevision int64
	OperationID      string
	EventType        string
	Payload          json.RawMessage
	OccurredAt       time.Time
}

const defaultReadLimit = 100
const maxReadLimit = 500

func (s *Service) AdoptSession(ctx context.Context, r AdoptSessionRequest) (Session, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", r.AdapterID}, {"lease_token", r.LeaseToken}, {"adapter_key", r.AdapterKey}, {"label", r.Label}, {"status", r.Status}} {
		if err := required(field.name, field.value); err != nil {
			return Session{}, err
		}
	}
	capabilities, err := canonicalCapabilities(r.Capabilities)
	if err != nil {
		return Session{}, err
	}
	id, err := s.ids()
	if err != nil {
		return Session{}, fmt.Errorf("generate session ID: %w", err)
	}
	v, err := s.store.AdoptSession(ctx, s.clock.Now().UTC(), store.AdoptSessionRequest{AdapterID: strings.TrimSpace(r.AdapterID), LeaseToken: strings.TrimSpace(r.LeaseToken), AdapterKey: strings.TrimSpace(r.AdapterKey), SessionID: id, Label: strings.TrimSpace(r.Label), Location: strings.TrimSpace(r.Location), Status: strings.TrimSpace(r.Status), Capabilities: capabilities})
	return v, mapStoreError(err)
}

func (s *Service) UpdateSession(ctx context.Context, r UpdateSessionRequest) (Session, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", r.AdapterID}, {"lease_token", r.LeaseToken}, {"session_id", r.SessionID}, {"label", r.Label}, {"status", r.Status}} {
		if err := required(field.name, field.value); err != nil {
			return Session{}, err
		}
	}
	if r.ExpectedRevision <= 0 {
		return Session{}, &Error{Code: CodeInvalid, Err: errors.New("expected_revision must be positive")}
	}
	capabilities, err := canonicalCapabilities(r.Capabilities)
	if err != nil {
		return Session{}, err
	}
	v, err := s.store.UpdateSession(ctx, s.clock.Now().UTC(), store.UpdateSessionRequest{AdapterID: strings.TrimSpace(r.AdapterID), LeaseToken: strings.TrimSpace(r.LeaseToken), SessionID: strings.TrimSpace(r.SessionID), ExpectedRevision: r.ExpectedRevision, Label: strings.TrimSpace(r.Label), Location: strings.TrimSpace(r.Location), Status: strings.TrimSpace(r.Status), Capabilities: capabilities})
	return v, mapStoreError(err)
}

func (s *Service) GetSession(ctx context.Context, sessionID string) (Session, error) {
	if err := required("session_id", sessionID); err != nil {
		return Session{}, err
	}
	v, err := s.store.GetSession(ctx, strings.TrimSpace(sessionID))
	return v, mapStoreError(err)
}
func (s *Service) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	limit, err := readLimit(limit)
	if err != nil {
		return nil, err
	}
	v, err := s.store.ListSessions(ctx, limit)
	return v, mapStoreError(err)
}
func (s *Service) AppendSessionEvent(ctx context.Context, r AppendSessionEventRequest) (SessionEvent, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", r.AdapterID}, {"lease_token", r.LeaseToken}, {"session_id", r.SessionID}, {"operation_id", r.OperationID}, {"event_type", r.EventType}} {
		if err := required(field.name, field.value); err != nil {
			return SessionEvent{}, err
		}
	}
	if r.ExpectedRevision <= 0 {
		return SessionEvent{}, &Error{Code: CodeInvalid, Err: errors.New("expected_revision must be positive")}
	}
	if !json.Valid(r.Payload) {
		return SessionEvent{}, &Error{Code: CodeInvalid, Err: errors.New("payload must be valid JSON")}
	}
	explicitOccurredAt := r.OccurredAt
	if r.OccurredAt.IsZero() {
		r.OccurredAt = s.clock.Now().UTC()
	}
	fingerprint, err := sessionEventFingerprint(r, explicitOccurredAt)
	if err != nil {
		return SessionEvent{}, fmt.Errorf("fingerprint session event: %w", err)
	}
	lookup, err := s.store.LookupSessionEventOperation(ctx, strings.TrimSpace(r.AdapterID), strings.TrimSpace(r.OperationID))
	if err != nil {
		return SessionEvent{}, mapStoreError(err)
	}
	if lookup.Found {
		if lookup.Fingerprint != fingerprint {
			return SessionEvent{}, &Error{Code: CodeOperationConflict, Err: store.ErrOperationConflict}
		}
		return lookup.Event, nil
	}
	id, err := s.ids()
	if err != nil {
		return SessionEvent{}, fmt.Errorf("generate session event ID: %w", err)
	}
	v, err := s.store.AppendSessionEvent(ctx, s.clock.Now().UTC(), store.AppendSessionEventRequest{AdapterID: strings.TrimSpace(r.AdapterID), LeaseToken: strings.TrimSpace(r.LeaseToken), SessionID: strings.TrimSpace(r.SessionID), ExpectedRevision: r.ExpectedRevision, OperationID: strings.TrimSpace(r.OperationID), Fingerprint: fingerprint, EventID: id, EventType: strings.TrimSpace(r.EventType), Payload: append(json.RawMessage(nil), r.Payload...), OccurredAt: r.OccurredAt.UTC()})
	return v, mapStoreError(err)
}
func (s *Service) ListSessionEvents(ctx context.Context, sessionID string, cursor int64, limit int) ([]SessionEvent, error) {
	if cursor < 0 {
		return nil, &Error{Code: CodeInvalid, Err: errors.New("cursor must not be negative")}
	}
	limit, err := readLimit(limit)
	if err != nil {
		return nil, err
	}
	v, err := s.store.ListSessionEvents(ctx, strings.TrimSpace(sessionID), cursor, limit)
	return v, mapStoreError(err)
}
func readLimit(value int) (int, error) {
	if value == 0 {
		return defaultReadLimit, nil
	}
	if value < 0 || value > maxReadLimit {
		return 0, &Error{Code: CodeInvalid, Err: fmt.Errorf("limit must be between 1 and %d", maxReadLimit)}
	}
	return value, nil
}
func sessionEventFingerprint(r AppendSessionEventRequest, explicitOccurredAt time.Time) (string, error) {
	v := struct {
		SessionID  string
		EventType  string
		Payload    json.RawMessage
		OccurredAt string
	}{strings.TrimSpace(r.SessionID), strings.TrimSpace(r.EventType), r.Payload, explicitOccurredAt.UTC().Format(time.RFC3339Nano)}
	if explicitOccurredAt.IsZero() {
		v.OccurredAt = ""
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
