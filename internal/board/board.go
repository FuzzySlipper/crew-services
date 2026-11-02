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
}

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
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Author    Author    `json:"author"`
	Site      string    `json:"site"`
	State     string    `json:"state"`
	Revision  int       `json:"revision"`
	Body      string    `json:"body,omitempty"`
	Mentions  []Mention `json:"mentions,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EditedAt  time.Time `json:"edited_at,omitempty"`
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
