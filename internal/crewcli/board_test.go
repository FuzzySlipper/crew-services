package crewcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
)

// fakeBoard answers the few crew-board calls these tests make, and records
// every write so a test can check nothing was written.
type fakeBoard struct {
	writes []string
}

func (f *fakeBoard) serve(t *testing.T) {
	t.Helper()
	webp := board.Attachment{Ref: strings.Repeat("a", 64), Kind: board.MediaImage, Type: "image/webp", State: board.AttachmentReady}
	held := board.Attachment{Ref: strings.Repeat("b", 64), Kind: board.MediaImage, State: board.AttachmentHeld, Reason: "scan"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/site", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"people": []string{"patch", "lore"}})
	})
	mux.HandleFunc("GET /v1/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		read := board.ThreadRead{Thread: board.Thread{ID: id, Title: "Title of " + id, Board: "design", State: "open", Posts: 1}}
		if id == "t1" {
			read.Posts = []board.Post{{ID: "p1", ThreadID: "t1", Body: "two images", CreatedAt: time.Now(), Attachments: []board.Attachment{webp, held}}}
		}
		_ = json.NewEncoder(w).Encode(read)
	})
	mux.HandleFunc("GET /v1/blobs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("RIFF-webp-bytes"))
	})
	mux.HandleFunc("GET /v1/blobs/{ref}/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(webp)
	})
	mux.HandleFunc("GET /v1/boards/{board}/search", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		_ = json.NewEncoder(w).Encode(map[string]any{"posts": []board.Post{
			{ID: "p1", ThreadID: "t1", Body: "first match", CreatedAt: now},
			{ID: "p2", ThreadID: "t2", Body: "other thread", CreatedAt: now},
			{ID: "p3", ThreadID: "t1", Body: "second match", CreatedAt: now},
		}})
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		f.writes = append(f.writes, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("CREW_BOARD_URL", server.URL)
	t.Setenv("CREW_BOARD_PERSON", "")
}

// inHarness sets the session variables as one harness would, clearing the
// others (this test may itself run inside an agent session).
func inHarness(t *testing.T, env string) {
	t.Helper()
	for _, d := range agentDrivers {
		t.Setenv(d.env, "")
	}
	if env != "" {
		t.Setenv(env, "native-session-id")
	}
}

func runBoard(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := BoardMain(context.Background(), args, "", strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAnAgentSessionCannotWriteAsAPerson(t *testing.T) {
	for _, d := range agentDrivers {
		t.Run(d.harness, func(t *testing.T) {
			fake := &fakeBoard{}
			fake.serve(t)
			inHarness(t, d.env)
			for _, args := range [][]string{
				{"reply", "t1", "--person", "patch", "hello"},
				{"reply", "t1", "--as", "s/abc", "hello"},
				{"read", "t1", "--person", "patch"},
			} {
				code, _, stderr := runBoard(args...)
				if code == 0 || !strings.Contains(stderr, "this is a "+d.harness+" session") || !strings.Contains(stderr, "`"+d.command+" board "+args[0]) {
					t.Errorf("%v = %d %q", args, code, stderr)
				}
			}
			// CREW_BOARD_PERSON inherited from a person's shell counts too.
			t.Setenv("CREW_BOARD_PERSON", "patch")
			if code, _, stderr := runBoard("reply", "t1", "hello"); code == 0 || !strings.Contains(stderr, d.command) {
				t.Errorf("an inherited CREW_BOARD_PERSON = %d %q", code, stderr)
			}
			t.Setenv("CREW_BOARD_PERSON", "")
			// With no flags, the error names the driver, not --person.
			code, _, stderr := runBoard("reply", "t1", "hello")
			if code == 0 || strings.Contains(stderr, "--person NAME") || !strings.Contains(stderr, d.command+" board reply") {
				t.Errorf("a bare reply = %d %q", code, stderr)
			}
			if len(fake.writes) != 0 {
				t.Fatalf("an agent session wrote %v", fake.writes)
			}
		})
	}
}

func TestOutsideASessionTheErrorsSayWhoIsWho(t *testing.T) {
	fake := &fakeBoard{}
	fake.serve(t)
	inHarness(t, "")
	_, _, stderr := runBoard("reply", "t1", "--as", "crew.lead", "hello")
	if !strings.Contains(stderr, "--as names a seat") || !strings.Contains(stderr, "through its driver") {
		t.Errorf("--as outside a session: %q", stderr)
	}
	_, _, stderr = runBoard("reply", "t1", "hello")
	if !strings.Contains(stderr, "--person NAME (patch, lore)") || !strings.Contains(stderr, "agents use their driver") {
		t.Errorf("no person: %q", stderr)
	}
	if code, _, stderr := runBoard("reply", "t1", "--person", "patch", "hello"); code != 0 || len(fake.writes) == 0 {
		t.Errorf("a person's reply = %d %q", code, stderr)
	}
}

func TestReadSavesAThreadsAttachments(t *testing.T) {
	fake := &fakeBoard{}
	fake.serve(t)
	inHarness(t, "")
	dir := filepath.Join(t.TempDir(), "images")
	code, stdout, stderr := runBoard("read", "t1", "--peek", "--attachments", dir)
	if code != 0 {
		t.Fatalf("read = %d %s", code, stderr)
	}
	saved := filepath.Join(dir, strings.Repeat("a", 16)+".webp")
	data, err := os.ReadFile(saved)
	if err != nil || string(data) != "RIFF-webp-bytes" || !strings.Contains(stdout, "saved image aaaaaaaaaaaa to "+saved) {
		t.Fatalf("saved %q, %v; output:\n%s", data, err, stdout)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a held attachment was saved too: %v", entries)
	}
	// blob --out DIR names the file the same way.
	out := t.TempDir()
	if code, stdout, _ := runBoard("blob", strings.Repeat("a", 64), "--out", out); code != 0 || !strings.Contains(stdout, filepath.Join(out, strings.Repeat("a", 16)+".webp")) {
		t.Fatalf("blob --out DIR = %d %s", code, stdout)
	}
}

func TestSearchGroupsByThread(t *testing.T) {
	fake := &fakeBoard{}
	fake.serve(t)
	inHarness(t, "")
	code, stdout, _ := runBoard("search", "design", "match")
	if code != 0 {
		t.Fatal(code)
	}
	if strings.Count(stdout, "· thread ") != 2 || !strings.Contains(stdout, `"Title of t1" · thread t1 · 2 matching posts (board read t1)`) {
		t.Fatalf("search output:\n%s", stdout)
	}
	if strings.Index(stdout, "first match") > strings.Index(stdout, "second match") || strings.Index(stdout, "second match") > strings.Index(stdout, "other thread") {
		t.Fatalf("matches are not grouped under their threads:\n%s", stdout)
	}
}

func TestSessionStartNamesTheBoardCommand(t *testing.T) {
	for _, driver := range []string{"crew-claude", "crew-codex"} {
		if text := BoardContext(driver); !strings.Contains(text, "`"+driver+" board`") || !strings.Contains(text, "Den MCP board tools are a different board") {
			t.Errorf("%s: %q", driver, text)
		}
	}
}
