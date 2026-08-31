package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"crew-services/internal/reviewdsh"
)

func TestParseConfigUsesFlagsAndEnvironment(t *testing.T) {
	env := map[string]string{
		"DEN_MCP_URL":                  "https://den.example/mcp",
		"DEN_MCP_TOKEN":                "env-token",
		"CREW_REVIEW_PROFILE":          "/tmp/reviewer.md",
		"CREW_REVIEW_LISTEN":           "127.0.0.1:9001",
		"CREW_REVIEW_DB":               "/tmp/reviews.sqlite",
		"CREW_REVIEW_MODEL":            "gpt-5.6-sol",
		"CREW_REVIEW_REASONING_EFFORT": "medium",
		"CREW_REVIEW_BACKEND":          "codex",
		"CODEX_COMMAND":                "/usr/local/bin/codex",
		"CREW_REVIEW_CAPACITY":         "4",
		"CREW_REVIEW_RUN_INTERVAL":     "2s",
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
	if cfg.listen != "127.0.0.1:9001" || cfg.db != "/tmp/reviews.sqlite" || cfg.codexModel != "gpt-5.6-sol" || cfg.codexEffort != "medium" {
		t.Fatalf("service/reviewer config = %+v", cfg)
	}
	if cfg.capacity != 3 || cfg.runInterval != 250*time.Millisecond || cfg.codexCommand != "codex-test" {
		t.Fatalf("runtime config = %+v", cfg)
	}
	if len(cfg.codexArgs) != 2 || cfg.codexArgs[0] != "app-server" || cfg.codexArgs[1] != "--stdio" {
		t.Fatalf("Codex args = %#v", cfg.codexArgs)
	}
}

func TestParseConfigSelectsDSHBackend(t *testing.T) {
	env := map[string]string{
		"CREW_REVIEW_BACKEND": "dsh",
		"CREW_REVIEW_DSH_URL": "http://127.0.0.1:3080/plugins/dsh-crew-messaging/reviewer-runtime",
	}
	cfg, err := parseConfig(nil, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.backend != "dsh" || cfg.dshURL != env["CREW_REVIEW_DSH_URL"] {
		t.Fatalf("DSH config = %+v", cfg)
	}
	runtime, err := newRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.(*reviewdsh.Runtime); !ok {
		t.Fatalf("runtime = %T, want *reviewdsh.Runtime", runtime)
	}
}

func TestParseConfigRejectsUnknownOrIncompleteBackend(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown":         {"CREW_REVIEW_BACKEND": "pi"},
		"dsh missing URL": {"CREW_REVIEW_BACKEND": "dsh"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(nil, func(key string) string { return env[key] }); err == nil {
				t.Fatal("parseConfig succeeded")
			}
		})
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
