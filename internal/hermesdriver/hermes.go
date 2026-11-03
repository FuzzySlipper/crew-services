// Package hermesdriver is the Hermes side of crew-hermes. Hermes is strongly
// profile based, so each profile (the default one and every
// ~/.hermes/profiles/<name>) is one crew address, and the dshdriver's
// delivery machinery runs over it: Hermes implements dshdriver.Native with a
// profile in place of a DSH session.
//
// A delivery goes into the profile's most recent chat through the Hermes
// gateway's own client (assets/deliver.py, run with Hermes's bundled
// Python): next_turn as a queued prompt, next_step as a steer that falls back
// to a queued prompt when no turn is running. no_wake mail is left in a
// per-profile spool that the profile's crew plugin attaches to its next turn
// (assets/crew), so it never starts one.
package hermesdriver

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"crew-services/internal/dshdriver"
)

// DefaultProfile is the profile whose home is the Hermes root itself.
const DefaultProfile = "default"

// HelperRequest is one delivery for assets/deliver.py.
type HelperRequest struct {
	Profile   string `json:"profile"`
	Mode      string `json:"mode"`
	Text      string `json:"text"`
	RequestID string `json:"request_id"`
}

// HelperResult is what assets/deliver.py answered.
type HelperResult struct {
	OK          bool   `json:"ok"`
	Session     string `json:"session"`
	Via         string `json:"via"`
	Status      string `json:"status"`
	AdmissionID string `json:"admission_id"`
	Definite    bool   `json:"definite"`
	Code        string `json:"code"`
	Error       string `json:"error"`
}

// Hermes is one Hermes install (one Linux user's ~/.hermes) seen as a
// dshdriver.Native.
type Hermes struct {
	// Root is the Hermes home (~/.hermes); Dir is crew-hermes's own
	// directory (~/.crew-hermes), shared with the crew plugin.
	Root, Dir string
	// Run makes one delivery through the gateway. NewHermes runs
	// assets/deliver.py; tests replace it.
	Run func(ctx context.Context, home string, request HelperRequest) (HelperResult, error)
	now func() time.Time

	mu sync.Mutex
	// held is every request a profile is known to hold (admitted, steered or
	// spooled), so the driver can tell a delivery reached it.
	held map[string]map[string]time.Time
	// marks is the first line of each request's text, to look for in a
	// profile's messages when a delivery's outcome was unclear.
	marks map[string]string
}

// NewHermes reads crew-hermes's record of delivered requests from state and
// runs deliveries with python and the hermes-agent checkout at agent.
func NewHermes(root, state, python, agent string, now func() time.Time) (*Hermes, error) {
	h := &Hermes{Root: root, Dir: state, now: now, held: map[string]map[string]time.Time{}, marks: map[string]string{}}
	h.Run = helperRunner(python, agent, filepath.Join(state, "deliver.py"))
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	if err := h.load(); err != nil {
		return nil, err
	}
	return h, nil
}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// Profiles lists the install's profiles: default, and each directory under
// profiles/ with a config.yaml.
func (h *Hermes) Profiles() []string {
	out := []string{DefaultProfile}
	entries, _ := os.ReadDir(filepath.Join(h.Root, "profiles"))
	for _, e := range entries {
		if !e.IsDir() || !profileName.MatchString(e.Name()) || e.Name() == DefaultProfile {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.Root, "profiles", e.Name(), "config.yaml")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out[1:])
	return out
}

// Home is a profile's Hermes home, or "" for no such profile.
func (h *Hermes) Home(profile string) string {
	for _, p := range h.Profiles() {
		if p == profile {
			if p == DefaultProfile {
				return h.Root
			}
			return filepath.Join(h.Root, "profiles", p)
		}
	}
	return ""
}

func notFound(profile string) error {
	return &dshdriver.RemoteError{Method: "profile", Code: "session/not-found", Message: "no Hermes profile " + profile}
}

// List reports every profile as an available session that is always busy.
// "Busy" is what lets the fabric hand over every policy at any time, and
// each is still delivered as its sender asked: next_turn is a queued prompt
// (the gateway starts it at once when no turn is running), next_step a steer
// that falls back to a queued prompt, and no_wake goes to the plugin's spool
// and waits for a turn the profile runs anyway. Reported idle, the fabric
// would hold no_wake mail back, and it would block the mail behind it.
func (h *Hermes) List(context.Context) ([]dshdriver.SessionSummary, error) {
	var out []dshdriver.SessionSummary
	for _, p := range h.Profiles() {
		out = append(out, dshdriver.SessionSummary{SessionID: p, AgentAvailable: true, Running: true, Cwd: h.Home(p), UpdatedAt: h.now().UnixMilli()})
	}
	return out, nil
}

// State lists the requests the profile holds as its queue, which is how the
// driver checks that a delivery arrived.
func (h *Hermes) State(_ context.Context, profile string) (dshdriver.SessionState, error) {
	if h.Home(profile) == "" {
		return dshdriver.SessionState{}, notFound(profile)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state := dshdriver.SessionState{Active: true, Running: true}
	for id := range h.held[profile] {
		state.Queue = append(state.Queue, dshdriver.QueuedItem{ID: id, RPCID: id})
	}
	return state, nil
}

// Events reports the profile's current model, and a user message for each
// request whose text turned up in the profile's chats after an unclear
// delivery.
func (h *Hermes) Events(ctx context.Context, profile string, _ int) ([]dshdriver.Event, error) {
	home := h.Home(profile)
	if home == "" {
		return nil, notFound(profile)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db")+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var events []dshdriver.Event
	var model string
	if db.QueryRowContext(ctx, `SELECT model FROM sessions WHERE model IS NOT NULL AND model != '' ORDER BY last_activity_at DESC LIMIT 1`).Scan(&model) == nil && model != "" {
		data, _ := json.Marshal(map[string]any{"message": map[string]any{"source": map[string]any{"model": model}}})
		events = append(events, dshdriver.Event{Type: "assistant/message", Data: data})
	}
	h.mu.Lock()
	marks := map[string]string{}
	for id, mark := range h.marks {
		if _, held := h.held[profile][id]; !held {
			marks[id] = mark
		}
	}
	h.mu.Unlock()
	if len(marks) == 0 {
		return events, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT content FROM messages WHERE role = 'user' ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return events, err
	}
	defer rows.Close()
	for rows.Next() {
		var content sql.NullString
		if rows.Scan(&content) != nil {
			continue
		}
		for id, mark := range marks {
			if mark != "" && strings.Contains(content.String, mark) {
				h.markHeld(profile, id)
				data, _ := json.Marshal(map[string]any{"source": map[string]any{"rpcId": id}})
				events = append(events, dshdriver.Event{Type: "user/message", Data: data})
				delete(marks, id)
			}
		}
	}
	return events, rows.Err()
}

// Prompt delivers into the profile's most recent chat: queued, or steered
// into a running turn.
func (h *Hermes) Prompt(ctx context.Context, p dshdriver.PromptRequest) error {
	home := h.Home(p.SessionID)
	if home == "" {
		return notFound(p.SessionID)
	}
	h.mu.Lock()
	h.marks[p.RequestID] = firstLine(p.Text)
	h.mu.Unlock()
	result, err := h.Run(ctx, home, HelperRequest{Profile: p.SessionID, Mode: p.Mode, Text: p.Text, RequestID: p.RequestID})
	if err != nil {
		return err
	}
	if !result.OK {
		if result.Definite {
			return &dshdriver.RemoteError{Method: "prompt.submit", Code: result.Code, Message: result.Error}
		}
		return fmt.Errorf("hermes %s: %s", result.Code, result.Error)
	}
	return h.markHeld(p.SessionID, p.RequestID)
}

// Inject spools a no_wake message for the profile's crew plugin, which
// attaches it to the next turn the profile runs. It never starts one.
func (h *Hermes) Inject(_ context.Context, p dshdriver.PromptRequest) error {
	if h.Home(p.SessionID) == "" {
		return notFound(p.SessionID)
	}
	dir := filepath.Join(h.Dir, "pending", p.SessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s.json", h.now().UnixNano(), safeName(p.RequestID))
	data, _ := json.Marshal(map[string]string{"request_id": p.RequestID, "text": p.Text})
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return h.markHeld(p.SessionID, p.RequestID)
}

func safeName(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(s, "_")
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

type heldRecord struct {
	Profile   string    `json:"profile"`
	RequestID string    `json:"request_id"`
	At        time.Time `json:"at"`
}

// heldFor is how long a delivered request is remembered.
const heldFor = 14 * 24 * time.Hour

func (h *Hermes) markHeld(profile, requestID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.held[profile][requestID]; ok {
		return nil
	}
	at := h.now().UTC()
	if h.held[profile] == nil {
		h.held[profile] = map[string]time.Time{}
	}
	h.held[profile][requestID] = at
	delete(h.marks, requestID)
	f, err := os.OpenFile(filepath.Join(h.Dir, "held.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(heldRecord{Profile: profile, RequestID: requestID, At: at})
	_, err = f.Write(append(line, '\n'))
	return err
}

// load reads the record of delivered requests, dropping old ones.
func (h *Hermes) load() error {
	data, err := os.ReadFile(filepath.Join(h.Dir, "held.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := h.now().Add(-heldFor)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var r heldRecord
		if json.Unmarshal(scanner.Bytes(), &r) != nil || r.At.Before(cutoff) {
			continue
		}
		if h.held[r.Profile] == nil {
			h.held[r.Profile] = map[string]time.Time{}
		}
		h.held[r.Profile][r.RequestID] = r.At
	}
	return scanner.Err()
}

// helperRunner runs assets/deliver.py with Hermes's Python for one delivery.
func helperRunner(python, agent, script string) func(context.Context, string, HelperRequest) (HelperResult, error) {
	return func(ctx context.Context, home string, request HelperRequest) (HelperResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		input, _ := json.Marshal(request)
		cmd := exec.CommandContext(ctx, python, "-I", script)
		cmd.Env = append(os.Environ(), "HERMES_HOME="+home, "CREW_HERMES_AGENT="+agent)
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return HelperResult{}, context.DeadlineExceeded
			}
			return HelperResult{}, fmt.Errorf("delivery helper: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		var result HelperResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			return HelperResult{}, fmt.Errorf("delivery helper answered %q: %v", strings.TrimSpace(stdout.String()), err)
		}
		return result, nil
	}
}
