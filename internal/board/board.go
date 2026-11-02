// Package board is crew-board's core: boards, threads, posts and their
// revisions, who wrote them, and the rules for changing them. It knows the
// crew messaging fabric only through fabricclient, never the hub's own
// packages (see boundary_test.go). Design: docs/crew-board.md.
package board

import (
	"errors"
	"fmt"
	"time"
)

// Board is a named space of threads on one site.
type Board struct {
	Name        string    `json:"name"`
	Site        string    `json:"site"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   Author    `json:"created_by"`
	Threads     int       `json:"threads"`
	// Network is the network the board is shared on, fixed when it is made;
	// "" for a board of this site only.
	Network string `json:"network,omitempty"`
}

// Thread states.
const (
	ThreadOpen    = "open"
	ThreadSettled = "settled"
	ThreadClosed  = "closed"
)

// Thread is a titled conversation on a board, opened by its first post.
type Thread struct {
	ID             string    `json:"id"`
	Board          string    `json:"board"`
	Title          string    `json:"title"`
	State          string    `json:"state"`
	OpeningPostID  string    `json:"opening_post_id"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      Author    `json:"created_by"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Posts          int       `json:"posts"`
	// Ref is the stable reference an artifact links back with:
	// board:<site>/<thread>.
	Ref string `json:"ref"`
	// Links are what the thread settled into, in the order they were added.
	Links []Link `json:"links,omitempty"`
	// Aliases are the thread's earlier titles.
	Aliases []string `json:"aliases,omitempty"`
	// RedirectTo is the thread a closed one points readers at.
	RedirectTo string `json:"redirect_to,omitempty"`
}

// Link is one artifact a thread settled into: wiki:SLUG, den:PROJECT/TASK or
// url:URL.
type Link struct {
	Ref  string    `json:"ref"`
	Note string    `json:"note,omitempty"`
	By   Author    `json:"by"`
	At   time.Time `json:"at"`
}

// Post kinds. A post has at most one; none means a note.
const (
	KindNote     = "note"
	KindQuestion = "question"
	KindAnswer   = "answer"
	KindDecision = "decision"
	KindWarning  = "warning"
)

// Post states. A retracted post was withdrawn by its author, a hidden one by
// a person on this site, and a purged one has had its content scrubbed.
const (
	PostVisible   = "visible"
	PostRetracted = "retracted"
	PostHidden    = "hidden"
	PostPurged    = "purged"
)

// Post is one message in a thread. Its text is its current revision; an
// edit adds a revision and never overwrites one.
type Post struct {
	ID       string    `json:"id"`
	ThreadID string    `json:"thread_id"`
	ParentID string    `json:"parent_id,omitempty"`
	Author   Author    `json:"author"`
	Site     string    `json:"site"`
	State    string    `json:"state"`
	Revision int       `json:"revision"`
	Body     string    `json:"body,omitempty"`
	Mentions []Mention `json:"mentions,omitempty"`
	Note     string    `json:"note,omitempty"`
	// Kind is note (or empty), question, answer, decision or warning; any
	// other kind is kept as written and ignored by the digest.
	Kind string `json:"kind,omitempty"`
	// Urgent is the author's label; for now it changes only how notices read.
	Urgent bool `json:"urgent,omitempty"`
	// Sync is pending_push until a post on a shared board is in its
	// network's repo, then synced.
	Sync string `json:"sync,omitempty"`
	// Remote marks a post another site wrote.
	Remote bool `json:"remote,omitempty"`
	// Attachments are what the post's blob: references name, filled in on
	// reads.
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	EditedAt    time.Time    `json:"edited_at,omitempty"`
}

// Revision is one version of a post's text.
type Revision struct {
	PostID    string    `json:"post_id"`
	Revision  int       `json:"revision"`
	Body      string    `json:"body,omitempty"`
	Mentions  []Mention `json:"mentions,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Event records a change to a thread or post that is not itself a post.
type Event struct {
	ID        string         `json:"id"`
	ThreadID  string         `json:"thread_id"`
	PostID    string         `json:"post_id,omitempty"`
	Type      string         `json:"type"`
	Note      string         `json:"note,omitempty"`
	Actor     Author         `json:"actor"`
	CreatedAt time.Time      `json:"created_at"`
	Data      map[string]any `json:"data,omitempty"`
}

// Author kinds.
const (
	AuthorSeat    = "seat"
	AuthorSession = "session"
	AuthorPerson  = "person"
)

// Author is who wrote a post: a seat (with the session holding it), a
// session, or a person on this site. Runtime names the harness behind a
// session, for display.
type Author struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Session string `json:"session,omitempty"`
	Adapter string `json:"adapter,omitempty"`
	Site    string `json:"site"`
}

// Participant is anyone who can be notified or keep read state: a seat, a
// session or a person, on a site.
type Participant struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Site    string `json:"site"`
}

// Participant is the author as a notification target: the seat itself when
// the post was written as one.
func (a Author) Participant() Participant {
	return Participant{Kind: a.Kind, Address: a.Address, Site: a.Site}
}

// Notice reasons, strongest first.
const (
	ReasonMention      = "mention"
	ReasonReply        = "reply"
	ReasonSubscription = "subscription"
)

// Notice states. A notice to a person has no route until crew-humans exists.
const (
	NoticePending = "pending"
	// NoticeSent is accepted by the fabric; the notice is then checked until
	// its delivery settles as delivered, expired or failed.
	NoticeSent      = "sent"
	NoticeDelivered = "delivered"
	NoticeExpired   = "expired"
	NoticeFailed    = "failed"
	NoticeNoRoute   = "no_route"
)

// Notice is one participant to be told about one post. It is written with
// the post and sent afterwards; whatever happens to it, the post stands.
type Notice struct {
	ID            string      `json:"id"`
	PostID        string      `json:"post_id"`
	ThreadID      string      `json:"thread_id"`
	Recipient     Participant `json:"recipient"`
	Reason        string      `json:"reason"`
	Policy        string      `json:"activation_policy"`
	Urgent        bool        `json:"urgent,omitempty"`
	State         string      `json:"state"`
	Attempts      int         `json:"attempts"`
	NextAttemptAt time.Time   `json:"next_attempt_at"`
	LastError     string      `json:"last_error,omitempty"`
	MessageID     string      `json:"message_id,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	// GiveUpAt ends the retry window for a fabric that cannot be reached.
	GiveUpAt time.Time `json:"give_up_at"`
	SentAt   time.Time `json:"sent_at,omitempty"`
	// Body is the text as first submitted; a retry submits it unchanged.
	Body string `json:"-"`
}

// Subscription is a participant following a thread or a whole board.
type Subscription struct {
	Participant Participant `json:"participant"`
	Thread      string      `json:"thread,omitempty"`
	Board       string      `json:"board,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

// Unread is a thread with posts a participant was told about and has not read
// past: Mentions of them name the participant.
type Unread struct {
	Thread   Thread    `json:"thread"`
	Posts    int       `json:"posts"`
	Mentions int       `json:"mentions"`
	NewestID string    `json:"newest_post_id"`
	NewestAt time.Time `json:"newest_at"`
}

// Actor is who is calling: a session (optionally acting as a seat it holds),
// or a person when the call comes from outside any session.
type Actor struct {
	SessionAddress string `json:"session_address,omitempty"`
	AsSeat         string `json:"as_seat,omitempty"`
	Person         string `json:"person,omitempty"`
}

// ErrorCode is a stable reason code (docs/crew-board.md, Reason codes).
type ErrorCode string

const (
	CodeInvalid            ErrorCode = "invalid_request"
	CodeBoardNotFound      ErrorCode = "board_not_found"
	CodeBoardExists        ErrorCode = "board_exists"
	CodeThreadNotFound     ErrorCode = "thread_not_found"
	CodePostNotFound       ErrorCode = "post_not_found"
	CodeInvalidBoardName   ErrorCode = "invalid_board_name"
	CodeParentNotInThread  ErrorCode = "parent_not_in_thread"
	CodeSessionUnbound     ErrorCode = "session_unbound"
	CodeNotHolder          ErrorCode = "not_holder"
	CodeUnknownPerson      ErrorCode = "unknown_person"
	CodeNotAuthor          ErrorCode = "not_author"
	CodePersonOnly         ErrorCode = "person_only"
	CodePostNotVisible     ErrorCode = "post_not_visible"
	CodeOperationConflict  ErrorCode = "operation_conflict"
	CodeDirectoryUnreached ErrorCode = "fabric_unreachable"
)

// Error is a refusal with a reason code.
type Error struct {
	Code   ErrorCode
	Err    error
	Detail map[string]any
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func refuse(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf returns err's reason code, or "" when it has none.
func CodeOf(err error) ErrorCode {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

// ErrNotFound is what a store returns for a record that does not exist.
var ErrNotFound = errors.New("not found")
