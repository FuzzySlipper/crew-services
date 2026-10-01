// Package engine drives a Rusty Engine product host directly over its loopback
// HTTP surface: live-debug commands, claimed runtime input and frame capture.
// No browser is involved, so nothing depends on window focus or page state.
package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	runtimePrefix  = "/__rusty/product/runtime"
	maxAnswerBytes = 8 << 20
	maxFrameBytes  = 64 << 20
)

// Host is one product host's origin, e.g. http://127.0.0.1:30300.
type Host struct {
	Origin string
	Client *http.Client
}

// RefusedError is a 422 from debug/execute: the command was refused by usage
// or the product, not lost in transport.
type RefusedError struct{ Command, Detail string }

func (e *RefusedError) Error() string { return fmt.Sprintf("%s refused: %s", e.Command, e.Detail) }

// ErrUnavailable is a 503: no runtime is serving (a restage is in progress
// or failed). Nothing was applied.
var ErrUnavailable = errors.New("product host has no serving runtime (503); re-read the binding after it returns")

func NewHost(origin string) (*Host, error) {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, fmt.Errorf("engine host origin must be an http origin, got %q", origin)
	}
	return &Host{Origin: parsed.Scheme + "://" + parsed.Host, Client: &http.Client{}}, nil
}

func (h *Host) do(ctx context.Context, method, path, contentType string, body []byte, limit int64) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, h.Origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := h.Client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response, nil, err
	}
	if int64(len(data)) > limit {
		return response, nil, fmt.Errorf("%s answered more than %d bytes", path, limit)
	}
	return response, data, nil
}

// Debug runs one live-debug command. Engine commands answer JSON text; any
// other answer is returned as {"message": text}.
func (h *Host) Debug(ctx context.Context, command string) (map[string]any, error) {
	response, data, err := h.do(ctx, http.MethodPost, runtimePrefix+"/debug/execute", "text/plain; charset=utf-8", []byte(command), maxAnswerBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	switch {
	case response.StatusCode == http.StatusServiceUnavailable:
		return nil, ErrUnavailable
	case response.StatusCode == http.StatusUnprocessableEntity:
		return nil, &RefusedError{Command: command, Detail: strings.TrimSpace(string(data))}
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d: %s", command, response.StatusCode, strings.TrimSpace(string(data)))
	}
	var answer map[string]any
	if err := json.Unmarshal(data, &answer); err != nil {
		return map[string]any{"message": strings.TrimSpace(string(data))}, nil
	}
	return answer, nil
}

// Binding is one runtime input binding, in canonical decimal text.
type Binding struct {
	InstanceID      string `json:"instanceId"`
	Generation      string `json:"generation"`
	ControlRevision string `json:"controlRevision"`
}

// Result is the host's typed answer to control and input requests.
// Disposition is the X-Rusty-Commit-Disposition header: not-applied,
// unknown, committed or resync-required.
type Result struct {
	Accepted          bool            `json:"accepted"`
	Code              string          `json:"code"`
	Disposition       string          `json:"disposition"`
	Diagnostic        string          `json:"diagnostic,omitempty"`
	Binding           *Binding        `json:"binding,omitempty"`
	NextInputSequence string          `json:"nextInputSequence,omitempty"`
	AcceptedThrough   string          `json:"acceptedThrough,omitempty"`
	ConsumedThrough   string          `json:"consumedThrough,omitempty"`
	Count             int             `json:"count,omitempty"`
	AcceptedCount     int             `json:"acceptedCount,omitempty"`
	DroppedCount      int             `json:"droppedCount,omitempty"`
	Readout           json.RawMessage `json:"readout,omitempty"`
	Commit            string          `json:"-"`
}

// post sends one JSON request. An error means the outcome is unknown unless
// it is an *HTTPError with a status the host answers before dispatch.
func (h *Host) post(ctx context.Context, path string, body any) (Result, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	response, data, err := h.do(ctx, http.MethodPost, runtimePrefix+path, "application/json", encoded, maxAnswerBytes)
	if err != nil {
		return Result{Commit: "unknown"}, fmt.Errorf("%s: %w", path, err)
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return Result{Commit: "not-applied"}, ErrUnavailable
	}
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusRequestEntityTooLarge || response.StatusCode == http.StatusUnsupportedMediaType || response.StatusCode == http.StatusRequestTimeout {
		return Result{Commit: "not-applied"}, fmt.Errorf("%s refused before dispatch: HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{Commit: "unknown"}, fmt.Errorf("%s: HTTP %d with an unreadable answer: %w", path, response.StatusCode, err)
	}
	result.Commit = response.Header.Get("X-Rusty-Commit-Disposition")
	if result.Commit == "" {
		result.Commit = "unknown"
	}
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("%s: HTTP %d: %s %s", path, response.StatusCode, result.Code, result.Diagnostic)
	}
	return result, nil
}

// Claim takes input from the binding's owner for leaseMs after the last input.
func (h *Host) Claim(ctx context.Context, binding Binding, label string, lease time.Duration) (Result, error) {
	return h.post(ctx, "/control/claim", map[string]any{"runtime": binding, "label": label, "leaseMs": fmt.Sprint(lease.Milliseconds())})
}

// ReleaseClaim hands input back to the page and clears what the harness held.
func (h *Host) ReleaseClaim(ctx context.Context, binding Binding) (Result, error) {
	return h.post(ctx, "/control/release", map[string]any{"runtime": binding})
}

// Input posts one ordered batch of wire events.
func (h *Host) Input(ctx context.Context, batch []map[string]any) (Result, error) {
	return h.post(ctx, "/input", map[string]any{"batch": batch})
}

// Catalog lists the host's live-debug command names.
func (h *Host) Catalog(ctx context.Context) ([]string, error) {
	response, data, err := h.do(ctx, http.MethodGet, runtimePrefix+"/debug/catalog", "", nil, maxAnswerBytes)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("debug catalog: HTTP %d", response.StatusCode)
	}
	var catalog struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("debug catalog: %w", err)
	}
	names := make([]string, 0, len(catalog.Commands))
	for _, command := range catalog.Commands {
		names = append(names, command.Name)
	}
	return names, nil
}

// Frame is one RSF1 frame: the runtime's own world image with its step.
type Frame struct {
	Sequence uint64 `json:"sequence"`
	Step     uint64 `json:"step"`
	Width    uint32 `json:"width"`
	Height   uint32 `json:"height"`
	Format   string `json:"format"`
	Held     bool   `json:"held"`
	Video    bool   `json:"video"`
	Cameras  string `json:"cameras,omitempty"`
	Payload  []byte `json:"-"`
}

// ParseFrame reads the RSF1 header: magic, header length, sequence, step,
// width, height, format (1 JPEG, 2 RGBA8, 3 PNG), flags (1 held, 2 video) and
// payload length. Later fields extend the header and are skipped.
func ParseFrame(data []byte) (Frame, error) {
	if len(data) < 40 || string(data[:4]) != "RSF1" {
		return Frame{}, errors.New("not an RSF1 frame")
	}
	le := binary.LittleEndian
	headerLength, payloadLength := uint64(le.Uint32(data[4:])), uint64(le.Uint32(data[36:]))
	if headerLength < 40 || headerLength+payloadLength > uint64(len(data)) {
		return Frame{}, errors.New("truncated RSF1 frame")
	}
	formats := map[byte]string{1: "jpeg", 2: "rgba8", 3: "png"}
	format, ok := formats[data[32]]
	if !ok {
		format = fmt.Sprintf("unknown-%d", data[32])
	}
	return Frame{Sequence: le.Uint64(data[8:]), Step: le.Uint64(data[16:]), Width: le.Uint32(data[24:]), Height: le.Uint32(data[28:]),
		Format: format, Held: data[33]&1 != 0, Video: data[33]&2 != 0, Payload: data[headerLength : headerLength+payloadLength]}, nil
}

// Capture draws one lossless world frame for a tool without becoming a
// viewer, so no page or window changes size. Zero width/height uses the
// output's own size. The UI overlay is never in it.
func (h *Host) Capture(ctx context.Context, width, height int) (Frame, error) {
	query := url.Values{"format": {"png"}}
	if width > 0 && height > 0 {
		query.Set("width", fmt.Sprint(width))
		query.Set("height", fmt.Sprint(height))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, data, err := h.do(ctx, http.MethodGet, runtimePrefix+"/frames/capture?"+query.Encode(), "", nil, maxFrameBytes)
	if err != nil {
		return Frame{}, fmt.Errorf("frame capture: %w", err)
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return Frame{}, ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return Frame{}, fmt.Errorf("frame capture: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	frame, err := ParseFrame(data)
	if err != nil {
		return Frame{}, fmt.Errorf("frame capture: %w", err)
	}
	frame.Cameras = response.Header.Get("X-Rusty-Frame-Cameras")
	return frame, nil
}
