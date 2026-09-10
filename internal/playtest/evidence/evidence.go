// Package evidence writes durable playtest evidence without replacing earlier
// artifacts when a new publication fails.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SyncWriter is the minimum durable sink for one JSONL publication.
type SyncWriter interface {
	io.Writer
	Sync() error
}

// Journal keeps one append-only evidence file open for a script run.
type Journal struct {
	file *os.File
}

func OpenJournal(path string) (*Journal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Journal{file: file}, nil
}

func (journal *Journal) Append(value any) error { return AppendJSON(journal.file, value) }

func (journal *Journal) Close() error { return journal.file.Close() }

// AppendJSON writes exactly one newline-terminated JSON record and syncs it.
// A short write is reported because it leaves a recoverable, incomplete final
// journal record rather than a complete publication.
func AppendJSON(writer SyncWriter, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode journal record: %w", err)
	}
	if err := writeAll(writer, append(data, '\n')); err != nil {
		return fmt.Errorf("write journal record: %w", err)
	}
	if err := writer.Sync(); err != nil {
		return fmt.Errorf("sync journal record: %w", err)
	}
	return nil
}

// AppendJSONFile opens, appends, syncs, and closes one journal record.
func AppendJSONFile(path string, value any) error {
	journal, err := OpenJournal(path)
	if err != nil {
		return err
	}
	appendErr := journal.Append(value)
	closeErr := journal.Close()
	if appendErr != nil {
		return appendErr
	}
	return closeErr
}

// WriteJSONFile publishes a complete JSON document by first syncing a uniquely
// named sibling and then replacing the destination. Existing evidence is left
// unchanged if encoding, writing, or syncing the replacement fails.
func WriteJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON state: %w", err)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	writeErr := writeAll(temporary, append(data, '\n'))
	if writeErr == nil {
		writeErr = temporary.Sync()
	}
	closeErr := temporary.Close()
	if writeErr != nil {
		return fmt.Errorf("write JSON state: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close JSON state: %w", closeErr)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish JSON state: %w", err)
	}
	if err := SyncDirectory(directory); err != nil {
		return fmt.Errorf("sync JSON state directory: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return errors.New("writer reported an invalid byte count")
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// SyncDirectory commits directory entry changes such as a renamed screenshot
// or atomic state replacement.
func SyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
