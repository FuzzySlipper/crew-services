package devserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstanceHostGetsInstanceLabelAndKeepArgsAndStopsThroughItsStopCommand(t *testing.T) {
	cfg := testConfig(t)
	manager := newTestManager(t, cfg)
	repoRoot := t.TempDir()
	writeServeManifest(t, repoRoot, "alpha", map[string]any{
		"command":      helperCommand("server"),
		"healthUrl":    "/health",
		"readyText":    "alpha-ready",
		"instanceArgs": "--instance {instance} --label {label}",
		"keepArgs":     "--keep",
		"stopCommand":  `printf '%s %s' "$DEN_SERVE_INSTANCE" {label} > {session_dir}/stop-requested`,
	})
	up, err := manager.Up(t.Context(), UpOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-1", Label: "crew-playtest:play-1", Keep: true})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	session := up.Session
	if !strings.HasSuffix(session.Command, " --instance play-1 --label crew-playtest:play-1 --keep") {
		t.Fatalf("Command = %q, want instance, label and keep args appended", session.Command)
	}
	if session.Label != "crew-playtest:play-1" || !session.Keep || !strings.Contains(session.StopCommand, "crew-playtest:play-1") {
		t.Fatalf("session owner not recorded: label=%q keep=%v stop=%q", session.Label, session.Keep, session.StopCommand)
	}
	if _, alive, err := manager.Running(t.Context(), StatusOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-1"}); err != nil || !alive {
		t.Fatalf("Running() = %v, %v; want alive", alive, err)
	}
	stopped, err := manager.Stop(t.Context(), StopOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-1"})
	if err != nil || !stopped.Stopped {
		t.Fatalf("Stop() = %+v, %v", stopped, err)
	}
	marker, err := os.ReadFile(filepath.Join(session.SessionDir, "stop-requested"))
	if err != nil || string(marker) != "play-1 crew-playtest:play-1" {
		t.Fatalf("stop command did not run with the session's identity: %q %v", marker, err)
	}
	if _, alive, err := manager.Running(t.Context(), StatusOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-1"}); err != nil || alive {
		t.Fatalf("Running() after stop = %v, %v; want gone", alive, err)
	}
}

func TestOrdinarySessionGetsNoInstanceArgs(t *testing.T) {
	cfg := testConfig(t)
	manager := newTestManager(t, cfg)
	repoRoot := t.TempDir()
	writeServeManifest(t, repoRoot, "alpha", map[string]any{
		"command":      helperCommand("server"),
		"healthUrl":    "/health",
		"readyText":    "alpha-ready",
		"instanceArgs": "--instance {instance}",
	})
	up, err := manager.Up(t.Context(), UpOptions{Project: "alpha", RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	defer stopSession(t, manager, up.Session)
	if strings.Contains(up.Session.Command, "--instance") || up.Session.Label != "" {
		t.Fatalf("ordinary session got instance arguments: %q label %q", up.Session.Command, up.Session.Label)
	}
}

func TestUpRefusesInstanceAndLabelThatCannotReachACommandLineSafely(t *testing.T) {
	manager := newTestManager(t, testConfig(t))
	repoRoot := t.TempDir()
	writeServeManifest(t, repoRoot, "alpha", map[string]any{
		"command":   helperCommand("server"),
		"healthUrl": "/health",
		"readyText": "alpha-ready",
	})
	for _, options := range []UpOptions{
		{Project: "alpha", RepoRoot: repoRoot, Instance: "a b"},
		{Project: "alpha", RepoRoot: repoRoot, Instance: strings.Repeat("x", 65)},
		{Project: "alpha", RepoRoot: repoRoot, Instance: "ok", Label: "x;rm -rf ~"},
	} {
		if _, err := manager.Up(t.Context(), options); err == nil {
			t.Fatalf("Up(%+v) accepted an unsafe owner", options)
		}
	}
}

func TestFailingStopCommandStillStopsTheProcessGroup(t *testing.T) {
	cfg := testConfig(t)
	manager := newTestManager(t, cfg)
	repoRoot := t.TempDir()
	writeServeManifest(t, repoRoot, "alpha", map[string]any{
		"command":     helperCommand("server"),
		"healthUrl":   "/health",
		"readyText":   "alpha-ready",
		"stopCommand": "echo no such session >&2; exit 3",
	})
	up, err := manager.Up(t.Context(), UpOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-2"})
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	stopped, err := manager.Stop(t.Context(), StopOptions{Project: "alpha", RepoRoot: repoRoot, Instance: "play-2"})
	if err != nil || !stopped.Stopped {
		t.Fatalf("Stop() = %+v, %v", stopped, err)
	}
	if !strings.Contains(stopped.Message, "stop command failed") || !strings.Contains(stopped.Message, "no such session") {
		t.Fatalf("Message = %q, want the stop command's failure", stopped.Message)
	}
	if processGroupAlive(up.Session.PID) {
		t.Fatal("process group survived a failed stop command")
	}
}

func TestPruneEndedRemovesOldEndedSessionsAndEarlierLaunches(t *testing.T) {
	root := t.TempDir()
	store := NewSessionStore(root)
	now := time.Now().UTC()
	old := now.Add(-100 * time.Hour)
	live := syscall.Getpgrp()
	write := func(key, launch string, state SessionState, age time.Time) string {
		t.Helper()
		state.SessionKey = key
		state.StatePath = store.CurrentPath(key)
		state.SessionDir = store.SessionDir(key, launch)
		state.StartedAt, state.LastCheckedAt = age, age
		if err := os.MkdirAll(state.SessionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteCurrent(state); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{state.SessionDir, state.StatePath, filepath.Dir(state.StatePath)} {
			if err := os.Chtimes(path, age, age); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(root, key)
	}
	oldEnded := write("alpha--old", "s1", SessionState{Ownership: "broker_owned", PID: 99999999, Status: "stopped"}, old)
	recentEnded := write("alpha--recent", "s1", SessionState{Ownership: "broker_owned", PID: 99999999, Status: "stopped"}, now)
	running := write("alpha", "s2", SessionState{Ownership: "broker_owned", PID: live, Status: "running"}, old)
	earlier := filepath.Join(running, "s1")
	if err := os.MkdirAll(earlier, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(earlier, old, old)
	unowned := write("beta", "s1", SessionState{Ownership: "unowned", Status: "unowned"}, old)
	corrupt := filepath.Join(root, "gamma--broken")
	if err := os.MkdirAll(corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(corrupt, old, old)

	store.PruneEnded(now, 72*time.Hour)

	for path, want := range map[string]bool{
		oldEnded:                     false,
		recentEnded:                  true,
		filepath.Join(running, "s2"): true,
		earlier:                      false,
		filepath.Join(unowned, "s1"): true,
		corrupt:                      false,
	} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", path, err == nil, want)
		}
	}
}

func TestListDropsDeadLeases(t *testing.T) {
	cfg := testConfig(t)
	manager := newTestManager(t, cfg)
	registry := NewLeaseRegistry(cfg.StateDir)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save([]LeaseRecord{{SessionKey: "dead", PID: 99999999}, {SessionKey: "live", PID: syscall.Getpgrp()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.List(t.Context()); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file leaseFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Leases) != 1 || file.Leases[0].SessionKey != "live" {
		t.Fatalf("leases after list = %+v, want only the live one", file.Leases)
	}
}
