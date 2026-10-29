// Package claudedriver delivers crew fabric messages into Claude Code sessions
// through each session's own inbox socket. It never starts, stops or steers a
// Claude Code process beyond posting a message the session reads itself.
package claudedriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Claude Code inbox priorities. "next" is read between tool calls of an
// active turn (or starts a turn when idle); "later" waits for the current turn
// to end. "now" is never used: it is meant to preempt.
const (
	PriorityNext  = "next"
	PriorityLater = "later"
)

// ErrInboxGone means nothing was sent because the session's socket is not
// accepting connections: the session has exited or is restarting.
var ErrInboxGone = errors.New("claude session inbox is not accepting connections")

// Inbox posts one message to a session's inbox socket.
type Inbox interface {
	// Dial opens a connection, or returns ErrInboxGone when nothing listens.
	Dial(ctx context.Context, socket, token string) (Conn, error)
}

// Conn is an open, authenticated inbox connection.
type Conn interface {
	// Post writes one message frame. An error here leaves it unknown whether
	// the session received the message.
	Post(msgID, text, priority string) error
	Close() error
}

// SocketInbox speaks the Claude Code 2.1.x inbox line protocol: an optional
// {"type":"auth"} line, then one JSON frame per message. The session answers
// nothing on the connection.
type SocketInbox struct {
	DialTimeout time.Duration
}

func (s SocketInbox) Dial(ctx context.Context, socket, token string) (Conn, error) {
	timeout := s.DialTimeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInboxGone, err)
	}
	conn := &socketConn{conn: raw}
	if token != "" {
		if err := conn.writeLine(map[string]string{"type": "auth", "token": token}); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("%w: auth: %v", ErrInboxGone, err)
		}
	}
	return conn, nil
}

type socketConn struct {
	conn net.Conn
}

func (c *socketConn) Post(msgID, text, priority string) error {
	return c.writeLine(map[string]any{
		"msgV":     1,
		"msg_id":   msgID,
		"type":     "user",
		"message":  map[string]string{"role": "user", "content": text},
		"priority": priority,
	})
}

func (c *socketConn) Close() error { return c.conn.Close() }

func (c *socketConn) writeLine(value any) error {
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// The session closes connections that send no complete line within 30s.
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.conn.Write(append(line, '\n'))
	return err
}
