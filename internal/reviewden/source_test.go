package reviewden

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	checker := GitSourceChecker{}
	ctx := context.Background()
	for name, commit := range map[string]string{"HEAD": first, "worktree": fromWorktree, "remote": fromRemote} {
		if has, err := checker.HasCommit(ctx, checkout, commit); err != nil || !has {
			t.Fatalf("%s commit: has=%v err=%v", name, has, err)
		}
	}
	if has, err := checker.HasCommit(ctx, checkout, strings.Repeat("ab", 20)); err != nil || has {
		t.Fatalf("unknown commit: has=%v err=%v", has, err)
	}
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
