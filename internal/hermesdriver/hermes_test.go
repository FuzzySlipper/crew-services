package hermesdriver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/dshdriver"
)

// install makes a Hermes root with the default profile and kate, each with a
// state.db holding a session's model and one user message.
func install(t *testing.T) (*Hermes, string) {
	t.Helper()
	root := t.TempDir()
	for _, home := range []string{root, filepath.Join(root, "profiles", "kate")} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(home, "config.yaml"), []byte("{}"), 0o600)
		db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			`CREATE TABLE sessions (id TEXT, model TEXT, last_activity_at REAL)`,
			`CREATE TABLE messages (id INTEGER PRIMARY KEY, role TEXT, content TEXT)`,
			`INSERT INTO sessions VALUES ('old', 'claude-sonnet-5-5', 1), ('new', 'claude-opus-5-5', 2)`,
			`INSERT INTO messages (role, content) VALUES ('user', 'Crew message from s/abc (crew message m-lost):' || char(10) || 'hello')`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
	}
	_ = os.MkdirAll(filepath.Join(root, "profiles", "notaprofile"), 0o700) // no config.yaml
	h, err := NewHermes(root, filepath.Join(t.TempDir(), "crew-hermes"), "python3", "/agent", func() time.Time { return time.Unix(1_800_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return h, root
}

func TestProfilesAreTheSessions(t *testing.T) {
	h, root := install(t)
	list, _ := h.List(context.Background())
	if len(list) != 2 || list[0].SessionID != "default" || list[0].Cwd != root || list[1].SessionID != "kate" || !list[1].AgentAvailable || !list[1].Running {
		t.Fatalf("list = %+v", list)
	}
	if _, err := h.State(context.Background(), "nobody"); dshdriver.ErrorCode(err) != "session/not-found" {
		t.Fatalf("an unknown profile = %v", err)
	}
}

func TestPromptRecordsWhatTheGatewayAdmitted(t *testing.T) {
	h, _ := install(t)
	var got HelperRequest
	h.Run = func(_ context.Context, home string, r HelperRequest) (HelperResult, error) {
		got = r
		if !strings.HasSuffix(home, "profiles/kate") {
			t.Errorf("home = %s", home)
		}
		return HelperResult{OK: true, Session: "s1", Via: "submit", Status: "queued", AdmissionID: "a1"}, nil
	}
	if err := h.Prompt(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "crew-delivery:d1", Mode: "queue", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got.Profile != "kate" || got.Mode != "queue" || got.RequestID != "crew-delivery:d1" {
		t.Fatalf("helper request = %+v", got)
	}
	state, _ := h.State(context.Background(), "kate")
	if !dshdriver.HasRequest(state, nil, "crew-delivery:d1") {
		t.Fatalf("kate's state does not hold the delivery: %+v", state)
	}
	// The record survives a restart.
	again, err := NewHermes(h.Root, h.Dir, "python3", "/agent", h.now)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := again.State(context.Background(), "kate"); !dshdriver.HasRequest(state, nil, "crew-delivery:d1") {
		t.Fatal("a restart forgot the delivery")
	}
}

func TestARefusalIsDefiniteAndAnUnclearAnswerIsNot(t *testing.T) {
	h, _ := install(t)
	h.Run = func(context.Context, string, HelperRequest) (HelperResult, error) {
		return HelperResult{OK: false, Definite: true, Code: "no-chat", Error: "the profile has no chat"}, nil
	}
	err := h.Prompt(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "r1", Text: "x"})
	if !dshdriver.Definite(err) || dshdriver.ErrorCode(err) != "no-chat" {
		t.Fatalf("refusal = %v", err)
	}
	h.Run = func(context.Context, string, HelperRequest) (HelperResult, error) {
		return HelperResult{OK: false, Code: "ConnectionClosed", Error: "lost"}, nil
	}
	if err := h.Prompt(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "r2", Text: "x"}); err == nil || dshdriver.Definite(err) {
		t.Fatalf("an unclear answer = %v", err)
	}
	h.Run = func(context.Context, string, HelperRequest) (HelperResult, error) {
		return HelperResult{}, context.DeadlineExceeded
	}
	if err := h.Prompt(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "r3", Text: "x"}); !dshdriver.Timeout(err) {
		t.Fatalf("a timeout = %v", err)
	}
}

func TestEventsCarryTheModelAndFindAnUnclearDelivery(t *testing.T) {
	h, _ := install(t)
	h.Run = func(context.Context, string, HelperRequest) (HelperResult, error) {
		return HelperResult{}, errors.New("connection dropped after sending")
	}
	text := "Crew message from s/abc (crew message m-lost):\nhello"
	_ = h.Prompt(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "crew-delivery:lost", Text: text})
	events, err := h.Events(context.Background(), "kate", 20)
	if err != nil {
		t.Fatal(err)
	}
	if !dshdriver.HasRequest(dshdriver.SessionState{}, events, "crew-delivery:lost") {
		t.Fatalf("the delivery in kate's messages was not found: %+v", events)
	}
	var model struct {
		Message struct {
			Source struct {
				Model string `json:"model"`
			} `json:"source"`
		} `json:"message"`
	}
	if len(events) == 0 || events[0].Type != "assistant/message" || json.Unmarshal(events[0].Data, &model) != nil || model.Message.Source.Model != "claude-opus-5-5" {
		t.Fatalf("model event = %+v", events)
	}
	// Once found, it is held.
	if state, _ := h.State(context.Background(), "kate"); !dshdriver.HasRequest(state, nil, "crew-delivery:lost") {
		t.Fatal("a found delivery is not held")
	}
}

func TestNoWakeMailIsSpooledForThePlugin(t *testing.T) {
	h, _ := install(t)
	h.Run = func(context.Context, string, HelperRequest) (HelperResult, error) {
		t.Fatal("no_wake mail went to the gateway")
		return HelperResult{}, nil
	}
	if err := h.Inject(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "crew-delivery:quiet", Text: "a subscription notice"}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(h.Dir, "pending", "kate", "*.json"))
	if len(files) != 1 {
		t.Fatalf("spool = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "a subscription notice") {
		t.Fatalf("spooled %s", data)
	}
	if state, _ := h.State(context.Background(), "kate"); !dshdriver.HasRequest(state, nil, "crew-delivery:quiet") {
		t.Fatal("spooled mail is not held")
	}
}

func TestInstallWritesTheHelperAndEachProfilesPlugin(t *testing.T) {
	h, root := install(t)
	profiles, err := h.Install()
	if err != nil || strings.Join(profiles, ",") != "default,kate" {
		t.Fatalf("installed %v, %v", profiles, err)
	}
	for _, path := range []string{
		filepath.Join(h.Dir, "deliver.py"),
		filepath.Join(root, "plugins", "crew", "__init__.py"),
		filepath.Join(root, "profiles", "kate", "plugins", "crew", "plugin.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s", path)
		}
	}
	if err := h.WriteIntro("kate", "Your crew messaging address is s/x."); err != nil {
		t.Fatal(err)
	}
}
