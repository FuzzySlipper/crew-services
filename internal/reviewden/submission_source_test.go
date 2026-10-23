package reviewden

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crew-services/internal/review"
)

func TestResolveGitSubmissionSourceUsesPushedTaskRangeAndPreservesCheckout(t *testing.T) {
	root := t.TempDir()
	origin, checkout := filepath.Join(root, "origin"), filepath.Join(root, "checkout")
	git := func(dir string, args ...string) string {
		t.Helper()
		output, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git(root, "init", "-q", "-b", "main", origin)
	git(origin, "commit", "-q", "--allow-empty", "-m", "Initial")
	base := git(origin, "rev-parse", "HEAD")
	git(root, "clone", "-q", origin, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "local-work"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(origin, "commit", "-q", "--allow-empty", "-m", "First part (#10318)")
	first := git(origin, "rev-parse", "HEAD")
	git(origin, "commit", "-q", "--allow-empty", "-m", "Another task (#10319)")
	git(origin, "commit", "-q", "--allow-empty", "-m", "Finish (#10318)")
	head := git(origin, "rev-parse", "HEAD")
	git(origin, "commit", "-q", "--allow-empty", "-m", "Later unrelated task (#10320)")

	request := review.SubmissionRequest{ProjectID: "project", TaskID: 10318, Repository: "owner/repo", Ref: "main"}
	ctx := context.Background()
	source, err := resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if err != nil || source.CommitSHA != head || source.BaseCommit != base {
		t.Fatalf("source=%+v err=%v, want %s..%s", source, err, base, head)
	}
	shallow := filepath.Join(root, "shallow")
	git(root, "clone", "-q", "--depth=1", "file://"+origin, shallow)
	fromShallow, err := resolveGitSubmissionSource(ctx, shallow, "origin", request)
	if err != nil || fromShallow != source {
		t.Fatalf("shallow source=%+v err=%v", fromShallow, err)
	}
	// A retry remains pinned to this task even after another task advances ref.
	retry, err := resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if err != nil || retry != source {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	request.CommitSHA = first
	explicit, err := resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if err != nil || explicit.CommitSHA != first || explicit.BaseCommit != base {
		t.Fatalf("override=%+v err=%v", explicit, err)
	}
	if got := git(checkout, "rev-parse", "HEAD"); got != base {
		t.Fatalf("moved HEAD to %s", got)
	}
	if got := git(checkout, "rev-parse", "main"); got != base {
		t.Fatalf("moved main to %s", got)
	}
	if body, err := os.ReadFile(filepath.Join(checkout, "local-work")); err != nil || string(body) != "keep me" {
		t.Fatalf("local work=%s err=%v", body, err)
	}
	request.CommitSHA = ""
	request.BaseCommit = first
	baseOverride, err := resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if err != nil || baseOverride.CommitSHA != head || baseOverride.BaseCommit != first {
		t.Fatalf("base override=%+v err=%v", baseOverride, err)
	}
	request.BaseCommit = ""
	request.CommitSHA = strings.Repeat("a", 40)
	_, err = resolveGitSubmissionSource(ctx, checkout, "origin", request)
	var rejected *review.SubmissionError
	if !errors.As(err, &rejected) || rejected.Code != "unknown_commit" {
		t.Fatalf("unknown commit err=%v", err)
	}
	request.CommitSHA = ""
	request.TaskID = 99
	_, err = resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if !errors.As(err, &rejected) || rejected.Code != "task_commits_not_found" {
		t.Fatalf("missing marker err=%v", err)
	}
	request.Ref = "absent"
	_, err = resolveGitSubmissionSource(ctx, checkout, "origin", request)
	if !errors.As(err, &rejected) || rejected.Code != "unknown_ref" {
		t.Fatalf("missing ref err=%v", err)
	}
}

func TestClassifySourceRefusalThroughMCPWrapper(t *testing.T) {
	for _, code := range []string{"unknown_commit", "base_not_ancestor", "round_not_supersedable"} {
		err := classifyToolError("request_review", toolEnvelope{Content: []toolContent{{Type: "text", Text: `{"error":"den_backend_request_failed","message":"{\"error\":{\"code\":\"` + code + `\",\"message\":\"invalid range\"}}"}`}}})
		var rejected *review.SubmissionError
		if !errors.As(err, &rejected) || rejected.Code != code {
			t.Fatalf("code=%s err=%v", code, err)
		}
	}
}
