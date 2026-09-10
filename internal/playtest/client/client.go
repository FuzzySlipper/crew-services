// Package client provides the small local HTTP boundary used by playtest CLI
// and MCP callers. It deliberately keeps session and game semantics in the
// playtest service.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultURL is the loopback address of the playtest service.
	DefaultURL       = "http://127.0.0.1:48200"
	maxResponseBytes = 16 << 20
)

// Request is the stable, intentionally small playtest command envelope.
// Data and Steps are forwarded as JSON so the client does not reinterpret
// service-owned input formats.
type Request struct {
	Op        string          `json:"op"`
	SessionID string          `json:"session_id,omitempty"`
	Game      string          `json:"game,omitempty"`
	Source    string          `json:"source,omitempty"`
	ScriptID  string          `json:"script_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Steps     json.RawMessage `json:"steps,omitempty"`
	BudgetMS  *int            `json:"budget_ms,omitempty"`
}

// Response is the service command envelope. Result remains raw JSON so each
// service operation can evolve its own result without a duplicated client API.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

// CommandError reports a service-declared command failure.
type CommandError struct {
	Message string
}

func (e *CommandError) Error() string { return e.Message }

// Client calls a loopback playtest service.
type Client struct {
	commandURL string
	http       *http.Client
}

// New validates a loopback HTTP service base URL.
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse playtest URL: %w", err)
	}
	if u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("PLAYTEST_URL must be an absolute loopback http URL without credentials, query, or fragment")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("PLAYTEST_URL must use a loopback host")
	}
	if httpClient == nil {
		// Game launch is bounded by the service at 90 seconds. Keep a small
		// client margin while request contexts can still cancel immediately.
		httpClient = &http.Client{Timeout: 120 * time.Second}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/command"
	u.RawPath = ""
	return &Client{commandURL: u.String(), http: httpClient}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Call sends one command and preserves the service response envelope.
func (c *Client) Call(ctx context.Context, command Request) (Response, error) {
	if c == nil || c.http == nil || c.commandURL == "" {
		return Response{}, errors.New("playtest client is not configured")
	}
	if strings.TrimSpace(command.Op) == "" {
		return Response{}, errors.New("playtest command op is required")
	}
	body, err := json.Marshal(command)
	if err != nil {
		return Response{}, fmt.Errorf("encode playtest command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.commandURL, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build playtest command: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("call playtest command: %w", err)
	}
	defer resp.Body.Close()
	reader := io.LimitReader(resp.Body, maxResponseBytes+1)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return Response{}, fmt.Errorf("read playtest command response: %w", err)
	}
	if len(payload) > maxResponseBytes {
		return Response{}, fmt.Errorf("playtest command response exceeded %d bytes", maxResponseBytes)
	}
	var result Response
	if err := json.Unmarshal(payload, &result); err != nil {
		return Response{}, fmt.Errorf("decode playtest command response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if result.Error != "" {
			return result, &CommandError{Message: result.Error}
		}
		return result, fmt.Errorf("playtest command returned HTTP %d", resp.StatusCode)
	}
	if !result.OK {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "playtest command failed"
		}
		return result, &CommandError{Message: message}
	}
	return result, nil
}

// Result sends a command and returns only its service-owned JSON result.
func (c *Client) Result(ctx context.Context, command Request) (json.RawMessage, error) {
	response, err := c.Call(ctx, command)
	if err != nil {
		return nil, err
	}
	if len(response.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return response.Result, nil
}

// Health returns the raw successful health response. It exists for local
// diagnostics without adding health as a playtest command operation.
func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	if c == nil || c.http == nil || c.commandURL == "" {
		return nil, errors.New("playtest client is not configured")
	}
	healthURL := strings.TrimSuffix(c.commandURL, "/command") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build playtest health request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call playtest health: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read playtest health response: %w", err)
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("playtest health response exceeded %d bytes", maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("playtest health returned HTTP %d", resp.StatusCode)
	}
	return payload, nil
}

// NormalizeSteps accepts either the service's steps array or an object with a
// steps member. The latter keeps CLI JSON ergonomic while preserving the one
// wire field consumed by the service.
func NormalizeSteps(value json.RawMessage) (json.RawMessage, error) {
	value = json.RawMessage(bytes.TrimSpace(value))
	if !json.Valid(value) || len(value) == 0 {
		return nil, errors.New("steps must be valid JSON")
	}
	if value[0] == '[' {
		return value, nil
	}
	if value[0] != '{' {
		return nil, errors.New("steps must be an array or an object with a steps array")
	}
	var envelope struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(value, &envelope); err != nil || len(envelope.Steps) == 0 {
		return nil, errors.New("steps object must contain a steps array")
	}
	envelope.Steps = json.RawMessage(bytes.TrimSpace(envelope.Steps))
	if !json.Valid(envelope.Steps) || len(envelope.Steps) == 0 || envelope.Steps[0] != '[' {
		return nil, errors.New("steps object must contain a steps array")
	}
	return envelope.Steps, nil
}
