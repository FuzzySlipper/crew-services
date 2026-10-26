// Package reviewhook runs a repository's command after Den records a
// looks_good review of a managed submission, for example to deploy the
// reviewed commit. Hooks never change the job or its Den round.
package reviewhook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crew-services/internal/review"
)

const (
	looksGood     = "looks_good"
	noteTailLines = 40
	noteTailBytes = 3000
	noteTimeout   = 30 * time.Second
)

// NotePoster posts a short status note on a task thread.
type NotePoster interface {
	PostTaskNote(ctx context.Context, key review.TaskKey, content string) error
}

// Config names one executable per repository ("owner/name").
type Config struct {
	Commands map[string]string
	Timeout  time.Duration
	LogDir   string
	Notes    NotePoster
	Logger   *log.Logger
	// Exec runs a command and returns its combined output; nil runs the
	// executable directly with the given environment.
	Exec func(ctx context.Context, path string, env []string) ([]byte, error)
}

// Runner runs hooks one at a time in the background.
type Runner struct {
	commands map[string]string
	timeout  time.Duration
	logDir   string
	notes    NotePoster
	logger   *log.Logger
	exec     func(context.Context, string, []string) ([]byte, error)
	mu       sync.Mutex
	running  sync.WaitGroup
}

// New validates that every command is an absolute path to an executable file.
func New(cfg Config) (*Runner, error) {
	if cfg.Timeout <= 0 {
		return nil, errors.New("hook timeout must be positive")
	}
	if cfg.LogDir == "" || !filepath.IsAbs(cfg.LogDir) {
		return nil, fmt.Errorf("hook log directory must be absolute: %q", cfg.LogDir)
	}
	commands := make(map[string]string, len(cfg.Commands))
	for repository, path := range cfg.Commands {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("hook for %s must be an absolute path: %q", repository, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("hook for %s: %w", repository, err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("hook for %s is not an executable file: %s", repository, path)
		}
		commands[strings.ToLower(repository)] = path
	}
	r := &Runner{commands: commands, timeout: cfg.Timeout, logDir: cfg.LogDir, notes: cfg.Notes, logger: cfg.Logger, exec: cfg.Exec}
	if r.logger == nil {
		r.logger = log.New(os.Stderr, "crew-review hook: ", log.LstdFlags)
	}
	if r.exec == nil {
		r.exec = runExecutable
	}
	return r, nil
}

// Finalized starts the repository's hook when the job is a looks_good review
// of a managed submission. It returns at once.
func (r *Runner) Finalized(job review.Job) {
	source := job.Admission.Source
	if job.Receipt == nil || job.Receipt.Verdict != looksGood || source == nil || source.CommitSHA == "" {
		return
	}
	path, ok := r.commands[strings.ToLower(source.Repository)]
	if !ok {
		return
	}
	r.running.Add(1)
	go func() {
		defer r.running.Done()
		r.run(job.Admission.Key, *source, path)
	}()
}

// Wait blocks until every started hook has finished.
func (r *Runner) Wait() { r.running.Wait() }

func (r *Runner) run(key review.Key, source review.ReviewSource, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	env := append(os.Environ(),
		"CREW_REVIEW_REPOSITORY="+source.Repository,
		"CREW_REVIEW_COMMIT="+source.CommitSHA,
		"CREW_REVIEW_REF="+source.Ref,
		"CREW_REVIEW_PROJECT="+key.ProjectID,
		fmt.Sprintf("CREW_REVIEW_TASK=%d", key.TaskID),
		fmt.Sprintf("CREW_REVIEW_ROUND=%d", key.ReviewRoundID),
	)
	// Hooks outlive a crew-review shutdown request: stopping a deploy midway
	// is worse than letting it finish within its timeout.
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	started := time.Now()
	output, err := r.exec(ctx, path, env)
	cancel()
	elapsed := time.Since(started).Round(time.Second)

	logPath := filepath.Join(r.logDir, fmt.Sprintf("%s-%d-round-%d.log", key.ProjectID, key.TaskID, key.ReviewRoundID))
	if writeErr := writeLog(logPath, output); writeErr != nil {
		r.logger.Printf("write hook log: %v", writeErr)
		logPath = ""
	}
	commit := shortCommit(source.CommitSHA)
	outcome := fmt.Sprintf("succeeded in %s", elapsed)
	if err != nil {
		outcome = fmt.Sprintf("failed after %s: %v", elapsed, err)
	}
	r.logger.Printf("%s for %s task %d at %s %s", filepath.Base(path), key.ProjectID, key.TaskID, commit, outcome)
	if r.notes == nil {
		return
	}
	note := fmt.Sprintf("Post-review hook `%s` for %s at `%s` %s.", filepath.Base(path), source.Repository, commit, outcome)
	if tail := outputTail(output); tail != "" {
		note += "\n\n```\n" + tail + "\n```"
	}
	if logPath != "" {
		note += "\n\nFull output: `" + logPath + "` on the crew-review host."
	}
	noteCtx, cancelNote := context.WithTimeout(context.Background(), noteTimeout)
	defer cancelNote()
	if postErr := r.notes.PostTaskNote(noteCtx, key.Task(), note); postErr != nil {
		r.logger.Printf("post hook note for %s task %d: %v", key.ProjectID, key.TaskID, postErr)
	}
}

func runExecutable(ctx context.Context, path string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

func writeLog(path string, output []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, output, 0o644)
}

func outputTail(output []byte) string {
	lines := strings.Split(strings.TrimRight(string(output), "\n"), "\n")
	if len(lines) > noteTailLines {
		lines = lines[len(lines)-noteTailLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > noteTailBytes {
		tail = tail[len(tail)-noteTailBytes:]
	}
	return strings.ReplaceAll(strings.TrimSpace(tail), "```", "'''")
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
