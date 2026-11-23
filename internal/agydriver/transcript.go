package agydriver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// agy 1.5.15 keeps each conversation under
// ~/.gemini/antigravity-cli/brain/<conversation id>/.system_generated/:
//
//   - logs/transcript_full.jsonl, one JSON object per step: {"step_index",
//     "source", "type", "status", "created_at", "content", …}. Hooks are given
//     its path as transcriptPath.
//   - messages/<uuid>.json, one file per message sent to the conversation
//     ({"id", "recipient", "sender", "content", …}), and messages/read.json,
//     a map of the ids the conversation has read. An undelivered/<uuid> marker
//     sits beside a message not yet handed over.
//
// A message the driver posts with SendAgentMessage enters the conversation as
// {"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"…[Message] timestamp=…
// sender=system priority=MESSAGE_PRIORITY_HIGH content=<text>…"}. One the
// driver returns from a PreInvocation hook enters as {"source":"SYSTEM_SDK",
// "type":"SYSTEM_MESSAGE","content":"…injected by a PreInvocation hook:\n<text>…"}.
// Neither carries an id of the driver's choosing, so both are matched by the
// "(crew message <id>)" marker in the rendered text. A tool's output quoting
// the marker is a different step type and does not count.

// transcriptScan is what one pass over new transcript lines found.
type transcriptScan struct {
	// Inserted: a system message step carries the marker.
	Inserted bool
	// Offset is where the next pass resumes: after the last complete line.
	Offset int64
}

// scanTranscript reads complete lines of path from offset, looking for a
// system message step that carries marker. A missing file reads as empty.
func scanTranscript(path string, offset int64, marker string) (transcriptScan, error) {
	out := transcriptScan{Offset: offset}
	if path == "" || marker == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() < offset {
		// The file was replaced or truncated: read it again from the start.
		out.Offset = 0
	}
	if _, err := f.Seek(out.Offset, io.SeekStart); err != nil {
		return out, err
	}
	mark := []byte(marker)
	reader := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// A partial last line is still being written; read it next pass.
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.Offset += int64(len(line))
		if bytes.Contains(line, mark) && isSystemMessage(line, marker) {
			out.Inserted = true
			return out, nil
		}
	}
}

// isSystemMessage reports whether line is a system message step whose text
// carries marker: a posted message or hook-injected context.
func isSystemMessage(line []byte, marker string) bool {
	var step struct {
		Type    string `json:"type"`
		Source  string `json:"source"`
		Content string `json:"content"`
	}
	if json.Unmarshal(line, &step) != nil || step.Type != "SYSTEM_MESSAGE" {
		return false
	}
	return strings.Contains(step.Content, marker)
}

// messagesDir is the conversation's message folder, beside the logs folder
// that holds the transcript.
func messagesDir(transcript string) string {
	if transcript == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Dir(transcript)), "messages")
}

// queued reports whether the conversation's message folder holds a message
// whose content carries marker: the language server took it in.
func queued(transcript, marker string) bool {
	dir := messagesDir(transcript)
	if dir == "" || marker == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || name == "read.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Contains(data, []byte(marker)) {
			continue
		}
		var msg struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(data, &msg) == nil && strings.Contains(msg.Content, marker) {
			return true
		}
	}
	return false
}

// transcriptModel is the model a transcript step names, or "".
func transcriptModel(line []byte) string {
	if !bytes.Contains(line, []byte(`"model"`)) {
		return ""
	}
	var step struct {
		Source string `json:"source"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(line, &step) != nil || step.Source != "MODEL" {
		return ""
	}
	return step.Model
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
