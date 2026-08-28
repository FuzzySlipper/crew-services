package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseConfigUsesFlagsAndEnvironment(t *testing.T) {
	env := map[string]string{
		"DEN_MCP_URL":              "https://den.example/mcp",
		"DEN_MCP_TOKEN":            "env-token",
		"CREW_REVIEW_PROFILE":      "/tmp/reviewer.md",
		"CODEX_COMMAND":            "/usr/local/bin/codex",
		"CREW_REVIEW_CAPACITY":     "4",
		"CREW_REVIEW_RUN_INTERVAL": "2s",
	}
	getenv := func(name string) string { return env[name] }
	cfg, err := parseConfig([]string{
		"-den-mcp-token", "flag-token",
		"-capacity", "3",
		"-run-interval", "250ms",
		"-codex-command", "codex-test",
		"-codex-arg", "app-server",
		"-codex-arg", "--stdio",
	}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.denURL != "https://den.example/mcp" || cfg.denToken != "flag-token" || cfg.profile != "/tmp/reviewer.md" {
		t.Fatalf("Den/profile config = %+v", cfg)
	}
	if cfg.capacity != 3 || cfg.runInterval != 250*time.Millisecond || cfg.codexCommand != "codex-test" {
		t.Fatalf("runtime config = %+v", cfg)
	}
	if len(cfg.codexArgs) != 2 || cfg.codexArgs[0] != "app-server" || cfg.codexArgs[1] != "--stdio" {
		t.Fatalf("Codex args = %#v", cfg.codexArgs)
	}
}

func TestParseConfigRejectsInvalidRunnerValues(t *testing.T) {
	for name, args := range map[string][]string{
		"capacity":     {"-capacity", "0"},
		"interval":     {"-run-interval", "0s"},
		"den endpoint": {"-den-mcp-url", ""},
		"profile":      {"-review-profile", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(args, func(string) string { return "" }); err == nil {
				t.Fatal("parseConfig succeeded")
			}
		})
	}
}

func TestStartRunLoopIsBoundedAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	reported := make(chan error, 1)
	done := startRunLoop(ctx, time.Millisecond, func(context.Context) (bool, error) {
		calls.Add(1)
		return true, errors.New("expected test error")
	}, func(err error) { reported <- err })
	select {
	case err := <-reported:
		if err.Error() != "expected test error" {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner loop did not make a bounded pass")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner loop did not stop")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want one completed bounded pass", got)
	}
}

func TestStartRunLoopsUsesConfiguredBoundedLanes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	done := startRunLoops(ctx, time.Millisecond, 3, func(context.Context) (bool, error) {
		started <- struct{}{}
		<-release
		return true, nil
	}, nil)
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("bounded lane %d did not start", i)
		}
	}
	if len(started) != 0 {
		t.Fatalf("more than three lanes started before release: %d", len(started))
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded lanes did not stop")
	}
}
