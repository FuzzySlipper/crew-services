package board

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMentionCandidates(t *testing.T) {
	body := "Thanks @patch and @rusty-engine.reviewer — cc @s/5b693970244d98db, @kate@interlake.\n" +
		"Not mentions: me@example.com, `@lore in code`, a/@x, and\n```\n@fenced.block\n```\nRepeat: @patch."
	var got []string
	for _, c := range mentionCandidates(body) {
		got = append(got, c.Text+"|"+c.Name+"|"+c.Site)
	}
	want := []string{"@patch|patch|", "@rusty-engine.reviewer|rusty-engine.reviewer|", "@s/5b693970244d98db|s/5b693970244d98db|", "@kate@interlake|kate|interlake"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %q\nwant %q", got, want)
	}
	for name, kind := range map[string]string{"patch": AuthorPerson, "rt-12.skeptic": AuthorSeat, "s/abc": AuthorSession} {
		if got := participantKind(name); got != kind {
			t.Errorf("participantKind(%q) = %s, want %s", name, got, kind)
		}
	}
}

func TestRenderForAgentFoldsAndImages(t *testing.T) {
	body := "Summary first.\n\n<details><summary>Benchmark details</summary>\nline one\nline two\nline three\n</details>\n\n" +
		`![cache layout](blob:sha256-3f2a "per-material index") and ![logo](https://example.com/x.png)`
	folded := RenderForAgent(body, false)
	for _, want := range []string{"[folded: Benchmark details, 3 lines; --unfold to show]", "[image: cache layout; caption per-material index; board blob 3f2a]", "[image: logo; link https://example.com/x.png]"} {
		if !strings.Contains(folded, want) {
			t.Fatalf("folded render lacks %q:\n%s", want, folded)
		}
	}
	if strings.Contains(folded, "line two") {
		t.Fatalf("folded text leaked: %s", folded)
	}
	unfolded := RenderForAgent(body, true)
	if !strings.Contains(unfolded, "[Benchmark details]\nline one\nline two\nline three\n[end of Benchmark details]") {
		t.Fatalf("unfolded render:\n%s", unfolded)
	}
}

func TestNewIDSortsByTimeAndCarriesTheSite(t *testing.T) {
	early, late := NewID("den", time.UnixMilli(1_700_000_000_000)), NewID("den", time.UnixMilli(1_700_000_000_001))
	if !strings.HasPrefix(early, "den-") || len(early) != len("den-")+26 || early >= late {
		t.Fatalf("ids %s %s", early, late)
	}
	if NewID("den", time.UnixMilli(5)) == NewID("den", time.UnixMilli(5)) {
		t.Fatal("two ids in the same millisecond collided")
	}
}

func TestFoldsQuotedInCodeStayAsWrittenForAgents(t *testing.T) {
	body := "Like this:\n\n```html\n<details><summary>Example</summary>text</details>\n```\n\n" +
		"or `<details><summary>Inline</summary>x</details>`.\n\n<details><summary>Real</summary>\nhidden\n</details>"
	got := RenderForAgent(body, false)
	for _, want := range []string{"<details><summary>Example</summary>text</details>\n```", "`<details><summary>Inline</summary>x</details>`", "[folded: Real, 1 line; --unfold to show]"} {
		if !strings.Contains(got, want) {
			t.Errorf("agent text lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hidden") {
		t.Errorf("the real fold was not folded:\n%s", got)
	}
}
