package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Fabric is the adapter's small view of the runtime-neutral HTTP contract.
type Fabric interface {
	Register(context.Context, string, string, time.Duration) (Lease, error)
	Renew(context.Context, string, string, time.Duration) (Lease, error)
	Adopt(context.Context, AdoptRequest) (Session, error)
	Update(context.Context, UpdateRequest) (Session, error)
	Resolve(context.Context, string) (Binding, error)
	Bind(context.Context, string, BindRequest) (Binding, error)
	Append(context.Context, string, AppendRequest) error
}

type Lease struct {
	AdapterID  string    `json:"adapter_id"`
	InstanceID string    `json:"instance_id"`
	LeaseToken string    `json:"lease_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Session struct {
	SessionID    string   `json:"session_id"`
	AdapterID    string   `json:"adapter_id"`
	Label        string   `json:"label"`
	Location     string   `json:"location"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
	Revision     int64    `json:"revision"`
}

type Binding struct {
	Address      string   `json:"address"`
	Bound        bool     `json:"bound"`
	AdapterID    string   `json:"adapter_id"`
	TargetRef    string   `json:"target_ref"`
	Revision     int64    `json:"revision"`
	Generation   int64    `json:"generation"`
	Capabilities []string `json:"capabilities"`
}

type AdoptRequest struct {
	AdapterID    string   `json:"adapter_id"`
	LeaseToken   string   `json:"lease_token"`
	AdapterKey   string   `json:"adapter_key"`
	Label        string   `json:"label"`
	Location     string   `json:"location,omitempty"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

type UpdateRequest struct {
	SessionID        string   `json:"-"`
	AdapterID        string   `json:"adapter_id"`
	LeaseToken       string   `json:"lease_token"`
	ExpectedRevision int64    `json:"expected_revision"`
	Label            string   `json:"label"`
	Location         string   `json:"location,omitempty"`
	Status           string   `json:"status"`
	Capabilities     []string `json:"capabilities"`
}

type BindRequest struct {
	ActorAdapterID   string   `json:"actor_adapter_id"`
	LeaseToken       string   `json:"lease_token"`
	AdapterID        string   `json:"adapter_id"`
	TargetRef        string   `json:"target_ref"`
	Capabilities     []string `json:"capabilities"`
	ExpectedRevision *int64   `json:"expected_revision,omitempty"`
}

type AppendRequest struct {
	AdapterID        string          `json:"adapter_id"`
	LeaseToken       string          `json:"lease_token"`
	ExpectedRevision int64           `json:"expected_revision"`
	OperationID      string          `json:"operation_id"`
	EventType        string          `json:"event_type"`
	Payload          json.RawMessage `json:"payload"`
}

// HTTPFabric uses only the public loopback boundary. Native identifiers remain
// in adapter_key and never become a public binding target.
type HTTPFabric struct {
	base   *url.URL
	client *http.Client
}

func NewHTTPFabric(baseURL string) (*HTTPFabric, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid fabric URL %q", baseURL)
	}
	return &HTTPFabric{base: base, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (h *HTTPFabric) Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (Lease, error) {
	var lease Lease
	err := h.call(ctx, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": adapterID, "instance_id": instanceID, "lease_duration": duration.String()}, &lease)
	return lease, err
}
func (h *HTTPFabric) Renew(ctx context.Context, adapterID, token string, duration time.Duration) (Lease, error) {
	var lease Lease
	err := h.call(ctx, http.MethodPost, "/v1/adapters/renew", map[string]any{"adapter_id": adapterID, "lease_token": token, "lease_duration": duration.String()}, &lease)
	return lease, err
}
func (h *HTTPFabric) Adopt(ctx context.Context, request AdoptRequest) (Session, error) {
	var session Session
	err := h.call(ctx, http.MethodPost, "/v1/sessions/adopt", request, &session)
	return session, err
}
func (h *HTTPFabric) Update(ctx context.Context, request UpdateRequest) (Session, error) {
	var session Session
	err := h.call(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(request.SessionID), request, &session)
	return session, err
}
func (h *HTTPFabric) Resolve(ctx context.Context, address string) (Binding, error) {
	var binding Binding
	err := h.call(ctx, http.MethodGet, "/v1/addresses/"+url.PathEscape(address), nil, &binding)
	return binding, err
}
func (h *HTTPFabric) Bind(ctx context.Context, address string, request BindRequest) (Binding, error) {
	var binding Binding
	err := h.call(ctx, http.MethodPut, "/v1/addresses/"+url.PathEscape(address)+"/binding", request, &binding)
	return binding, err
}
func (h *HTTPFabric) Append(ctx context.Context, sessionID string, request AppendRequest) error {
	return h.call(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/events", request, nil)
}

func (h *HTTPFabric) call(ctx context.Context, method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	u := *h.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
		return &HTTPError{Status: response.StatusCode, Body: string(data)}
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("fabric HTTP %d: %s", e.Status, e.Body) }
