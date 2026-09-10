package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type shortWriter struct {
	bytes.Buffer
	limit   int
	written bool
}

func (writer *shortWriter) Write(data []byte) (int, error) {
	if writer.written || writer.limit <= 0 {
		return 0, nil
	}
	if len(data) > writer.limit {
		data = data[:writer.limit]
	}
	writer.written = true
	return writer.Buffer.Write(data)
}

func (writer *shortWriter) Sync() error { return nil }

func TestAppendJSONRejectsAndPreservesPartialRecord(t *testing.T) {
	writer := &shortWriter{limit: 8}
	err := AppendJSON(writer, map[string]any{"event": "final"})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("append error = %v, want short write", err)
	}
	if writer.Len() == 0 || bytes.Contains(writer.Bytes(), []byte("\n")) {
		t.Fatalf("partial publication was not retained as an incomplete tail: %q", writer.Bytes())
	}
}

func TestWriteJSONFilePreservesPriorArtifactOnEncodeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{\"before\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONFile(path, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unsupported JSON value was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\"before\":true}\n" {
		t.Fatalf("prior artifact changed: %q", data)
	}
}

func TestWriteJSONFilePublishesParseableDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteJSONFile(path, map[string]any{"phase": "stopped"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]string
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("published document is not JSON: %v", err)
	}
	if state["phase"] != "stopped" {
		t.Fatalf("state = %#v", state)
	}
}
