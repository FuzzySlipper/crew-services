package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrSeatHeld        = errors.New("seat is held")
	ErrSeatVacant      = errors.New("seat is vacant")
	ErrNotHolder       = errors.New("session does not hold the seat")
	ErrSeatNotFound    = errors.New("seat not found")
	ErrSeatRetired     = errors.New("seat is retired")
	ErrVersionConflict = errors.New("seat text version conflict")
	ErrVersionPruned   = errors.New("seat text version was pruned")
	ErrVersionNotFound = errors.New("seat text version was never issued")
)

// SeatHeldError is ErrSeatHeld with the holding that is in the way.
type SeatHeldError struct{ Holding Holding }

func (e *SeatHeldError) Error() string {
	return fmt.Sprintf("%s by %s", ErrSeatHeld, e.Holding.HolderAddress)
}
func (e *SeatHeldError) Is(target error) bool { return target == ErrSeatHeld }

// VersionConflictError is ErrVersionConflict with the version now current.
type VersionConflictError struct{ Current int64 }

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("%s: current version is %d", ErrVersionConflict, e.Current)
}
func (e *VersionConflictError) Is(target error) bool { return target == ErrVersionConflict }

// Seat text kinds. A seat has exactly one of each.
const (
	SeatTextCharter = "charter"
	SeatTextHandoff = "handoff"
)

// Seat states as read models report them. Stale is computed on read and is
// never stored: a stale holding stays held until someone relieves it.
const (
	SeatVacant  = "vacant"
	SeatHeld    = "held"
	SeatStale   = "stale"
	SeatRetired = "retired"
)

// Holding end reasons.
const (
	HoldingLeft               = "left"
	HoldingLeftWithoutHandoff = "left_without_handoff"
	HoldingRelieved           = "relieved"
	HoldingRetired            = "retired"
)

// SeatNoticeSender is the service-owned origin of relief and charter-change
// notices. It is never an agent address and has no binding.
const SeatNoticeSender = "fabric/seats"

// IsSeatAddress reports whether address has the seat form scope.name, for
// example rusty-engine.reviewer. It checks the shape only; whether the seat
// exists is a store question. Seats created before 2026-11-02 used name@scope;
// those that remain are retired and stay readable by their exact address.
func IsSeatAddress(address string) bool {
	scope, name, ok := strings.Cut(address, ".")
	return ok && seatPart(scope) && seatPart(name)
}

// SeatScope returns the scope part of a seat address, or "" when address is
// not one.
func SeatScope(address string) string {
	if !IsSeatAddress(address) {
		return ""
	}
	scope, _, _ := strings.Cut(address, ".")
	return scope
}

func seatPart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Holding is one session's time in one seat. It is never edited after it ends.
type Holding struct {
	SeatAddress      string     `json:"seat_address"`
	HoldingID        int64      `json:"holding_id"`
	SeatGeneration   int64      `json:"seat_generation"`
	HolderAddress    string     `json:"holder_address"`
	HolderAdapterID  string     `json:"holder_adapter_id"`
	HolderGeneration int64      `json:"holder_generation"`
	StartedAt        time.Time  `json:"started_at"`
	StartNote        string     `json:"start_note,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	EndReason        string     `json:"end_reason,omitempty"`
	EndNote          string     `json:"end_note,omitempty"`
	RelievedBy       int64      `json:"relieved_by,omitempty"`
	// Stale and StaleReason are computed on read for the current holding.
	Stale       bool       `json:"stale,omitempty"`
	StaleReason string     `json:"stale_reason,omitempty"`
	StaleSince  *time.Time `json:"stale_since,omitempty"`
}

// SeatText is one version of a seat's charter or handoff.
type SeatText struct {
	SeatAddress    string    `json:"seat_address"`
	Kind           string    `json:"kind"`
	Version        int64     `json:"version"`
	Body           string    `json:"body,omitempty"`
	Length         int       `json:"length"`
	SetAt          time.Time `json:"set_at"`
	SetByAddress   string    `json:"set_by_address"`
	SetByAdapterID string    `json:"set_by_adapter_id"`
	SetBySeats     []string  `json:"set_by_seats"`
	HoldingID      int64     `json:"holding_id,omitempty"`
}

// Seat is the read model of one seat. Listings carry only the first line of
// the charter; GetSeat fills Charter and Handoff in full.
type Seat struct {
	Address        string    `json:"address"`
	Generation     int64     `json:"generation"`
	State          string    `json:"state"`
	Holding        *Holding  `json:"holding,omitempty"`
	CharterVersion int64     `json:"charter_version"`
	CharterSummary string    `json:"charter_summary,omitempty"`
	HandoffVersion int64     `json:"handoff_version"`
	QueuedMail     int       `json:"queued_mail"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
	Charter        *SeatText `json:"charter,omitempty"`
	Handoff        *SeatText `json:"handoff,omitempty"`
}

type SeatFilter struct {
	Scope  string
	Holder string
	State  string
}

// SeatOperationRequest is every seat write. The acting session must be bound
// to the calling adapter, as a message sender must.
type SeatOperationRequest struct {
	Kind            string // take, leave, relieve, set_text, retire
	AdapterID       string
	LeaseToken      string
	OperationID     string
	Fingerprint     string
	SessionAddress  string
	SeatAddress     string
	Note            string
	TextKind        string
	Body            string
	ExpectedVersion *int64
	// KeepVersions bounds the text versions kept per seat and kind.
	KeepVersions int
	// Notice IDs are service-generated and used only if the operation sends
	// a relief or charter-change notice.
	NoticeMessageID  string
	NoticeDeliveryID string
	NoticeExpiresAt  time.Time
}

// SeatOperationResult is what a seat write returns and what its receipt
// replays.
type SeatOperationResult struct {
	Seat            Seat      `json:"seat"`
	Holding         *Holding  `json:"holding,omitempty"`
	PreviousHolding *Holding  `json:"previous_holding,omitempty"`
	Text            *SeatText `json:"text,omitempty"`
	NoticeMessageID string    `json:"notice_message_id,omitempty"`
	Replayed        bool      `json:"replayed"`
}

type SeatOperationLookup struct {
	Found       bool
	Fingerprint string
	Result      SeatOperationResult
}

// ReliefNoticeBody is the text of the notice a relieved holder receives.
func ReliefNoticeBody(seat string, by Holding, note string) string {
	text := fmt.Sprintf("You no longer hold the seat %s: %s (%s) relieved you. Messages you send as %s will be refused. Its charter, handoff and holdings stay readable with `seat show %s`.", seat, by.HolderAddress, by.HolderAdapterID, seat, seat)
	if note != "" {
		text += "\nTheir note: " + note
	}
	return text
}

// CharterNoticeBody is the text of the notice a holder receives when another
// session sets its seat's charter.
func CharterNoticeBody(seat string, text SeatText) string {
	by := text.SetByAddress + " (" + text.SetByAdapterID + ")"
	if len(text.SetBySeats) > 0 {
		by += ", holding " + strings.Join(text.SetBySeats, ", ")
	}
	return fmt.Sprintf("The charter of your seat %s was changed by %s (version %d). Read it with `seat show %s`.", seat, by, text.Version, seat)
}
