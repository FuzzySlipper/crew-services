package crewcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crew-services/internal/board"
)

func TestLastModelReadsTheNewestRecognizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := []string{`{"model":"old"}`, `{"model":"new"}`, `{"other":1}`, ``}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	extract := func(line []byte) string {
		if s := string(line); strings.HasPrefix(s, `{"model":"`) {
			return strings.TrimSuffix(strings.TrimPrefix(s, `{"model":"`), `"}`)
		}
		return ""
	}
	if got := LastModel(path, extract); got != "new" {
		t.Fatalf("model = %q", got)
	}
	if got := LastModel(filepath.Join(t.TempDir(), "missing"), extract); got != "" {
		t.Fatalf("a missing transcript gave %q", got)
	}
}

func TestAuthorNamesShowTheModel(t *testing.T) {
	seat := board.Author{Kind: board.AuthorSeat, Address: "crew.lead", Session: "s/abc", Adapter: "claude-code", Model: "claude-opus-5-5"}
	if got := AuthorName(seat); got != "crew.lead (Claude Code, claude-opus-5-5, s/abc)" {
		t.Errorf("seat = %q", got)
	}
	session := board.Author{Kind: board.AuthorSession, Address: "s/def", Adapter: "crew-codex", Model: "gpt-6-astra"}
	if got := AuthorName(session); got != "s/def (Codex, gpt-6-astra)" {
		t.Errorf("session = %q", got)
	}
	if got := AuthorName(board.Author{Kind: board.AuthorSession, Address: "s/def", Adapter: "crew-codex"}); got != "s/def (Codex)" {
		t.Errorf("no model = %q", got)
	}
}

func TestALabelledAgentIsKnownByItsLabel(t *testing.T) {
	session := board.Author{Kind: board.AuthorSession, Address: "s/74fc", Adapter: "crew-hermes", Model: "claude-opus-5-5", Label: "librarian"}
	if got := AuthorName(session); got != "librarian (Hermes, claude-opus-5-5, s/74fc)" {
		t.Errorf("session = %q", got)
	}
	seat := board.Author{Kind: board.AuthorSeat, Address: "crew.check", Session: "s/74fc", Adapter: "crew-hermes", Model: "claude-opus-5-5", Label: "librarian"}
	if got := AuthorName(seat); got != "crew.check (Hermes librarian, claude-opus-5-5, s/74fc)" {
		t.Errorf("seat = %q", got)
	}
}

func TestLastModelFindsAModelBeforeALongTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"turn_context","model":"gpt-6-astra"}` + "\n")
	// More than 4 MiB of output after the model, in lines that straddle the
	// reader's chunk boundaries.
	line := `{"type":"event","output":"` + strings.Repeat("x", 300_000) + `"}` + "\n"
	for written := 0; written < 5<<20; written += len(line) {
		_, _ = f.WriteString(line)
	}
	f.Close()
	extract := func(line []byte) string {
		var entry struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type == "turn_context" {
			return entry.Model
		}
		return ""
	}
	if got := LastModel(path, extract); got != "gpt-6-astra" {
		t.Fatalf("model = %q", got)
	}
}
