package reviewden

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitSourceChecker reports whether a review checkout's HEAD contains a
// commit. It only runs read-only git commands: no fetch, checkout, or reset.
type GitSourceChecker struct{}

func (GitSourceChecker) ContainsCommit(ctx context.Context, workspace string, commitSHA string) (bool, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !fullCommitPattern.MatchString(commitSHA) {
		return false, fmt.Errorf("commit %q is not a full SHA", commitSHA)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "merge-base", "--is-ancestor", commitSHA, "HEAD")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 1:
			return false, nil
		case 128:
			// The commit object is not in this clone yet; the checkout has
			// not been pulled since the push.
			if strings.Contains(string(output), commitSHA) || strings.Contains(strings.ToLower(string(output)), "not a valid commit") {
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("git merge-base --is-ancestor in %s: %w: %s", workspace, err, strings.TrimSpace(string(output)))
}
