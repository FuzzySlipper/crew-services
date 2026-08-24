package codexadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// controlState is deliberately narrow. The fabric remains authoritative for
// public sessions and bindings; this only lets a restarted adapter remember
// which successful create operation owns which native thread.
type controlState struct {
	Created map[string]persistedControlSession `json:"created"`
}

type persistedControlSession struct {
	Session ControlSession `json:"session"`
	Mapping Mapping        `json:"mapping"`
}

func loadControlState(path string) (map[string]persistedControlSession, error) {
	if path == "" {
		return map[string]persistedControlSession{}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]persistedControlSession{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read control state: %w", err)
	}
	var state controlState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode control state: %w", err)
	}
	if state.Created == nil {
		state.Created = map[string]persistedControlSession{}
	}
	for operation, value := range state.Created {
		if operation == "" || value.Session.SessionID == "" || value.Mapping.Address == "" || value.Mapping.ThreadID == "" {
			return nil, fmt.Errorf("invalid control state entry %q", operation)
		}
	}
	return state.Created, nil
}

func saveControlState(path string, values map[string]persistedControlSession) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(controlState{Created: values}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode control state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create control state directory: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write control state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace control state: %w", err)
	}
	return nil
}
