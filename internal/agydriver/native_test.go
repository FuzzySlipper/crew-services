package agydriver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStatHandlesSpacesInTheCommandName(t *testing.T) {
	comm, ppid, start, err := parseStat("4242 (agy (x) y) S 4100 4242 4100 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 12 0 987654 0 0")
	if err != nil || comm != "agy (x) y" || ppid != 4100 || start != 987654 {
		t.Fatalf("comm=%q ppid=%d start=%d err=%v", comm, ppid, start, err)
	}
}

func TestFindAgyWalksUpThroughTheShell(t *testing.T) {
	root := t.TempDir()
	write := func(pid, stat string) {
		dir := filepath.Join(root, pid)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644)
	}
	rest := " 0 0 0 0 0 0 0 0 0 0 0 0 0 20 0 1 0 555 0 0"
	write("10", "10 (agy) S 1"+rest)
	write("20", "20 (sh) S 10"+rest)
	procs := Procs{Root: root}
	proc, ok := procs.FindAgy(20)
	if !ok || proc != (Proc{PID: 10, Start: 555}) {
		t.Fatalf("proc=%#v ok=%v", proc, ok)
	}
	if !procs.Alive(proc) || procs.Alive(Proc{PID: 10, Start: 1}) {
		t.Fatal("Alive does not check the start time")
	}
}

func TestListeningPortsReadsLoopbackListenersOfTheProcess(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:9E5B 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:B0A5 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 222 1 0000000000000000 100 0 0 10 0
   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 333 1 0000000000000000 100 0 0 10 0
   3: 0100007F:9E5C 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 444 1 0000000000000000 100 0 0 10 0
`
	got := listeningPorts(table, map[string]bool{"111": true, "333": true, "444": true})
	if !reflect.DeepEqual(got, []int{0x9E5B}) {
		t.Fatalf("ports=%v", got)
	}
}

func TestConversationArg(t *testing.T) {
	for args, want := range map[string]string{
		"--conversation abc":         "abc",
		"--conversation=abc --model": "abc",
		"-c":                         "",
		"--prompt hi":                "",
	} {
		if got := ConversationArg(strings.Fields(args)); got != want {
			t.Errorf("%q: got %q want %q", args, got, want)
		}
	}
}

func TestTranscriptCountsOnlySystemMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "transcript_full.jsonl")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	marker := "(crew message m-1)"
	lines := []map[string]string{
		{"source": "MODEL", "type": "RUN_COMMAND", "content": "echo " + marker},
		{"source": "MODEL", "type": "PLANNER_RESPONSE", "content": "I saw " + marker, "model": "gemini-4-argon-high"},
	}
	var text []byte
	for _, l := range lines {
		b, _ := json.Marshal(l)
		text = append(append(text, b...), '\n')
	}
	_ = os.WriteFile(path, text, 0o644)
	scan, err := scanTranscript(path, 0, marker)
	if err != nil || scan.Inserted || scan.Offset != int64(len(text)) {
		t.Fatalf("scan=%#v err=%v", scan, err)
	}
	b, _ := json.Marshal(map[string]string{"source": "SYSTEM", "type": "SYSTEM_MESSAGE", "content": "[Message] content=Crew message from s/x " + marker})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if scan, _ = scanTranscript(path, scan.Offset, marker); !scan.Inserted {
		t.Fatal("system message step not found")
	}
	if model := transcriptModel(text[:len(text)-1][strings.IndexByte(string(text), '\n')+1:]); model != "gemini-4-argon-high" {
		t.Fatalf("model=%q", model)
	}
}

func TestQueuedReadsTheMessageFolder(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, ".system_generated", "logs", "transcript_full.jsonl")
	messages := filepath.Join(dir, ".system_generated", "messages")
	_ = os.MkdirAll(messages, 0o755)
	_ = os.WriteFile(filepath.Join(messages, "read.json"), []byte(`{"x":true}`), 0o644)
	if queued(transcript, "(crew message m-1)") {
		t.Fatal("queued with no message file")
	}
	_ = os.WriteFile(filepath.Join(messages, "x.json"), []byte(`{"id":"x","content":"hi (crew message m-1)"}`), 0o644)
	if !queued(transcript, "(crew message m-1)") {
		t.Fatal("message file not found")
	}
}

func TestFillNotifyRewritesOnlyDenSubmissions(t *testing.T) {
	args := func(server, tool string, arguments any) map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for k, v := range map[string]any{"ServerName": server, "ToolName": tool, "Arguments": arguments} {
			out[k], _ = json.Marshal(v)
		}
		return out
	}
	in := HookInput{ConversationID: "conv-1"}
	in.ToolCall.Name = "call_mcp_tool"
	// Other tools and servers: no output at all, which agy reads as no
	// opinion (an empty object would deny the call).
	in.ToolCall.Args = args("den-wiki", "submit_task_for_review", map[string]any{})
	if out := fillNotify(t.Context(), hookClient{base: "http://127.0.0.1:1"}, in, HookOptions{}); out != nil {
		t.Fatalf("other server: %s", out)
	}
	in.ToolCall.Args = args("den", "submit_task_for_review", map[string]any{"notify_address": "s/given"})
	if out := fillNotify(t.Context(), hookClient{base: "http://127.0.0.1:1"}, in, HookOptions{}); out != nil {
		t.Fatalf("already set: %s", out)
	}
}
