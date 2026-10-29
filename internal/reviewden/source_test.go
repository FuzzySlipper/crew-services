package reviewden

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitSourceCheckerFindsWorktreeAndRemoteCommits(t *testing.T) {
	root := t.TempDir()
	origin, checkout := filepath.Join(root, "origin"), filepath.Join(root, "checkout")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git(root, "init", "-q", "-b", "main", origin)
	git(origin, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(origin, "rev-parse", "HEAD")
	git(root, "clone", "-q", origin, checkout)

	// A commit made in a sibling worktree shares the clone's object store but
	// is not reachable from the main checkout's HEAD.
	worktree := filepath.Join(root, "task-worktree")
	git(checkout, "worktree", "add", "-q", "-b", "task", worktree)
	git(worktree, "commit", "-q", "--allow-empty", "-m", "task change")
	fromWorktree := git(worktree, "rev-parse", "HEAD")

	// A commit pushed to origin from elsewhere is not in the clone until a fetch.
	git(origin, "commit", "-q", "--allow-empty", "-m", "pushed elsewhere")
	fromRemote := git(origin, "rev-parse", "HEAD")

	checker := &GitSourceChecker{}
	ctx := context.Background()
	for name, commit := range map[string]string{"HEAD": first, "worktree": fromWorktree} {
		if has, err := checker.HasCommit(ctx, checkout, commit); err != nil || !has {
			t.Fatalf("%s commit: has=%v err=%v", name, has, err)
		}
	}
	// The remote commit is not there yet; the check starts a background
	// fetch, and a later check finds it.
	if has, err := checker.HasCommit(ctx, checkout, fromRemote); err != nil || has {
		t.Fatalf("remote commit before fetch: has=%v err=%v", has, err)
	}
	waitFor(t, func() bool {
		has, err := checker.HasCommit(ctx, checkout, fromRemote)
		if err != nil {
			t.Fatal(err)
		}
		return has
	})
	if has, err := checker.HasCommit(ctx, checkout, strings.Repeat("ab", 20)); err != nil || has {
		t.Fatalf("unknown commit: has=%v err=%v", has, err)
	}
	// That check started a fetch; let it finish before the checkout is removed.
	checker.waitForFetches()
	if _, err := checker.HasCommit(ctx, checkout, "main"); err == nil {
		t.Fatal("non-SHA input was accepted")
	}
	if head := git(checkout, "rev-parse", "HEAD"); head != first {
		t.Fatalf("checker moved HEAD to %s", head)
	}
	if branch := git(checkout, "rev-parse", "main"); branch != first {
		t.Fatalf("checker moved main to %s", branch)
	}
}

// TestHungGitFetchBlocksNoCheck runs a fake git whose fetch hangs in a child
// process that keeps the output pipe open, as a stuck credential helper or
// network transfer would.
func TestHungGitFetchBlocksNoCheck(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\n" +
		"echo \"$GIT_TERMINAL_PROMPT $*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *fetch*) sleep 5; exit 0 ;;\n" +
		"  *cat-file*) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	checker := &GitSourceChecker{git: script, fetchTimeout: 300 * time.Millisecond, waitDelay: 200 * time.Millisecond}
	commit := strings.Repeat("ab", 20)
	ctx := context.Background()

	started := time.Now()
	for _, workspace := range []string{"/repo/hung", "/repo/hung", "/repo/other"} {
		if has, err := checker.HasCommit(ctx, workspace, commit); err != nil || has {
			t.Fatalf("%s: has=%v err=%v", workspace, has, err)
		}
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("checks waited %s on a hung fetch", elapsed)
	}

	// The hung fetch is killed at its timeout even though its child holds
	// the pipe, and the next check reports it and starts a fresh fetch.
	var fetchErr error
	waitFor(t, func() bool {
		_, fetchErr = checker.HasCommit(ctx, "/repo/hung", commit)
		return fetchErr != nil
	})
	if !strings.Contains(fetchErr.Error(), "git fetch in /repo/hung") {
		t.Fatalf("fetch error=%v", fetchErr)
	}
	checker.waitForFetches()

	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var fetches []string
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(line, "0 ") {
			t.Fatalf("git ran without GIT_TERMINAL_PROMPT=0: %q", line)
		}
		if strings.Contains(line, " fetch ") {
			fetches = append(fetches, line)
			if !strings.Contains(line, "http.lowSpeedLimit=1000") || !strings.Contains(line, "http.lowSpeedTime=30") {
				t.Fatalf("fetch without low-speed limits: %q", line)
			}
		}
	}
	// One fetch per checkout while it ran, then the retry for /repo/hung.
	hung := 0
	for _, line := range fetches {
		if strings.Contains(line, "/repo/hung") {
			hung++
		}
	}
	if hung < 1 || hung > 2 || len(fetches)-hung != 1 {
		t.Fatalf("fetches=%q", fetches)
	}
}

func (c *GitSourceChecker) waitForFetches() {
	c.mu.Lock()
	fetches := make([]*sourceFetch, 0, len(c.fetches))
	for _, fetch := range c.fetches {
		fetches = append(fetches, fetch)
	}
	c.mu.Unlock()
	for _, fetch := range fetches {
		<-fetch.done
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
