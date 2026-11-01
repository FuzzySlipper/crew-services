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

// Seat error codes (docs/seats.md).
const (
	CodeSeatHeld           ErrorCode = "seat_held"
	CodeSeatVacant         ErrorCode = "seat_vacant"
	CodeNotHolder          ErrorCode = "not_holder"
	CodeSeatNotFound       ErrorCode = "seat_not_found"
	CodeSeatRetired        ErrorCode = "seat_retired"
	CodeInvalidSeatAddress ErrorCode = "invalid_seat_address"
	CodeVersionConflict    ErrorCode = "version_conflict"
	CodeVersionPruned      ErrorCode = "version_pruned"
	CodeVersionNotFound    ErrorCode = "version_not_found"
)

type Seat = store.Seat
type Holding = store.Holding
type SeatText = store.SeatText
type SeatOperationResult = store.SeatOperationResult

// seatNoticeTTL is how long a relief or charter-change notice waits for its
// recipient session.
const seatNoticeTTL = 24 * time.Hour

func WithSeatTextVersions(value int) Option {
	return func(s *Service) error {
		if value <= 0 {
			return errors.New("seat text versions must be positive")
		}
		s.seatTextVersions = value
		return nil
	}
}

// SeatRequest is a seat write made by a session through its adapter.
type SeatRequest struct {
	AdapterID      string
	LeaseToken     string
	OperationID    string
	SessionAddress string
	SeatAddress    string
	Note           string
}

// SetSeatTextRequest replaces a seat's charter or handoff with Body.
type SetSeatTextRequest struct {
	SeatRequest
	Kind            string
	Body            string
	ExpectedVersion *int64
}

func (s *Service) TakeSeat(ctx context.Context, request SeatRequest) (SeatOperationResult, error) {
	return s.seatOperation(ctx, "take", request, store.SeatOperationRequest{})
}

func (s *Service) LeaveSeat(ctx context.Context, request SeatRequest) (SeatOperationResult, error) {
	return s.seatOperation(ctx, "leave", request, store.SeatOperationRequest{})
}

func (s *Service) RelieveSeat(ctx context.Context, request SeatRequest) (SeatOperationResult, error) {
	return s.seatOperation(ctx, "relieve", request, store.SeatOperationRequest{})
}

func (s *Service) RetireSeat(ctx context.Context, request SeatRequest) (SeatOperationResult, error) {
	return s.seatOperation(ctx, "retire", request, store.SeatOperationRequest{})
}

// SetSeatText writes a new version of a seat's charter or handoff. It always
// replaces the whole text; there is deliberately no append.
func (s *Service) SetSeatText(ctx context.Context, request SetSeatTextRequest) (SeatOperationResult, error) {
	if err := validTextKind(request.Kind); err != nil {
		return SeatOperationResult{}, err
	}
	if request.ExpectedVersion != nil && *request.ExpectedVersion < 0 {
		return SeatOperationResult{}, invalid("expected_version must not be negative")
	}
	return s.seatOperation(ctx, "set_text", request.SeatRequest, store.SeatOperationRequest{TextKind: request.Kind, Body: request.Body, ExpectedVersion: request.ExpectedVersion})
}

func (s *Service) seatOperation(ctx context.Context, kind string, request SeatRequest, extra store.SeatOperationRequest) (SeatOperationResult, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", request.AdapterID}, {"lease_token", request.LeaseToken}, {"operation_id", request.OperationID}, {"session_address", request.SessionAddress}} {
		if err := required(field.name, field.value); err != nil {
			return SeatOperationResult{}, err
		}
	}
	seat, err := seatAddress(request.SeatAddress)
	if err != nil {
		return SeatOperationResult{}, err
	}
	operation := extra
	operation.Kind, operation.AdapterID, operation.LeaseToken = kind, strings.TrimSpace(request.AdapterID), request.LeaseToken
	operation.OperationID, operation.SessionAddress, operation.SeatAddress = strings.TrimSpace(request.OperationID), strings.TrimSpace(request.SessionAddress), seat
	operation.Note = strings.TrimSpace(request.Note)
	operation.Fingerprint, err = seatFingerprint(operation)
	if err != nil {
		return SeatOperationResult{}, fmt.Errorf("fingerprint seat request: %w", err)
	}
	lookup, err := s.store.LookupSeatOperation(ctx, operation.AdapterID, operation.OperationID)
	if err != nil {
		return SeatOperationResult{}, mapStoreError(err)
	}
	if lookup.Found {
		if lookup.Fingerprint != operation.Fingerprint {
			return SeatOperationResult{}, &Error{Code: CodeOperationConflict, Err: store.ErrOperationConflict}
		}
		lookup.Result.Replayed = true
		return lookup.Result, nil
	}
	if operation.NoticeMessageID, err = s.ids(); err != nil {
		return SeatOperationResult{}, fmt.Errorf("generate notice message ID: %w", err)
	}
	if operation.NoticeDeliveryID, err = s.ids(); err != nil {
		return SeatOperationResult{}, fmt.Errorf("generate notice delivery ID: %w", err)
	}
	now := s.clock.Now().UTC()
	operation.NoticeExpiresAt = now.Add(min(seatNoticeTTL, s.maxTTLDuration))
	operation.KeepVersions = s.seatTextVersions
	result, err := s.store.SeatOperation(ctx, now, operation)
	return result, mapStoreError(err)
}

func seatFingerprint(request store.SeatOperationRequest) (string, error) {
	canonical := struct {
		Kind, SessionAddress, SeatAddress, Note, TextKind, Body string
		ExpectedVersion                                         *int64
	}{request.Kind, request.SessionAddress, request.SeatAddress, request.Note, request.TextKind, request.Body, request.ExpectedVersion}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ListSeats is side-effect free. Filters: scope (the part after @), holder (a
// session address) and state (vacant, held, stale, retired).
func (s *Service) ListSeats(ctx context.Context, filter store.SeatFilter) ([]Seat, error) {
	filter.Scope, filter.Holder, filter.State = strings.TrimSpace(filter.Scope), strings.TrimSpace(filter.Holder), strings.TrimSpace(filter.State)
	switch filter.State {
	case "", store.SeatVacant, store.SeatHeld, store.SeatStale, store.SeatRetired:
	default:
		return nil, invalid("state must be vacant, held, stale or retired")
	}
	values, err := s.store.ListSeats(ctx, s.clock.Now().UTC(), filter)
	return values, mapStoreError(err)
}

func (s *Service) GetSeat(ctx context.Context, address string) (Seat, error) {
	seat, err := seatAddress(address)
	if err != nil {
		return Seat{}, err
	}
	value, err := s.store.GetSeat(ctx, s.clock.Now().UTC(), seat)
	return value, mapStoreError(err)
}

// GetSeatText returns one version of a seat text; version 0 is the current one.
func (s *Service) GetSeatText(ctx context.Context, address, kind string, version int64) (SeatText, error) {
	seat, err := seatAddress(address)
	if err != nil {
		return SeatText{}, err
	}
	if err := validTextKind(kind); err != nil {
		return SeatText{}, err
	}
	if version < 0 {
		return SeatText{}, invalid("version must not be negative")
	}
	value, err := s.store.GetSeatText(ctx, seat, kind, version)
	return value, mapStoreError(err)
}

func (s *Service) ListSeatTexts(ctx context.Context, address, kind string) ([]SeatText, error) {
	seat, err := seatAddress(address)
	if err != nil {
		return nil, err
	}
	if err := validTextKind(kind); err != nil {
		return nil, err
	}
	values, err := s.store.ListSeatTexts(ctx, seat, kind)
	return values, mapStoreError(err)
}

func (s *Service) ListSeatHoldings(ctx context.Context, address string) ([]Holding, error) {
	seat, err := seatAddress(address)
	if err != nil {
		return nil, err
	}
	values, err := s.store.ListSeatHoldings(ctx, s.clock.Now().UTC(), seat)
	return values, mapStoreError(err)
}

// PruneSeatOperations removes seat operation receipts older than the
// retention window, as PruneDeliveryOperations does for delivery receipts.
func (s *Service) PruneSeatOperations(ctx context.Context) (int64, error) {
	cutoff := s.clock.Now().UTC().Add(-s.retention)
	var total int64
	for {
		removed, err := s.store.PruneSeatOperations(ctx, cutoff, pruneBatch)
		total += removed
		if err != nil {
			return total, mapStoreError(err)
		}
		if removed < pruneBatch {
			return total, nil
		}
	}
}

func seatAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !store.IsSeatAddress(value) {
		return "", &Error{Code: CodeInvalidSeatAddress, Err: fmt.Errorf("seat address must be name@scope in lowercase letters, digits and hyphens: %q", value)}
	}
	return value, nil
}

func validTextKind(kind string) error {
	if kind != store.SeatTextCharter && kind != store.SeatTextHandoff {
		return invalid("kind must be charter or handoff")
	}
	return nil
}

// seatErrorDetail is the extra body a seat error carries over HTTP: the
// holding in the way, or the current text version.
func seatErrorDetail(err error) any {
	var held *store.SeatHeldError
	if errors.As(err, &held) {
		return map[string]any{"holding": held.Holding}
	}
	var conflict *store.VersionConflictError
	if errors.As(err, &conflict) {
		return map[string]any{"current_version": conflict.Current}
	}
	return nil
}
