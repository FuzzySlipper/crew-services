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

func testCheckout(t *testing.T, path, remote string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", path}, {"-C", path, "remote", "add", "origin", remote}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestWorkspaceDiscoveryMatchesRemoteNotDirectoryName(t *testing.T) {
	root := t.TempDir()
	wrong := testCheckout(t, filepath.Join(root, "project"), "https://github.com/another/project.git")
	want := testCheckout(t, filepath.Join(root, "renamed-checkout"), "git@github.com:Owner/Project.git")
	client := &Client{workspaceRoots: []string{root}}
	for _, configured := range []string{"", filepath.Join(root, "old-name"), wrong} {
		got, err := client.resolveWorkspace(context.Background(), "project", configured, "https://github.com/owner/project")
		if err != nil || got != want {
			t.Fatalf("configured=%s got=%s err=%v", configured, got, err)
		}
	}
}

func TestWorkspaceDiscoveryAmbiguityExplicitSelectionAndSymlinks(t *testing.T) {
	root := t.TempDir()
	first := testCheckout(t, filepath.Join(root, "first"), "https://github.com/owner/project.git")
	if err := os.Symlink(first, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	client := &Client{workspaceRoots: []string{root, root}}
	got, err := client.resolveWorkspace(context.Background(), "project", "", "ssh://git@github.com/owner/project.git")
	if err != nil || got != first {
		t.Fatalf("symlink dedup: got=%s err=%v", got, err)
	}
	second := testCheckout(t, filepath.Join(root, "second"), "https://github.com/owner/project.git")
	_, err = client.resolveWorkspace(context.Background(), "project", "", "https://github.com/owner/project.git")
	if !errors.Is(err, review.ErrWorkspaceAmbiguous) || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Fatalf("ambiguity: %v", err)
	}
	got, err = client.resolveWorkspace(context.Background(), "project", second, "https://github.com/owner/project.git")
	if err != nil || got != second {
		t.Fatalf("explicit choice: got=%s err=%v", got, err)
	}
}

func TestWorkspaceDiscoveryRejectsMissingAndNonGitPaths(t *testing.T) {
	root := t.TempDir()
	checkout := testCheckout(t, filepath.Join(root, "repo"), "https://github.com/owner/project.git")
	subdir := filepath.Join(checkout, "src")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	client := &Client{}
	for _, path := range []string{"", root, "relative"} {
		_, err := client.resolveWorkspace(context.Background(), "project", path, "")
		if !errors.Is(err, review.ErrWorkspaceRequired) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	got, err := client.resolveWorkspace(context.Background(), "project", subdir, "https://github.com/owner/project.git")
	if err != nil || got != subdir {
		t.Fatalf("explicit project subdirectory: %s %v", got, err)
	}
	got, err = client.resolveWorkspace(context.Background(), "project", checkout, "")
	if err != nil || got != checkout {
		t.Fatalf("legacy checkout: %s %v", got, err)
	}
	_, err = client.resolveWorkspace(context.Background(), "project", "", "https://github.com/owner/missing.git")
	if !errors.Is(err, review.ErrWorkspaceRequired) || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("missing: %v", err)
	}
}

func TestRepositoryIdentity(t *testing.T) {
	for raw, want := range map[string]string{
		"git.example:Group/Repo.git":                "git.example/Group/Repo",
		"git@[::1]:Group/Repo.git":                  "::1/Group/Repo",
		"git://git.example:9418/Group/Repo.git":     "git.example/Group/Repo",
		"HTTPS://github.com/Owner/Repo.git":         "github.com/owner/repo",
		"https://github.com/Owner/Repo.git":         "github.com/owner/repo",
		"git@github.com:owner/repo.git":             "github.com/owner/repo",
		"ssh://git@github.com:22/owner/repo.git":    "github.com/owner/repo",
		"ssh://git@git.example:2222/Group/Repo.git": "git.example:2222/Group/Repo",
		"https://git.example/Group/Repo.git":        "git.example/Group/Repo",
		"/home/dev/repo":                            "", "owner/repo": "", "": "",
	} {
		if got := repositoryIdentity(raw); got != want {
			t.Errorf("%q = %q want %q", raw, got, want)
		}
	}
}
