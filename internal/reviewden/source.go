package reviewden

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	// fetchTimeout bounds one fetch made when a commit is not in the clone.
	fetchTimeout = time.Minute
	// gitWaitDelay bounds how long a git process may outlive its context,
	// for example while a credential helper or ssh child still holds its
	// output pipe.
	gitWaitDelay = 5 * time.Second
	// A fetch whose transfer stays below fetchLowSpeedLimit bytes per second
	// for fetchLowSpeedTime seconds is aborted by git itself.
	fetchLowSpeedLimit = "1000"
	fetchLowSpeedTime  = "30"
)

// GitSourceChecker reports whether a review checkout's repository holds a
// commit, so a reviewer can read it with git show and git diff. The working
// tree may be at another commit: agents often push from per-task worktrees
// and nothing pulls the main checkout.
//
// When the commit is absent it starts one background `git fetch` for the
// checkout, which updates remote-tracking refs and objects but never the
// working tree, HEAD, or local branches, and reports the commit as not there
// yet. The caller checks again later: the submission waits in source_pending
// and the next background pass finds the fetched commit. A fetch therefore
// never runs inside a caller's submit or the submission loop, and a hung
// fetch delays only the checkout it belongs to. At most one fetch runs per
// checkout; a failed fetch is reported by the next check, which starts
// another. The zero value is ready to use.
type GitSourceChecker struct {
	// git, fetchTimeout and waitDelay override the defaults in tests.
	git          string
	fetchTimeout time.Duration
	waitDelay    time.Duration

	mu      sync.Mutex
	fetches map[string]*sourceFetch
}

// sourceFetch is one checkout's background fetch: running until done is
// closed, then holding its result until a check reads it.
type sourceFetch struct {
	done chan struct{}
	err  error
}

func (c *GitSourceChecker) HasCommit(ctx context.Context, workspace string, commitSHA string) (bool, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !fullCommitPattern.MatchString(commitSHA) {
		return false, fmt.Errorf("commit %q is not a full SHA", commitSHA)
	}
	present, err := c.commitPresent(ctx, workspace, commitSHA)
	if err != nil || present {
		return present, err
	}
	return false, c.fetchInBackground(workspace)
}

// fetchInBackground starts a fetch for the checkout unless one is running.
// It returns the error of a fetch that finished since the last check.
func (c *GitSourceChecker) fetchInBackground(workspace string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var finished error
	if fetch, ok := c.fetches[workspace]; ok {
		select {
		case <-fetch.done:
			finished = fetch.err
		default:
			return nil
		}
	}
	if c.fetches == nil {
		c.fetches = map[string]*sourceFetch{}
	}
	fetch := &sourceFetch{done: make(chan struct{})}
	c.fetches[workspace] = fetch
	go func() {
		// The fetch belongs to the checker, not to the caller that started
		// it, so a short caller deadline does not cut it off.
		fetchCtx, cancel := context.WithTimeout(context.Background(), c.timeout())
		defer cancel()
		cmd := c.command(fetchCtx, workspace, append(append([]string(nil), lowSpeedArgs...), "fetch", "--quiet")...)
		if output, err := cmd.CombinedOutput(); err != nil {
			fetch.err = fmt.Errorf("git fetch in %s: %w: %s", workspace, err, strings.TrimSpace(string(output)))
		}
		close(fetch.done)
	}()
	return finished
}

func (c *GitSourceChecker) commitPresent(ctx context.Context, workspace, commitSHA string) (bool, error) {
	output, err := c.command(ctx, workspace, "cat-file", "-e", commitSHA+"^{commit}").CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && strings.Contains(strings.ToLower(string(output)), "not a valid") {
		return false, nil
	}
	return false, fmt.Errorf("git cat-file in %s: %w: %s", workspace, err, strings.TrimSpace(string(output)))
}

// command builds a non-interactive git command: git never prompts for
// credentials, and the process cannot outlive its context by more than the
// wait delay.
func (c *GitSourceChecker) command(ctx context.Context, workspace string, args ...string) *exec.Cmd {
	cmd := gitCommand(ctx, c.git, workspace, args...)
	if c.waitDelay > 0 {
		cmd.WaitDelay = c.waitDelay
	}
	return cmd
}

// gitCommand is the non-interactive git invocation used for every review
// checkout command. An empty git means the git on PATH.
func gitCommand(ctx context.Context, git, workspace string, args ...string) *exec.Cmd {
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", workspace}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// lowSpeedArgs make git abort a network transfer that stalls.
var lowSpeedArgs = []string{"-c", "http.lowSpeedLimit=" + fetchLowSpeedLimit, "-c", "http.lowSpeedTime=" + fetchLowSpeedTime}

func (c *GitSourceChecker) timeout() time.Duration {
	if c.fetchTimeout > 0 {
		return c.fetchTimeout
	}
	return fetchTimeout
}
