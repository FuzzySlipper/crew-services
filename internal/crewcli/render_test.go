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

func TestRenderMailToASeatOffersAReplyAsTheSeat(t *testing.T) {
	got := RenderMail(Mail{Sender: "s/abc", MessageID: "m4", Body: "review this", Recipient: "crew.reviewer"}, "crew-claude", nil)
	if !strings.HasPrefix(got, "Crew message to your seat crew.reviewer from s/abc (crew message m4):") {
		t.Fatalf("unexpected header: %q", got)
	}
	if !strings.HasSuffix(got, `To reply as the seat, run: crew-claude send --as crew.reviewer --to s/abc "your reply"`) {
		t.Fatalf("missing seat reply line: %q", got)
	}
}

func TestRenderMailFromASeatNamesTheHolder(t *testing.T) {
	got := RenderMail(Mail{Sender: "crew.lead", SenderSession: "s/xyz", MessageID: "m5", Body: "ship it"}, "crew-codex", &SessionInfo{Runtime: "claude-code", Cwd: "/w"})
	if !strings.HasPrefix(got, "Crew message from the seat crew.lead, held by s/xyz, a Claude Code session in /w (crew message m5):") || !strings.HasSuffix(got, `crew-codex send --to crew.lead "your reply"`) {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestSeatNoticeHasNoReplyLine(t *testing.T) {
	if got := RenderMail(Mail{Sender: "fabric/seats", MessageID: "m6", Body: "You no longer hold the seat"}, "crew-claude", nil); strings.Contains(got, "To reply") {
		t.Fatalf("seat notice offers a reply: %q", got)
	}
}

func TestResolveTakesASeatAddressAsIs(t *testing.T) {
	if got, err := Resolve("crew.reviewer", nil); err != nil || got != "crew.reviewer" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
}
