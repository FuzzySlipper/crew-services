package claudedriver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// Claude Code 2.1.x records what its inbox did with a message in the
// session's transcript (~/.claude/projects/<cwd slug>/<session id>.jsonl):
//
//   - {"type":"queue-operation","operation":"enqueue","content":"<text>"} when
//     the inbox accepts the frame into the session's in-memory queue. It
//     carries the text but not the msg_id.
//   - the message entering the conversation, carrying the frame's msg_id as
//     origin.msg_id: a {"type":"attachment","attachment":{"type":
//     "queued_command","origin":{"kind":"peer","msg_id":…}}} line when an
//     active turn absorbs it at a step boundary, or a {"type":"user",
//     "origin":{"kind":"peer","msg_id":…}} line when it starts a turn.
//
// A message the session held, refused or dropped produces neither, and the
// queue is lost if the process exits before the message enters the
// conversation.

// transcriptScan is what one pass over new transcript lines found.
type transcriptScan struct {
	// Inserted: a line carries the msg_id as the origin of a message in the
	// conversation.
	Inserted bool
	// Enqueued: an enqueue line carries the message's text.
	Enqueued bool
	// Offset is where the next pass resumes: after the last complete line.
	Offset int64
}

// scanTranscript reads complete lines of path from offset, looking for the
// native msgID and for an enqueue of text containing marker. A missing file
// reads as empty.
func scanTranscript(path string, offset int64, msgID, marker string) (transcriptScan, error) {
	out := transcriptScan{Offset: offset}
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
	id := []byte(msgID)
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
		switch {
		case bytes.Contains(line, id) && carriesMsgID(line, msgID):
			out.Inserted = true
			return out, nil
		case marker != "" && bytes.Contains(line, mark) && isEnqueue(line, marker):
			out.Enqueued = true
		}
	}
}

// carriesMsgID reports whether line is a conversation entry whose peer origin
// is msgID. The ID appearing anywhere else (a tool result quoting it, say)
// does not count.
func carriesMsgID(line []byte, msgID string) bool {
	var entry struct {
		Type   string `json:"type"`
		Origin struct {
			MsgID string `json:"msg_id"`
		} `json:"origin"`
		Attachment struct {
			Origin struct {
				MsgID string `json:"msg_id"`
			} `json:"origin"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return false
	}
	switch entry.Type {
	case "user":
		return entry.Origin.MsgID == msgID
	case "attachment":
		return entry.Attachment.Origin.MsgID == msgID
	}
	return false
}

func isEnqueue(line []byte, marker string) bool {
	var entry struct {
		Type      string `json:"type"`
		Operation string `json:"operation"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return false
	}
	return entry.Type == "queue-operation" && entry.Operation == "enqueue" && bytes.Contains([]byte(entry.Content), []byte(marker))
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// defaultTranscript is where Claude Code keeps a session's transcript when
// the hook did not say: <projects dir>/<cwd with every other character
// replaced by '-'>/<session id>.jsonl.
func defaultTranscript(projectsDir, cwd, sessionID string) string {
	if projectsDir == "" || cwd == "" || sessionID == "" {
		return ""
	}
	return filepath.Join(projectsDir, slugUnsafe.ReplaceAllString(cwd, "-"), sessionID+".jsonl")
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
