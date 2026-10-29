// Package dshdriver is the DeepSeek Harness (DSH) edge of the crew messaging
// fabric. Its driver delivers fabric messages into DSH sessions through the
// web host's session API (`dsh web`), and never starts, stops or restarts DSH
// or a session's agent: the host resumes a cold session itself when it is
// prompted.
package dshdriver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SessionSummary is one row of session/list.
type SessionSummary struct {
	SessionID       string `json:"sessionId"`
	AgentAvailable  bool   `json:"agentAvailable"`
	Running         bool   `json:"running"`
	Blank           bool   `json:"blank"`
	Origin          string `json:"origin,omitempty"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// QueuedItem is one pending inbox item of a loaded session.
type QueuedItem struct {
	ID        string `json:"id"`
	Placement string `json:"placement"`
	RPCID     string `json:"rpcId,omitempty"`
}

// SessionState is session/state. A cold session has Active false and a nil
// Queue.
type SessionState struct {
	Cursor       int64        `json:"cursor"`
	Active       bool         `json:"active"`
	Running      bool         `json:"running"`
	Queue        []QueuedItem `json:"queue"`
	PendingInput []string     `json:"pendingInput"`
}

// Event is one raw session log event from session/events.
type Event struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Data json.RawMessage `json:"data"`
}

// PromptRequest is session/prompt. RequestID is persisted on the user message
// and is what the host deduplicates on.
type PromptRequest struct {
	SessionID string
	RequestID string
	Mode      string // "queue" or "steer"
	Text      string
}

// RemoteError is a definite refusal from the host: an `ok:false` response
// carrying the host's error code. The host refused the call; for a prompt
// that normally means nothing was admitted, but the driver still checks.
type RemoteError struct {
	Method  string
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("dsh %s: %s: %s", e.Method, e.Code, e.Message)
}

// ErrorCode returns the host error code of err, or "".
func ErrorCode(err error) string {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Code
	}
	return ""
}

// notSentError marks a call that never reached the host (the dial failed).
type notSentError struct{ err error }

func (e *notSentError) Error() string { return "dsh web host not reachable: " + e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// Definite reports whether err says for certain the host did not carry out
// the call: it refused it, or the call never reached it. Anything else, such
// as a reset connection or an unreadable response, is ambiguous.
func Definite(err error) bool {
	var remote *RemoteError
	var notSent *notSentError
	return errors.As(err, &remote) || errors.As(err, &notSent)
}

// Timeout reports whether err is the call running out of time. The host may
// still be working on it, so a prompt that timed out is never sent again.
func Timeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Credentials are where the web host listens and its current launch token.
type Credentials struct {
	BaseURL string
	Token   string
}

// TokenSource returns the host's current credentials. The launch token
// changes on every `dsh web` start.
type TokenSource func(ctx context.Context) (Credentials, error)

var launchLine = regexp.MustCompile(`(https?://[^\s?]+)/?\?token=([A-Za-z0-9._~-]+)`)

// ParseLaunchLine finds the last `dsh web: <url>?token=<token>` line in log.
func ParseLaunchLine(log string) (Credentials, bool) {
	matches := launchLine.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return Credentials{}, false
	}
	last := matches[len(matches)-1]
	return Credentials{BaseURL: strings.TrimRight(last[1], "/"), Token: last[2]}, true
}

// JournalTokens reads the launch URL `dsh web` logs at start from the user
// journal of its systemd unit, the same place dsh-crew/get-token.sh reads it.
// baseURL, when set, replaces the logged host.
func JournalTokens(unit, baseURL string) TokenSource {
	return func(ctx context.Context) (Credentials, error) {
		out, err := exec.CommandContext(ctx, "journalctl", "--user", "-u", unit, "-o", "cat", "--no-pager", "-n", "2000").Output()
		if err != nil {
			return Credentials{}, fmt.Errorf("read %s journal: %w", unit, err)
		}
		creds, ok := ParseLaunchLine(string(out))
		if !ok {
			return Credentials{}, fmt.Errorf("no dsh web launch token in the %s journal", unit)
		}
		if baseURL != "" {
			creds.BaseURL = strings.TrimRight(baseURL, "/")
		}
		return creds, nil
	}
}

// WebHost is a client of the DSH web host's session API. It connects as an
// independent API operator with interactions=none, so it is never a party to
// approvals or questions, and reconnects with a fresh launch token when the
// host answers 401 (after a host restart).
type WebHost struct {
	tokens        TokenSource
	http          *http.Client
	callTimeout   time.Duration
	promptTimeout time.Duration

	mu        sync.Mutex
	base      string
	connected bool
}

// NewWebHost returns a client that takes its credentials from tokens.
func NewWebHost(tokens TokenSource) *WebHost {
	jar, _ := cookiejar.New(nil)
	return &WebHost{
		tokens: tokens,
		http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		callTimeout:   15 * time.Second,
		promptTimeout: 60 * time.Second,
	}
}

func (h *WebHost) connect(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connected {
		return h.base, nil
	}
	creds, err := h.tokens(ctx)
	if err != nil {
		return "", &notSentError{err}
	}
	query := url.Values{"token": {creds.Token}, "interactions": {"none"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, creds.BaseURL+"/connect?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return "", &notSentError{err}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", &notSentError{fmt.Errorf("dsh web /connect answered %s (stale launch token?)", resp.Status)}
	}
	h.base, h.connected = creds.BaseURL, true
	return h.base, nil
}

func (h *WebHost) disconnect() {
	h.mu.Lock()
	h.connected = false
	h.mu.Unlock()
}

var errUnauthorized = errors.New("dsh web host answered 401")

// call makes one unary session API call. A 401 is a refusal before the host
// ran anything, so the call is made once more after reconnecting.
func (h *WebHost) call(ctx context.Context, method string, args any, timeout time.Duration, out any) error {
	for attempt := 0; ; attempt++ {
		err := h.callOnce(ctx, method, args, timeout, out)
		if errors.Is(err, errUnauthorized) {
			h.disconnect()
			if attempt == 0 {
				continue
			}
			return &notSentError{err}
		}
		return err
	}
}

func (h *WebHost) callOnce(ctx context.Context, method string, args any, timeout time.Duration, out any) error {
	base, err := h.connect(ctx)
	if err != nil {
		return err
	}
	rpcID := newRPCID()
	body, err := json.Marshal(map[string]any{"type": "client-request", "rpcId": rpcID, "method": method, "payload": map[string]any{"args": args}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			h.disconnect()
			return &notSentError{err}
		}
		return fmt.Errorf("dsh %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("dsh %s: read response: %w", method, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dsh %s: HTTP %s: %s", method, resp.Status, strings.TrimSpace(string(data)))
	}
	var envelope struct {
		Type   string `json:"type"`
		RPCID  string `json:"rpcId"`
		Result struct {
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("dsh %s: unreadable response: %w", method, err)
	}
	if envelope.Type != "server-response" || envelope.RPCID != rpcID {
		return fmt.Errorf("dsh %s: unexpected response %q for rpc %q", method, envelope.Type, envelope.RPCID)
	}
	if !envelope.Result.OK {
		if envelope.Result.Error == nil {
			return fmt.Errorf("dsh %s: ok:false without an error", method)
		}
		return &RemoteError{Method: method, Code: envelope.Result.Error.Code, Message: envelope.Result.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result.Value, out); err != nil {
		return fmt.Errorf("dsh %s: unreadable value: %w", method, err)
	}
	return nil
}

func newRPCID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "crew-dsh-" + hex.EncodeToString(b)
}

// List reads every visible session without resuming any.
func (h *WebHost) List(ctx context.Context) ([]SessionSummary, error) {
	var out struct {
		Items []SessionSummary `json:"items"`
	}
	err := h.call(ctx, "session/list", map[string]any{"_request": map[string]any{}}, h.callTimeout, &out)
	return out.Items, err
}

// State reads a session's passive state without resuming it.
func (h *WebHost) State(ctx context.Context, sessionID string) (SessionState, error) {
	var out SessionState
	err := h.call(ctx, "session/state", map[string]any{"request": map[string]any{"sessionId": sessionID}}, h.callTimeout, &out)
	return out, err
}

// Events reads the end of a session's log (a cold-safe, passive read).
func (h *WebHost) Events(ctx context.Context, sessionID string, maxMessages int) ([]Event, error) {
	var out struct {
		Records []struct {
			Type  string `json:"type"`
			Event Event  `json:"event"`
		} `json:"records"`
	}
	request := map[string]any{"address": map[string]any{"kind": "session", "sessionId": sessionID}, "maxMessages": maxMessages}
	if err := h.call(ctx, "session/events", map[string]any{"request": request}, h.callTimeout, &out); err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(out.Records))
	for _, record := range out.Records {
		if record.Type == "event" {
			events = append(events, record.Event)
		}
	}
	return events, nil
}

// Prompt admits one prompt into a session's inbox, resuming a cold session
// first. It returns nil only for `accepted:true`.
func (h *WebHost) Prompt(ctx context.Context, p PromptRequest) error {
	var out struct {
		Accepted bool `json:"accepted"`
	}
	request := map[string]any{"requestId": p.RequestID, "sessionId": p.SessionID, "mode": p.Mode, "content": []map[string]string{{"type": "text", "text": p.Text}}}
	if err := h.call(ctx, "session/prompt", map[string]any{"request": request}, h.promptTimeout, &out); err != nil {
		return err
	}
	if !out.Accepted {
		return fmt.Errorf("dsh session/prompt: response without accepted:true")
	}
	return nil
}

// HasRequest reports whether a session holds the prompt with requestID: still
// in its inbox (session/state queue[].rpcId), admitted to the inbox
// (agent/inbox/spliced inserted[].source.rpcId), or taken into the
// conversation (user/message source.rpcId) among the recent events.
func HasRequest(state SessionState, events []Event, requestID string) bool {
	for _, item := range state.Queue {
		if item.RPCID == requestID {
			return true
		}
	}
	type source struct {
		Kind  string `json:"kind"`
		RPCID string `json:"rpcId"`
	}
	for _, event := range events {
		switch event.Type {
		case "user/message":
			var data struct {
				Source source `json:"source"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.Source.RPCID == requestID {
				return true
			}
		case "agent/inbox/spliced":
			var data struct {
				Inserted []struct {
					Source source `json:"source"`
				} `json:"inserted"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				for _, item := range data.Inserted {
					if item.Source.RPCID == requestID {
						return true
					}
				}
			}
		}
	}
	return false
}
