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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"crew-services/internal/fabricclient"
	"crew-services/internal/review"
	"crew-services/internal/reviewcodex"
	"crew-services/internal/reviewden"
	"crew-services/internal/reviewdsh"
	"crew-services/internal/reviewhook"
)

const defaultReviewProfile = "/home/system/crew-services/reviewer.md"

type commandConfig struct {
	listen         string
	db             string
	capacity       int
	denURL         string
	denToken       string
	profile        string
	backend        string
	dshURL         string
	codexModel     string
	codexEffort    string
	codexCommand   string
	codexArgs      []string
	runInterval    time.Duration
	workspaceRoots []string
	// submissionInterval paces the background advance of unfinished review
	// submissions (gate waits, Den retries, checkout waits).
	submissionInterval time.Duration
	// sourceGrace bounds how long a submission waits for the review checkout
	// to contain its commit before reporting source_missing.
	sourceGrace time.Duration
	// gateTimeout is the GitHub check gate timeout requested from Den.
	gateTimeout time.Duration
	// looksGoodHooks maps a repository ("owner/name") to an executable run
	// after Den records a looks_good review of one of its managed submissions.
	looksGoodHooks map[string]string
	hookTimeout    time.Duration
	// fabricURL is the crew-messaging fabric that receives submission notices
	// such as a failed check gate; empty disables delivery (notices are still
	// recorded and wait).
	fabricURL string
	// fabricInstance is crew-review's stable fabric instance ID, so a restart
	// takes over its own producer lease.
	fabricInstance string
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

	den, err := reviewden.New(cfg.denURL, cfg.denToken, nil, reviewden.WithWorkspaceRoots(cfg.workspaceRoots), reviewden.WithGateTimeout(cfg.gateTimeout))
	if err != nil {
		return err
	}
	runtime, err := newRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	options := []review.Option{review.WithBackend(cfg.backend), review.WithSourceChecker(reviewden.GitSourceChecker{}, cfg.sourceGrace)}
	if cfg.backend == "codex" {
		options = append(options, review.WithRuntimeChecker(reviewcodex.SandboxCheck{Command: cfg.codexCommand}))
	}
	var hooks *reviewhook.Runner
	if len(cfg.looksGoodHooks) > 0 {
		dbPath, err := filepath.Abs(cfg.db)
		if err != nil {
			_ = runtime.Close()
			return err
		}
		hooks, err = reviewhook.New(reviewhook.Config{
			Commands: cfg.looksGoodHooks,
			Timeout:  cfg.hookTimeout,
			LogDir:   filepath.Join(filepath.Dir(dbPath), "hooks"),
			Notes:    den,
		})
		if err != nil {
			_ = runtime.Close()
			return err
		}
		options = append(options, review.WithFinalizedHook(hooks))
	}
	if cfg.fabricURL != "" {
		fabric, err := fabricclient.New(cfg.fabricURL)
		if err != nil {
			_ = runtime.Close()
			return err
		}
		notifier, err := review.NewFabricNotifier(fabric, review.SystemClock{}, review.FabricNotifierConfig{InstanceID: cfg.fabricInstance})
		if err != nil {
			_ = runtime.Close()
			return err
		}
		options = append(options, review.WithNotifier(notifier))
	}
	svc, err := review.New(store, den, runtime, cfg.profile, options...)
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
	submissionsDone := startRunLoop(ctx, cfg.submissionInterval, svc.AdvanceSubmissions, func(err error) {
		if ctx.Err() == nil {
			logger.Printf("review submission loop: %v", err)
		}
	})
	// Notices run on their own loop at the submission interval, so a slow or
	// unavailable fabric never delays review admission.
	noticesDone := startRunLoop(ctx, cfg.submissionInterval, svc.SendNotices, func(err error) {
		if ctx.Err() == nil {
			logger.Printf("review notice loop: %v", err)
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
	<-submissionsDone
	<-noticesDone
	closeErr := svc.Close()
	if hooks != nil {
		hooks.Wait()
	}
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
		listen:       envOr(getenv, "CREW_REVIEW_LISTEN", "127.0.0.1:8413"),
		db:           envOr(getenv, "CREW_REVIEW_DB", "crew-review.db"),
		capacity:     2,
		denURL:       envOr(getenv, "DEN_MCP_URL", reviewden.DefaultMCPURL),
		denToken:     getenv("DEN_MCP_TOKEN"),
		profile:      envOr(getenv, "CREW_REVIEW_PROFILE", defaultReviewProfile),
		backend:      strings.ToLower(envOr(getenv, "CREW_REVIEW_BACKEND", "codex")),
		dshURL:       strings.TrimSpace(getenv("CREW_REVIEW_DSH_URL")),
		codexModel:   strings.TrimSpace(getenv("CREW_REVIEW_MODEL")),
		codexEffort:  strings.TrimSpace(getenv("CREW_REVIEW_REASONING_EFFORT")),
		codexCommand: envOr(getenv, "CODEX_COMMAND", "codex"),
		runInterval:  500 * time.Millisecond,

		submissionInterval: 30 * time.Second,
		sourceGrace:        15 * time.Minute,
		gateTimeout:        reviewden.DefaultGateTimeout,
		hookTimeout:        45 * time.Minute,
		fabricURL:          envOr(getenv, "CREW_REVIEW_FABRIC_URL", "http://127.0.0.1:8787"),
		fabricInstance:     envOr(getenv, "CREW_REVIEW_FABRIC_INSTANCE", "crew-review-local"),
	}
	if raw := strings.TrimSpace(getenv("CREW_REVIEW_RUN_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return commandConfig{}, fmt.Errorf("parse CREW_REVIEW_RUN_INTERVAL: %w", err)
		}
		cfg.runInterval = interval
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"CREW_REVIEW_SUBMISSION_INTERVAL", &cfg.submissionInterval},
		{"CREW_REVIEW_SOURCE_GRACE", &cfg.sourceGrace},
		{"CREW_REVIEW_GATE_TIMEOUT", &cfg.gateTimeout},
		{"CREW_REVIEW_HOOK_TIMEOUT", &cfg.hookTimeout},
	} {
		if raw := strings.TrimSpace(getenv(setting.name)); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return commandConfig{}, fmt.Errorf("parse %s: %w", setting.name, err)
			}
			*setting.target = value
		}
	}
	if raw := strings.TrimSpace(getenv("CREW_REVIEW_CAPACITY")); raw != "" {
		capacity, err := strconv.Atoi(raw)
		if err != nil {
			return commandConfig{}, fmt.Errorf("parse CREW_REVIEW_CAPACITY: %w", err)
		}
		cfg.capacity = capacity
	}
	var codexArgs repeatString
	var workspaceRoots repeatString
	var looksGoodHooks repeatString
	flags := flag.NewFlagSet("crew-review", flag.ContinueOnError)
	flags.StringVar(&cfg.listen, "listen", cfg.listen, "loopback listen address")
	flags.StringVar(&cfg.db, "db", cfg.db, "separate review SQLite database")
	flags.IntVar(&cfg.capacity, "capacity", cfg.capacity, "maximum running jobs")
	flags.StringVar(&cfg.denURL, "den-mcp-url", cfg.denURL, "Den MCP endpoint")
	flags.StringVar(&cfg.denToken, "den-mcp-token", cfg.denToken, "Den MCP bearer token")
	flags.StringVar(&cfg.profile, "review-profile", cfg.profile, "reviewer profile file")
	flags.StringVar(&cfg.backend, "backend", cfg.backend, "reviewer backend: codex or dsh")
	flags.StringVar(&cfg.dshURL, "dsh-url", cfg.dshURL, "DSH reviewer runtime control URL")
	flags.StringVar(&cfg.codexModel, "codex-model", cfg.codexModel, "Codex reviewer model; empty inherits Codex config")
	flags.StringVar(&cfg.codexEffort, "codex-effort", cfg.codexEffort, "Codex reviewer reasoning effort; empty inherits Codex config")
	flags.StringVar(&cfg.codexCommand, "codex-command", cfg.codexCommand, "Codex App Server executable")
	flags.Var(&workspaceRoots, "workspace-root", "absolute directory containing local Git checkouts; repeatable; overrides CREW_REVIEW_WORKSPACE_ROOTS")
	flags.Var(&codexArgs, "codex-arg", "Codex App Server argument; repeatable")
	flags.DurationVar(&cfg.runInterval, "run-interval", cfg.runInterval, "delay between bounded single-job runner passes")
	flags.DurationVar(&cfg.submissionInterval, "submission-interval", cfg.submissionInterval, "delay between background passes over unfinished review submissions")
	flags.DurationVar(&cfg.sourceGrace, "source-grace", cfg.sourceGrace, "how long a submission waits for the review checkout to contain its commit")
	flags.DurationVar(&cfg.gateTimeout, "gate-timeout", cfg.gateTimeout, "GitHub check gate timeout requested from Den (at most 12h)")
	flags.Var(&looksGoodHooks, "looks-good-hook", "owner/name=/absolute/executable run after a looks_good review of that repository; repeatable; overrides CREW_REVIEW_LOOKS_GOOD_HOOKS")
	flags.DurationVar(&cfg.hookTimeout, "hook-timeout", cfg.hookTimeout, "time limit for one looks_good hook run")
	flags.StringVar(&cfg.fabricURL, "fabric-url", cfg.fabricURL, "crew-messaging URL for submission notices; \"off\" disables delivery")
	flags.StringVar(&cfg.fabricInstance, "fabric-instance", cfg.fabricInstance, "stable crew-messaging instance ID")
	if err := flags.Parse(args); err != nil {
		return commandConfig{}, err
	}
	cfg.fabricURL = strings.TrimSpace(cfg.fabricURL)
	if strings.EqualFold(cfg.fabricURL, "off") {
		cfg.fabricURL = ""
	}
	cfg.workspaceRoots = filepath.SplitList(strings.TrimSpace(getenv("CREW_REVIEW_WORKSPACE_ROOTS")))
	if len(workspaceRoots) > 0 {
		cfg.workspaceRoots = append([]string(nil), workspaceRoots...)
	}
	for _, root := range cfg.workspaceRoots {
		if !filepath.IsAbs(root) {
			return commandConfig{}, fmt.Errorf("workspace root must be absolute: %q", root)
		}
	}
	if len(codexArgs) > 0 {
		cfg.codexArgs = append([]string(nil), codexArgs...)
	}
	hookSpecs := strings.FieldsFunc(getenv("CREW_REVIEW_LOOKS_GOOD_HOOKS"), func(r rune) bool { return r == ';' || r == '\n' })
	if len(looksGoodHooks) > 0 {
		hookSpecs = []string(looksGoodHooks)
	}
	hooks, err := parseLooksGoodHooks(hookSpecs)
	if err != nil {
		return commandConfig{}, err
	}
	cfg.looksGoodHooks = hooks
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
	cfg.backend = strings.ToLower(strings.TrimSpace(cfg.backend))
	switch cfg.backend {
	case "codex":
		if strings.TrimSpace(cfg.codexCommand) == "" {
			return commandConfig{}, errors.New("Codex command is required for codex backend")
		}
	case "dsh":
		if strings.TrimSpace(cfg.dshURL) == "" {
			return commandConfig{}, errors.New("CREW_REVIEW_DSH_URL or -dsh-url is required for dsh backend")
		}
	default:
		return commandConfig{}, fmt.Errorf("unknown CREW_REVIEW_BACKEND %q; expected codex or dsh", cfg.backend)
	}
	if cfg.runInterval <= 0 {
		return commandConfig{}, errors.New("run interval must be positive")
	}
	if cfg.submissionInterval <= 0 {
		return commandConfig{}, errors.New("submission interval must be positive")
	}
	if cfg.sourceGrace <= 0 {
		return commandConfig{}, errors.New("source grace must be positive")
	}
	if cfg.gateTimeout <= 0 || cfg.gateTimeout > reviewden.MaxGateTimeout || cfg.gateTimeout%time.Second != 0 {
		return commandConfig{}, fmt.Errorf("gate timeout must be whole seconds between 1s and %s", reviewden.MaxGateTimeout)
	}
	if cfg.hookTimeout <= 0 {
		return commandConfig{}, errors.New("hook timeout must be positive")
	}
	return cfg, nil
}

// parseLooksGoodHooks reads "owner/name=/absolute/executable" entries.
func parseLooksGoodHooks(specs []string) (map[string]string, error) {
	hooks := map[string]string{}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		repository, path, ok := strings.Cut(spec, "=")
		repository, path = strings.TrimSpace(repository), strings.TrimSpace(path)
		owner, name, hasSlash := strings.Cut(repository, "/")
		if !ok || !hasSlash || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("looks_good hook must be owner/name=/absolute/executable: %q", spec)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("looks_good hook for %s must be an absolute path: %q", repository, path)
		}
		if _, exists := hooks[strings.ToLower(repository)]; exists {
			return nil, fmt.Errorf("duplicate looks_good hook for %s", repository)
		}
		hooks[strings.ToLower(repository)] = path
	}
	return hooks, nil
}

func newRuntime(ctx context.Context, cfg commandConfig) (review.ReviewerRuntime, error) {
	switch cfg.backend {
	case "codex":
		return reviewcodex.New(ctx, reviewcodex.Config{
			Command:     cfg.codexCommand,
			Args:        cfg.codexArgs,
			Capacity:    cfg.capacity,
			ProfilePath: cfg.profile,
			Model:       cfg.codexModel,
			Effort:      cfg.codexEffort,
		})
	case "dsh":
		return reviewdsh.New(reviewdsh.Config{URL: cfg.dshURL, Capacity: cfg.capacity})
	default:
		return nil, fmt.Errorf("unknown reviewer backend %q", cfg.backend)
	}
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
