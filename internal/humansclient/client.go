// Package humansclient is the HTTP client other services use to reach
// crew-humans (docs/crew-humans.md). It has its own copies of the contract's
// shapes, so callers never import crew-humans' packages.
package humansclient

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

type Client struct {
	base string
	http *http.Client
}

func New(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("crew-humans URL %q is not an http address", base)
	}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}, nil
}

type Person struct {
	Name    string `json:"name"`
	Display string `json:"display"`
}

type NotifyRequest struct {
	Person         string `json:"person"`
	Source         string `json:"source"`
	IdempotencyKey string `json:"idempotency_key"`
	Title          string `json:"title"`
	Text           string `json:"text,omitempty"`
	Link           string `json:"link,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Group          string `json:"group,omitempty"`
	Urgent         bool   `json:"urgent,omitempty"`
}

type NotifyResult struct {
	NoticeID string   `json:"notice_id"`
	Outcome  string   `json:"outcome"`
	Routes   []string `json:"routes"`
}

type Delivery struct {
	Route     int        `json:"route"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
}

type Notice struct {
	ID         string     `json:"id"`
	Person     string     `json:"person"`
	Source     string     `json:"source"`
	Title      string     `json:"title"`
	Text       string     `json:"text,omitempty"`
	Link       string     `json:"link,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Urgent     bool       `json:"urgent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ReadAt     *time.Time `json:"read_at,omitempty"`
	Deliveries []Delivery `json:"deliveries"`
}

type Inbox struct {
	Notices []Notice `json:"notices"`
	Unread  int      `json:"unread"`
}

// HTTPError is a refusal from crew-humans, with its reason code.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("crew-humans answered %d %s: %s", e.Status, e.Code, e.Message)
}

// People lists the people on crew-humans' site.
func (c *Client) People(ctx context.Context) (string, []Person, error) {
	var out struct {
		Site   string   `json:"site"`
		People []Person `json:"people"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/people", nil, &out)
	return out.Site, out.People, err
}

func (c *Client) Notify(ctx context.Context, request NotifyRequest) (NotifyResult, error) {
	var out NotifyResult
	err := c.do(ctx, http.MethodPost, "/v1/notices", request, &out)
	return out, err
}

// Notices lists a person's notices, newest first; before (an ID) pages to
// older ones.
func (c *Client) Notices(ctx context.Context, person string, unreadOnly bool, before string, limit int) (Inbox, error) {
	q := url.Values{"person": {person}, "limit": {fmt.Sprint(limit)}}
	if unreadOnly {
		q.Set("state", "unread")
	}
	if before != "" {
		q.Set("before", before)
	}
	var out Inbox
	err := c.do(ctx, http.MethodGet, "/v1/notices?"+q.Encode(), nil, &out)
	return out, err
}

// Notice reads one notice.
func (c *Client) Notice(ctx context.Context, id string) (Notice, error) {
	var out Notice
	err := c.do(ctx, http.MethodGet, "/v1/notices/"+url.PathEscape(id), nil, &out)
	return out, err
}

// MarkRead marks notices read: the IDs given, all through one ID, or all in
// a group. Give exactly one.
func (c *Client) MarkRead(ctx context.Context, person string, ids []string, through, group string) error {
	return c.do(ctx, http.MethodPost, "/v1/notices/read", map[string]any{"person": person, "ids": ids, "through": through, "group": group}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode >= 300 {
		var problem struct{ Code, Error string }
		_ = json.Unmarshal(data, &problem)
		return &HTTPError{Status: response.StatusCode, Code: problem.Code, Message: problem.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
