// Package fabricclient is a small HTTP client for the crew-messaging loopback
// API, shared by runtime drivers and producers. It knows the fabric's public
// JSON contract only; no runtime concepts live here.
package fabricclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Lease struct {
	AdapterID  string    `json:"adapter_id"`
	InstanceID string    `json:"instance_id"`
	LeaseToken string    `json:"lease_token"`
	ExpiresAt  time.Time `json:"expires_at"`
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

// BindRequest points a named address at a target the adapter drives.
// ExpectedRevision is required to change an existing binding.
type BindRequest struct {
	ActorAdapterID   string   `json:"actor_adapter_id"`
	LeaseToken       string   `json:"lease_token"`
	AdapterID        string   `json:"adapter_id"`
	TargetRef        string   `json:"target_ref"`
	Capabilities     []string `json:"capabilities"`
	ExpectedRevision *int64   `json:"expected_revision,omitempty"`
}

type SessionAddressRequest struct {
	AdapterID    string   `json:"adapter_id"`
	LeaseToken   string   `json:"lease_token"`
	NativeKey    string   `json:"native_key"`
	Capabilities []string `json:"capabilities"`
}

type Message struct {
	MessageID        string `json:"message_id"`
	Body             string `json:"body"`
	SenderAddress    string `json:"sender_address"`
	RecipientAddress string `json:"recipient_address"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	ActivationPolicy string `json:"activation_policy"`
}

type MessageRequest struct {
	ProducerID       string `json:"producer_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	SenderAddress    string `json:"sender_address"`
	RecipientAddress string `json:"recipient_address"`
	Body             string `json:"body"`
	ActivationPolicy string `json:"activation_policy,omitempty"`
	TTL              string `json:"ttl"`
}

type SubmittedMessage struct {
	Message  Message `json:"message"`
	Replayed bool    `json:"replayed"`
}

type Delivery struct {
	DeliveryID          string `json:"delivery_id"`
	MessageID           string `json:"message_id"`
	RecipientAddress    string `json:"recipient_address"`
	RecipientGeneration int64  `json:"recipient_generation"`
	State               string `json:"state"`
	DispatchAction      string `json:"dispatch_action"`
	NativeAttemptRef    string `json:"native_attempt_ref"`
	ClaimOwnerAdapterID string `json:"claim_owner_adapter_id,omitempty"`
	AttemptCount        int    `json:"attempt_count,omitempty"`
}

type Claim struct {
	Claimed  bool      `json:"claimed"`
	Reason   string    `json:"reason,omitempty"`
	Message  *Message  `json:"message,omitempty"`
	Delivery *Delivery `json:"delivery,omitempty"`
	// Head is the address's blocking head delivery when nothing was claimed,
	// for example one still dispatching after a driver crash.
	Head           *Delivery `json:"head,omitempty"`
	ClaimToken     string    `json:"claim_token,omitempty"`
	DispatchAction string    `json:"dispatch_action,omitempty"`
}

type ClaimRequest struct {
	AdapterID           string `json:"adapter_id"`
	LeaseToken          string `json:"lease_token"`
	OperationID         string `json:"operation_id"`
	RecipientAddress    string `json:"recipient_address"`
	RecipientGeneration int64  `json:"recipient_generation"`
	Availability        string `json:"availability"`
	ClaimDuration       string `json:"claim_duration"`
}

type ClaimTokenRequest struct {
	AdapterID   string `json:"adapter_id"`
	LeaseToken  string `json:"lease_token"`
	OperationID string `json:"operation_id"`
	ClaimToken  string `json:"claim_token"`
}

type DispatchRequest struct {
	AdapterID        string `json:"adapter_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	ClaimToken       string `json:"claim_token"`
	NativeAttemptRef string `json:"native_attempt_ref"`
}

// NotInsertedRequest reports that a begun dispatch definitely inserted
// nothing natively, so the delivery goes back to the queue as the address head.
type NotInsertedRequest struct {
	AdapterID        string `json:"adapter_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	ClaimToken       string `json:"claim_token"`
	NativeAttemptRef string `json:"native_attempt_ref"`
	Reason           string `json:"reason"`
}

type ReconcileRequest struct {
	AdapterID        string `json:"adapter_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	NativeAttemptRef string `json:"native_attempt_ref"`
}

// Client talks to one crew-messaging instance.
type Client struct {
	base   *url.URL
	client *http.Client
}

func New(baseURL string) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid fabric URL %q", baseURL)
	}
	return &Client{base: base, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (c *Client) Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (Lease, error) {
	var lease Lease
	err := c.call(ctx, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": adapterID, "instance_id": instanceID, "lease_duration": duration.String()}, &lease)
	return lease, err
}

func (c *Client) Renew(ctx context.Context, adapterID, token string, duration time.Duration) (Lease, error) {
	var lease Lease
	err := c.call(ctx, http.MethodPost, "/v1/adapters/renew", map[string]any{"adapter_id": adapterID, "lease_token": token, "lease_duration": duration.String()}, &lease)
	return lease, err
}

func (c *Client) RegisterSessionAddress(ctx context.Context, request SessionAddressRequest) (Binding, error) {
	var binding Binding
	err := c.call(ctx, http.MethodPost, "/v1/session-addresses", request, &binding)
	return binding, err
}

func (c *Client) Bind(ctx context.Context, address string, request BindRequest) (Binding, error) {
	var binding Binding
	err := c.call(ctx, http.MethodPut, "/v1/addresses/"+url.PathEscape(address)+"/binding", request, &binding)
	return binding, err
}

func (c *Client) Resolve(ctx context.Context, address string) (Binding, error) {
	var binding Binding
	err := c.call(ctx, http.MethodGet, "/v1/addresses/"+url.PathEscape(address), nil, &binding)
	return binding, err
}

func (c *Client) Submit(ctx context.Context, request MessageRequest) (SubmittedMessage, error) {
	var result SubmittedMessage
	err := c.call(ctx, http.MethodPost, "/v1/messages", request, &result)
	return result, err
}

func (c *Client) Claim(ctx context.Context, request ClaimRequest) (Claim, error) {
	var claim Claim
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/claim", request, &claim)
	return claim, err
}

func (c *Client) Begin(ctx context.Context, deliveryID string, request DispatchRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/begin-dispatch", request, &delivery)
	return delivery, err
}

func (c *Client) Release(ctx context.Context, deliveryID string, request ClaimTokenRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/release", request, &delivery)
	return delivery, err
}

func (c *Client) Acknowledge(ctx context.Context, deliveryID string, request ReconcileRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/acknowledge", request, &delivery)
	return delivery, err
}

func (c *Client) Unknown(ctx context.Context, deliveryID string, request ReconcileRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/outcome-unknown", request, &delivery)
	return delivery, err
}

// Fail records that a dispatch definitely did not insert the message.
func (c *Client) Fail(ctx context.Context, deliveryID string, request ReconcileRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/fail", request, &delivery)
	return delivery, err
}

// NotInserted moves a dispatching delivery back to queued after a definite
// native rejection. Use Unknown when the native outcome is not known.
func (c *Client) NotInserted(ctx context.Context, deliveryID string, request NotInsertedRequest) (Delivery, error) {
	var delivery Delivery
	err := c.call(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(deliveryID)+"/not-inserted", request, &delivery)
	return delivery, err
}

func (c *Client) call(ctx context.Context, method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	u := *c.base
	escapedPath := strings.TrimRight(u.EscapedPath(), "/") + path
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return fmt.Errorf("decode fabric request path %q: %w", escapedPath, err)
	}
	u.Path = decodedPath
	u.RawPath = escapedPath
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
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

// HTTPError is a non-2xx fabric response. Code is the fabric's error code when
// the body carries one.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("fabric HTTP %d: %s", e.Status, e.Body) }

// Code returns the fabric error code of err, or "" when err is not a fabric
// response.
func Code(err error) string {
	var typed *HTTPError
	if !errors.As(err, &typed) {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(typed.Body), &body)
	return body.Code
}
