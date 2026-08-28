package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"crew-services/internal/review"
	"crew-services/internal/reviewcodex"
	"crew-services/internal/reviewden"
)

const defaultReviewProfile = "/home/agents/profiles/reviewer/SOUL.md"

type commandConfig struct {
	listen       string
	db           string
	capacity     int
	denURL       string
	denToken     string
	profile      string
	codexCommand string
	codexArgs    []string
	runInterval  time.Duration
}

type repeatString []string

func (s *repeatString) String() string { return strings.Join(*s, " ") }
func (s *repeatString) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "crew-review:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseConfig(args, os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := review.OpenSQLite(ctx, cfg.db, review.SystemClock{}, cfg.capacity)
	if err != nil {
		return err
	}
	defer store.Close()

	den, err := reviewden.New(cfg.denURL, cfg.denToken, nil)
	if err != nil {
		return err
	}
	runtime, err := reviewcodex.New(ctx, reviewcodex.Config{
		Command:     cfg.codexCommand,
		Args:        cfg.codexArgs,
		Capacity:    cfg.capacity,
		ProfilePath: cfg.profile,
	})
	if err != nil {
		return err
	}
	svc, err := review.New(store, den, runtime, cfg.profile, review.WithBackend("codex"))
	if err != nil {
		_ = runtime.Close()
		return err
	}

	logger := log.New(os.Stderr, "crew-review: ", log.LstdFlags)
	loopDone := startRunLoops(ctx, cfg.runInterval, cfg.capacity, svc.RunOne, func(err error) {
		if ctx.Err() == nil {
			logger.Printf("review job loop: %v", err)
		}
	})
	server := &http.Server{Addr: cfg.listen, Handler: review.NewHandler(svc)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	serverErr := server.ListenAndServe()
	stop()
	<-loopDone
	closeErr := svc.Close()
	if errors.Is(serverErr, http.ErrServerClosed) {
		return closeErr
	}
	if serverErr != nil {
		return serverErr
	}
	return closeErr
}

func parseConfig(args []string, getenv func(string) string) (commandConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := commandConfig{
		listen:       "127.0.0.1:8413",
		db:           "crew-review.db",
		capacity:     2,
		denURL:       envOr(getenv, "DEN_MCP_URL", reviewden.DefaultMCPURL),
		denToken:     getenv("DEN_MCP_TOKEN"),
		profile:      envOr(getenv, "CREW_REVIEW_PROFILE", defaultReviewProfile),
		codexCommand: envOr(getenv, "CODEX_COMMAND", "codex"),
		runInterval:  500 * time.Millisecond,
	}
	if raw := strings.TrimSpace(getenv("CREW_REVIEW_RUN_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return commandConfig{}, fmt.Errorf("parse CREW_REVIEW_RUN_INTERVAL: %w", err)
		}
		cfg.runInterval = interval
	}
	if raw := strings.TrimSpace(getenv("CREW_REVIEW_CAPACITY")); raw != "" {
		capacity, err := strconv.Atoi(raw)
		if err != nil {
			return commandConfig{}, fmt.Errorf("parse CREW_REVIEW_CAPACITY: %w", err)
		}
		cfg.capacity = capacity
	}
	var codexArgs repeatString
	flags := flag.NewFlagSet("crew-review", flag.ContinueOnError)
	flags.StringVar(&cfg.listen, "listen", cfg.listen, "loopback listen address")
	flags.StringVar(&cfg.db, "db", cfg.db, "separate review SQLite database")
	flags.IntVar(&cfg.capacity, "capacity", cfg.capacity, "maximum running jobs")
	flags.StringVar(&cfg.denURL, "den-mcp-url", cfg.denURL, "Den MCP endpoint")
	flags.StringVar(&cfg.denToken, "den-mcp-token", cfg.denToken, "Den MCP bearer token")
	flags.StringVar(&cfg.profile, "review-profile", cfg.profile, "reviewer profile file")
	flags.StringVar(&cfg.codexCommand, "codex-command", cfg.codexCommand, "Codex App Server executable")
	flags.Var(&codexArgs, "codex-arg", "Codex App Server argument; repeatable")
	flags.DurationVar(&cfg.runInterval, "run-interval", cfg.runInterval, "delay between bounded single-job runner passes")
	if err := flags.Parse(args); err != nil {
		return commandConfig{}, err
	}
	if len(codexArgs) > 0 {
		cfg.codexArgs = append([]string(nil), codexArgs...)
	}
	if strings.TrimSpace(cfg.listen) == "" {
		return commandConfig{}, errors.New("listen address is required")
	}
	if cfg.capacity < 1 {
		return commandConfig{}, errors.New("capacity must be positive")
	}
	if strings.TrimSpace(cfg.denURL) == "" {
		return commandConfig{}, errors.New("Den MCP URL is required")
	}
	if strings.TrimSpace(cfg.profile) == "" {
		return commandConfig{}, errors.New("review profile is required")
	}
	if strings.TrimSpace(cfg.codexCommand) == "" {
		return commandConfig{}, errors.New("Codex command is required")
	}
	if cfg.runInterval <= 0 {
		return commandConfig{}, errors.New("run interval must be positive")
	}
	return cfg, nil
}

func envOr(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

// startRunLoops starts a fixed number of bounded lanes. Each lane performs at
// most one durable job at a time; the store/runtime still enforce aggregate
// capacity and same-task serialization.
func startRunLoops(ctx context.Context, interval time.Duration, lanes int, run func(context.Context) (bool, error), report func(error)) <-chan struct{} {
	if lanes < 1 {
		lanes = 1
	}
	done := make(chan struct{})
	var group sync.WaitGroup
	group.Add(lanes)
	for i := 0; i < lanes; i++ {
		go func() {
			defer group.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := run(ctx); err != nil && ctx.Err() == nil && report != nil {
						report(err)
					}
				}
			}
		}()
	}
	go func() {
		group.Wait()
		close(done)
	}()
	return done
}

// startRunLoop is retained as a small one-lane helper for callers/tests that
// need only a single bounded worker.
func startRunLoop(ctx context.Context, interval time.Duration, run func(context.Context) (bool, error), report func(error)) <-chan struct{} {
	return startRunLoops(ctx, interval, 1, run, report)
}
