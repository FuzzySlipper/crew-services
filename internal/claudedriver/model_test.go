package claudedriver

import "testing"

func TestTranscriptModelTakesAssistantLinesOnly(t *testing.T) {
	cases := map[string]string{
		`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[]}}`: "claude-opus-5-5",
		`{"type":"assistant","message":{"model":"<synthetic>"}}`:                  "",
		`{"type":"user","message":{"content":"the assistant said model x"}}`:      "",
		`not json "assistant"`: "",
	}
	for line, want := range cases {
		if got := transcriptModel([]byte(line)); got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
}
