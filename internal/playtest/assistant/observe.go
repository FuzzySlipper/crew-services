package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"crew-services/internal/playtest/client"
)

const (
	debugResponseLimit  = 1 << 20
	debugRequestTimeout = 5 * time.Second
)

// Observer obtains a native capture and a deliberately small set of optional,
// product-published read-only facts. ProductURL is supplied by the planner and
// checked against the active session profile by the command entrypoint.
type Observer struct {
	Client       *client.Client
	SessionID    string
	ProductURL   string
	CaptureEvery int
	observations int
	Commands     []string
}

// Observation keeps capture metadata and product facts separate. Capture is
// original playtest-service metadata; it makes no visual interpretation claim.
// Facts retain their original JSON values so product numeric precision survives.
type Observation struct {
	CapturedAt time.Time                  `json:"captured_at"`
	Capture    json.RawMessage            `json:"capture"`
	Facts      map[string]json.RawMessage `json:"facts"`
}

// Observe captures the current native screenshot metadata, then executes only
// allowlisted read-only product queries. A requested but unavailable fact is an
// error; callers must not replace it with a guessed value.
func (o *Observer) Observe(ctx context.Context) (Observation, error) {
	commands := make([]string, len(o.Commands))
	for i, command := range o.Commands {
		validated, err := validateCommand(command)
		if err != nil {
			return Observation{}, err
		}
		commands[i] = validated
	}
	if o.Client == nil {
		return Observation{}, errors.New("playtest observer client is required")
	}
	if strings.TrimSpace(o.SessionID) == "" {
		return Observation{}, errors.New("playtest observer session ID is required")
	}

	started := time.Now().UTC()
	var capture json.RawMessage
	every := o.CaptureEvery
	if every <= 0 {
		every = 1
	}
	if o.observations%every == 0 {
		var err error
		capture, err = o.Client.Result(ctx, client.Request{Op: "observe", SessionID: o.SessionID})
		if err != nil {
			return Observation{}, fmt.Errorf("capture session observation: %w", err)
		}
		if !json.Valid(capture) {
			return Observation{}, errors.New("capture session observation returned invalid JSON")
		}
	} else {
		capture = json.RawMessage(`{"status":"not_captured_this_update"}`)
	}
	o.observations++
	result := Observation{CapturedAt: started, Capture: capture, Facts: make(map[string]json.RawMessage, len(commands))}
	if len(commands) == 0 {
		return result, nil
	}

	origin, err := debugOrigin(o.ProductURL)
	if err != nil {
		return Observation{}, err
	}
	transport := debugTransport{origin: origin, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	catalog, err := transport.catalog(ctx)
	if err != nil {
		return Observation{}, err
	}
	for _, command := range commands {
		if !catalog[commandName(command)] {
			return Observation{}, fmt.Errorf("capability_unavailable: product debug command %q", commandName(command))
		}
	}
	for _, command := range commands {
		raw, err := transport.execute(ctx, command)
		if err != nil {
			return Observation{}, fmt.Errorf("observe product debug command %q: %w", command, err)
		}
		result.Facts[command] = rawJSON(raw)
	}
	return result, nil
}

func validateCommand(command string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) == 1 && (fields[0] == "loading-bay.readout" || fields[0] == "combat.observe" || fields[0] == "interaction.inspect" || fields[0] == "interaction.help") {
		return fields[0], nil
	}
	if len(fields) != 4 || fields[0] != "spatial.map" || (fields[1] != "json" && fields[1] != "ascii") {
		return "", errors.New("product observation command must be combat.observe, interaction.inspect, interaction.help, loading-bay.readout or spatial.map <json|ascii> <radius> <cellSize>")
	}
	radius, err := strconv.Atoi(fields[2])
	if err != nil || radius < 0 || radius > 15 {
		return "", errors.New("spatial.map radius must be an integer from 0 through 15")
	}
	cellSize, err := strconv.ParseFloat(fields[3], 64)
	if err != nil || math.IsNaN(cellSize) || math.IsInf(cellSize, 0) || cellSize <= 0 {
		return "", errors.New("spatial.map cellSize must be finite and greater than zero")
	}
	return strings.Join(fields, " "), nil
}

func commandName(command string) string { return strings.Fields(command)[0] }

func debugOrigin(raw string) (*url.URL, error) {
	origin, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil {
		return nil, errors.New("product observation requires an HTTP(S) product URL without credentials")
	}
	origin.Path = ""
	origin.RawPath = ""
	origin.RawQuery = ""
	origin.Fragment = ""
	return origin, nil
}

type debugTransport struct {
	origin *url.URL
	client *http.Client
}

func (t debugTransport) catalog(ctx context.Context) (map[string]bool, error) {
	raw, status, err := t.request(ctx, http.MethodGet, "/__rusty/product/runtime/debug/catalog", "")
	if err != nil {
		return nil, fmt.Errorf("request product debug catalog: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, errors.New("capability_unavailable: product live-debug catalog")
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("product debug catalog HTTP %d", status)
	}
	var catalog struct {
		Available bool `json:"available"`
		Commands  []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("invalid product debug catalog: %w", err)
	}
	if !catalog.Available {
		return nil, errors.New("capability_unavailable: product live-debug catalog")
	}
	available := make(map[string]bool, len(catalog.Commands))
	for _, command := range catalog.Commands {
		available[command.Name] = true
	}
	return available, nil
}

func (t debugTransport) execute(ctx context.Context, command string) ([]byte, error) {
	raw, status, err := t.request(ctx, http.MethodPost, "/__rusty/product/runtime/debug/execute", command)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("product debug query HTTP %d", status)
	}
	return raw, nil
}

func (t debugTransport) request(ctx context.Context, method, path, body string) ([]byte, int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, debugRequestTimeout)
	defer cancel()
	u := *t.origin
	u.Path = path
	u.RawPath = ""
	req, err := http.NewRequestWithContext(requestCtx, method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, debugResponseLimit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > debugResponseLimit {
		return nil, resp.StatusCode, errors.New("product debug response exceeds 1 MiB")
	}
	return raw, resp.StatusCode, nil
}

func rawJSON(raw []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if json.Valid(trimmed) {
		return append(json.RawMessage(nil), trimmed...)
	}
	// Existing readout commands are text. Encode that text as a JSON string,
	// rather than parsing or inferring facts from it.
	encoded, _ := json.Marshal(string(raw))
	return encoded
}
