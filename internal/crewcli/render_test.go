package crewcli

import (
	"strings"
	"testing"
)

func TestRenderMessageNamesSenderAndEndsWithReplyCommand(t *testing.T) {
	got := RenderMessage("s/abc", "m1", "  gate failed \n", "crew-codex", &SessionInfo{Runtime: "codex", Cwd: "/home/agent/dev/rusty-engine"})
	if !strings.HasPrefix(got, "Crew message from s/abc, a Codex session in /home/agent/dev/rusty-engine (crew message m1):\n\ngate failed\n\n") {
		t.Fatalf("unexpected header/body: %q", got)
	}
	if !strings.Contains(got, "not from your user") || !strings.Contains(got, "memory") {
		t.Fatalf("missing the not-your-user notice: %q", got)
	}
	if !strings.HasSuffix(got, `To reply, run: crew-codex send --to s/abc "your reply"`) {
		t.Fatalf("missing reply line: %q", got)
	}
}

func TestRenderMessageWithUnknownSender(t *testing.T) {
	got := RenderMessage("crew-review", "m2", "check failed", "crew-claude", nil)
	if !strings.HasPrefix(got, "Crew message from crew-review (crew message m2):") {
		t.Fatalf("unexpected header: %q", got)
	}
}

func TestRenderMessageFromServiceOriginHasNoReplyLine(t *testing.T) {
	got := RenderMessage("system/crew-review", "m3", "gate failed", "crew-claude", nil)
	if strings.Contains(got, "To reply") {
		t.Fatalf("service notice offers a reply: %q", got)
	}
}
