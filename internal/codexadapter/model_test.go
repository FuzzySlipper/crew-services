package codexadapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestThreadModelReadsTheRolloutsNewestTurn(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := `{"type":"session_meta","payload":{"id":"th-1"}}
{"type":"turn_context","payload":{"model":"gpt-6-astra"}}
{"type":"response_item","payload":{"type":"message","model":"not-this"}}
{"type":"turn_context","payload":{"model":"gpt-6.1-sol"}}
{"type":"event_msg","payload":{}}
`
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-02T05-00-00-th-1.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ThreadModel(home, "th-1"); got != "gpt-6.1-sol" {
		t.Fatalf("model = %q", got)
	}
	if got := ThreadModel(home, "th-2"); got != "" {
		t.Fatalf("a thread with no rollout gave %q", got)
	}
	if got := ThreadModel(home, "*"); got != "" {
		t.Fatalf("a glob as thread id gave %q", got)
	}
}
