package reviewcodex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCodex writes an executable that stands in for the codex command.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSandboxCheckRunsReadOnlyProbeInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	command := fakeCodex(t, `echo "$PWD $*"`)
	got := SandboxCheck{Command: command}.CheckRuntime(context.Background(), workspace)
	if !got.OK || got.Workspace != workspace {
		t.Fatalf("check = %+v", got)
	}
	if !strings.Contains(got.Detail, workspace+` sandbox -c sandbox_mode="read-only" git rev-parse HEAD`) {
		t.Fatalf("probe detail = %q", got.Detail)
	}
}

func TestSandboxCheckReportsSandboxFailure(t *testing.T) {
	command := fakeCodex(t, `echo "bwrap: Can't mkdir parents for /data/system/tmp/codex-daemon-1000: Permission denied" >&2; exit 1`)
	got := SandboxCheck{Command: command}.CheckRuntime(context.Background(), "")
	if got.OK || !strings.Contains(got.Detail, "bwrap: Can't mkdir parents") || !strings.Contains(got.Command, " true") {
		t.Fatalf("check = %+v", got)
	}
}

func TestSandboxCheckTimesOut(t *testing.T) {
	command := fakeCodex(t, `exec sleep 5`)
	got := SandboxCheck{Command: command, Timeout: 50 * time.Millisecond}.CheckRuntime(context.Background(), t.TempDir())
	if got.OK || !strings.Contains(got.Detail, "timed out") {
		t.Fatalf("check = %+v", got)
	}
}
