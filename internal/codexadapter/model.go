package codexadapter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"crew-services/internal/crewcli"
)

// CodexHome is CODEX_HOME, or ~/.codex.
func CodexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	user, _ := os.UserHomeDir()
	return filepath.Join(user, ".codex")
}

// ThreadModel is the model of a thread's newest turn, from its rollout file
// (CODEX_HOME/sessions/YYYY/MM/DD/rollout-…-THREAD.jsonl), or "".
func ThreadModel(home, threadID string) string {
	if threadID == "" || strings.ContainsAny(threadID, "/*?[") {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	return crewcli.LastModel(paths[len(paths)-1], rolloutModel)
}

// rolloutModel is the model a rollout's turn_context line names, or "".
func rolloutModel(line []byte) string {
	if !bytes.Contains(line, []byte(`"turn_context"`)) {
		return ""
	}
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Model string `json:"model"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "turn_context" {
		return ""
	}
	return entry.Payload.Model
}
