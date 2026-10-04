package hosting

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// RustyDevEndReason explains an Engine product host that ended by itself. The
// rusty CLI keeps the record of a dev session that ended for a reason a caller
// must see (idle-expired, port-unavailable, project-removed) for a day, and
// `rusty dev list --all --json` lists it with its label. A crash leaves no
// record, and a rusty without the command lists nothing; both give "".
func RustyDevEndReason(rusty string) EndReason {
	return func(ctx context.Context, label string) string {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, rusty, "dev", "list", "--all", "--json").Output()
		if err != nil {
			return ""
		}
		return endedState(output, label)
	}
}

// endedState is the state of the last ended record with label.
func endedState(listing []byte, label string) string {
	var records []map[string]any
	if json.Unmarshal(listing, &records) != nil {
		return ""
	}
	reason := ""
	for _, record := range records {
		state, _ := record["state"].(string)
		if record["label"] == label && record["endedAt"] != nil && state != "" {
			reason = state
		}
	}
	return reason
}

// FindRusty locates the rusty CLI: on PATH, else in ~/.local/bin, where
// `rusty` installs itself. It returns "" when there is none.
func FindRusty() string {
	if path, err := exec.LookPath("rusty"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".local", "bin", "rusty")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}
