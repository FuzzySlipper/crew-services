package crewcli

import (
	"strings"
	"testing"
)

func TestRenderMessageEndsWithReplyCommand(t *testing.T) {
	got := RenderMessage("s/abc", "m1", "  gate failed \n", "crew-codex")
	if !strings.HasPrefix(got, "Crew message from s/abc (crew message m1):\n\ngate failed\n\n") {
		t.Fatalf("unexpected header/body: %q", got)
	}
	if !strings.HasSuffix(got, `To reply, run: crew-codex send --to s/abc "your reply"`) {
		t.Fatalf("missing reply line: %q", got)
	}
}
