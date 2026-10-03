package reviewden

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestGitSourceCheckerReportsContainedAndMissingCommits(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "-q")
	runGit("commit", "-q", "--allow-empty", "-m", "first")
	first := runGit("rev-parse", "HEAD")
	runGit("commit", "-q", "--allow-empty", "-m", "second")
	second := runGit("rev-parse", "HEAD")
	runGit("checkout", "-q", first)

	checker := GitSourceChecker{}
	ctx := context.Background()
	if contains, err := checker.ContainsCommit(ctx, repo, first); err != nil || !contains {
		t.Fatalf("HEAD commit: contains=%v err=%v", contains, err)
	}
	if contains, err := checker.ContainsCommit(ctx, repo, second); err != nil || contains {
		t.Fatalf("later commit: contains=%v err=%v", contains, err)
	}
	if contains, err := checker.ContainsCommit(ctx, repo, strings.Repeat("ab", 20)); err != nil || contains {
		t.Fatalf("unknown commit: contains=%v err=%v", contains, err)
	}
	if _, err := checker.ContainsCommit(ctx, repo, "main"); err == nil {
		t.Fatal("non-SHA input was accepted")
	}
	if head := runGit("rev-parse", "HEAD"); head != first {
		t.Fatalf("checker changed HEAD to %s", head)
	}
}
