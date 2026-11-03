package crewcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crew-services/internal/board"
)

func TestASessionsPersonDefaultsToTheMachineAndCanBeSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CREW_PERSON", "")
	if got := PersonFor("native-1"); got != "" {
		t.Fatalf("with nothing set: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/crew"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/crew/person"), []byte("lore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := PersonFor("native-1"); got != "lore" {
		t.Fatalf("the machine's default: %q", got)
	}
	t.Setenv("CREW_PERSON", "patch")
	if got := PersonFor("native-1"); got != "patch" {
		t.Fatalf("CREW_PERSON: %q", got)
	}
	var out, errOut bytes.Buffer
	if code := whoami([]string{"--person", "lore"}, "native-1", &out, &errOut); code != 0 || PersonFor("native-1") != "lore" || PersonFor("native-2") != "patch" {
		t.Fatalf("whoami --person = %d %q %q", code, out.String(), errOut.String())
	}
	if code := whoami([]string{"--person", "Lore Smith"}, "native-1", &out, &errOut); code != 2 {
		t.Fatalf("a bad name = %d", code)
	}
	if code := whoami([]string{"--person", "lore"}, "", &out, &errOut); code != 1 {
		t.Fatalf("outside a session = %d", code)
	}
	if text := PersonContext("lore"); !strings.Contains(text, "You work for Lore") || PersonContext("") != "" {
		t.Fatalf("session-start text = %q", text)
	}
	seat := board.Author{Kind: board.AuthorSeat, Address: "rusty-engine.reviewer", Session: "s/5b69", Adapter: "claude-code", Model: "claude-opus-5-5", WorkingFor: "patch"}
	if got := AuthorName(seat); got != "rusty-engine.reviewer (Claude Code, claude-opus-5-5, working for Patch, s/5b69)" {
		t.Fatalf("author = %q", got)
	}
}
