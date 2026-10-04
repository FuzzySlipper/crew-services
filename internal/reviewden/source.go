package reviewden

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fetchTimeout bounds the one fetch made when a commit is not in the clone.
const fetchTimeout = time.Minute

// GitSourceChecker reports whether a review checkout's repository holds a
// commit, so a reviewer can read it with git show and git diff. The working
// tree may be at another commit: agents often push from per-task worktrees
// and nothing pulls the main checkout. When the commit is absent it runs one
// `git fetch`, which updates remote-tracking refs and objects but never the
// working tree, HEAD, or local branches.
type GitSourceChecker struct{}

func (GitSourceChecker) HasCommit(ctx context.Context, workspace string, commitSHA string) (bool, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !fullCommitPattern.MatchString(commitSHA) {
		return false, fmt.Errorf("commit %q is not a full SHA", commitSHA)
	}
	present, err := commitPresent(ctx, workspace, commitSHA)
	if err != nil || present {
		return present, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if output, err := exec.CommandContext(fetchCtx, "git", "-C", workspace, "fetch", "--quiet").CombinedOutput(); err != nil {
		return false, fmt.Errorf("git fetch in %s: %w: %s", workspace, err, strings.TrimSpace(string(output)))
	}
	return commitPresent(ctx, workspace, commitSHA)
}

func commitPresent(ctx context.Context, workspace, commitSHA string) (bool, error) {
	output, err := exec.CommandContext(ctx, "git", "-C", workspace, "cat-file", "-e", commitSHA+"^{commit}").CombinedOutput()
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
