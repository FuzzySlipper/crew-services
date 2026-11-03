// Package humans is crew-humans: the people on a site and how each is
// notified (docs/crew-humans.md). Other services ask it to notify a person;
// it records the notice in that person's inbox and delivers it over the
// person's routes. The fabric and crew-board know nothing of its routes.
package humans

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Person is someone on this site, from the people file.
type Person struct {
	Name    string        `json:"name"`
	Display string        `json:"display"`
	Routes  []RouteConfig `json:"routes"`
}

// Route kinds.
const (
	RouteInbox         = "inbox"
	RouteHomeAssistant = "home-assistant"
)

// What a push route sends.
const (
	PushMentions = "mentions" // everything but subscription notices, and anything urgent
	PushAll      = "all"
	PushUrgent   = "urgent"
)

// RouteConfig is one of a person's routes. Secrets live in TokenFile, never
// in the people file, and are never served.
type RouteConfig struct {
	Kind      string `json:"kind"`
	URL       string `json:"url,omitempty"`
	Service   string `json:"service,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	Push      string `json:"push,omitempty"`
}

// Public is the route as reads show it: no token file.
func (r RouteConfig) Public() RouteConfig {
	r.TokenFile = ""
	return r
}

// pushes reports whether this route sends n.
func (r RouteConfig) pushes(n Notice) bool {
	switch r.Push {
	case PushAll:
		return true
	case PushUrgent:
		return n.Urgent
	}
	return n.Urgent || n.Reason != ReasonSubscription
}

// ReasonSubscription is the one reason crew-humans treats specially: a
// notice about something the person follows, which a "mentions" route keeps
// in the inbox only.
const ReasonSubscription = "subscription"

// NotifyRequest is the one write other services make.
type NotifyRequest struct {
	Person         string `json:"person"`
	Source         string `json:"source"`
	IdempotencyKey string `json:"idempotency_key"`
	Title          string `json:"title"`
	Text           string `json:"text,omitempty"`
	Link           string `json:"link,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// Group collects notices about the same thing, so a phone shows the
	// newest one in place of the older ones (crew-board sends the thread).
	Group  string `json:"group,omitempty"`
	Urgent bool   `json:"urgent,omitempty"`
}

// Outcomes of a notify.
const (
	OutcomeAccepted = "accepted" // recorded and queued for at least one push route
	OutcomeRecorded = "recorded" // recorded in the inbox only
)

// NotifyResult is what a notify answers, at once; delivery comes after.
type NotifyResult struct {
	NoticeID string   `json:"notice_id"`
	Outcome  string   `json:"outcome"`
	Routes   []string `json:"routes"`
}

// Notice is one notice in a person's inbox, with how each route fared.
type Notice struct {
	ID             string     `json:"id"`
	Person         string     `json:"person"`
	Source         string     `json:"source"`
	IdempotencyKey string     `json:"idempotency_key"`
	Title          string     `json:"title"`
	Text           string     `json:"text,omitempty"`
	Link           string     `json:"link,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	Group          string     `json:"group,omitempty"`
	Urgent         bool       `json:"urgent,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
	Deliveries     []Delivery `json:"deliveries"`
}

// Delivery states.
const (
	DeliveryPending = "pending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

// Delivery is one push route's attempt at a notice. The inbox has none: a
// notice is in the inbox once it is recorded.
type Delivery struct {
	NoticeID      string     `json:"-"`
	Route         int        `json:"route"`
	Kind          string     `json:"kind"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt time.Time  `json:"-"`
	GiveUpAt      time.Time  `json:"-"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

// ErrorCode is a stable reason code.
type ErrorCode string

const (
	CodeInvalid       ErrorCode = "invalid_request"
	CodeUnknownPerson ErrorCode = "unknown_person"
	CodeNotFound      ErrorCode = "notice_not_found"
	CodeConflict      ErrorCode = "idempotency_conflict"
)

// Error is a refusal with a reason code.
type Error struct {
	Code ErrorCode
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Code, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func refuse(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf returns err's reason code, or "".
func CodeOf(err error) ErrorCode {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

// ErrNotFound is what a store returns for a record that does not exist.
var ErrNotFound = errors.New("not found")

// newID is a notice ID that sorts by creation order within this process:
// "hn-", milliseconds and a sequence for notices made in the same
// millisecond, in hex, then random hex so IDs never collide across restarts.
func newID(now time.Time) string {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("humans: no randomness: " + err.Error())
	}
	idMu.Lock()
	ms := now.UnixMilli()
	if ms <= idLast {
		ms, idSeq = idLast, idSeq+1
	} else {
		idSeq = 0
	}
	idLast = ms
	seq := idSeq
	idMu.Unlock()
	return fmt.Sprintf("hn-%012x%04x%s", ms, seq&0xffff, hex.EncodeToString(random[:]))
}

var (
	idMu   sync.Mutex
	idLast int64
	idSeq  int
)
