package reviewcodex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"crew-services/internal/review"
)

// SandboxCheck runs one command in the same read-only Codex sandbox that
// reviewer turns use. A broken sandbox fails every reviewer command while the
// turn itself still completes, so this is the direct way to see it.
type SandboxCheck struct {
	// Command is the Codex executable; empty means "codex".
	Command string
	// Timeout bounds the probe; zero means 30 seconds.
	Timeout time.Duration
}

const sandboxCheckOutputLimit = 2000

// CheckRuntime runs `git rev-parse HEAD` in workspace inside the sandbox, or
// `true` in the home directory when no review has run yet.
func (c SandboxCheck) CheckRuntime(ctx context.Context, workspace string) review.RuntimeCheck {
	command := strings.TrimSpace(c.Command)
	if command == "" {
		command = "codex"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	probe := []string{"git", "rev-parse", "HEAD"}
	dir := workspace
	if dir == "" {
		probe = []string{"true"}
		dir, _ = os.UserHomeDir()
	}
	args := append([]string{"sandbox", "-c", `sandbox_mode="read-only"`}, probe...)
	result := review.RuntimeCheck{Workspace: dir, Command: command + " " + strings.Join(args, " ")}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := exec.CommandContext(ctx, command, args...)
	run.Dir = dir
	output, err := run.CombinedOutput()
	detail := strings.TrimSpace(string(output))
	if len(detail) > sandboxCheckOutputLimit {
		detail = strings.ToValidUTF8(detail[len(detail)-sandboxCheckOutputLimit:], "")
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Detail = "sandbox probe timed out after " + timeout.String()
		if detail != "" {
			result.Detail += ": " + detail
		}
	case err != nil:
		result.Detail = "reviewer sandbox cannot run commands: " + err.Error()
		if detail != "" {
			result.Detail += ": " + detail
		}
	default:
		result.OK = true
		result.Detail = "reviewer sandbox ran the probe"
		if detail != "" {
			result.Detail += ": " + detail
		}
	}
	return result
}
