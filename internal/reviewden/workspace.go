package reviewden

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"crew-services/internal/review"
)

// WithWorkspaceRoots configures shallow discovery on the review host. Each root
// contains checkouts directly; no recursive traversal of build trees is needed.
func WithWorkspaceRoots(roots []string) func(*Client) error {
	return func(c *Client) error {
		for _, root := range roots {
			root = strings.TrimSpace(root)
			if !filepath.IsAbs(root) {
				return fmt.Errorf("workspace root must be absolute: %q", root)
			}
			c.workspaceRoots = append(c.workspaceRoots, filepath.Clean(root))
		}
		return nil
	}
}

// ValidateWorkspace is a read-only admission preflight, before creating a round.
func (c *Client) ValidateWorkspace(ctx context.Context, key review.TaskKey) error {
	data, err := c.call(ctx, "get_project", struct {
		ProjectID string `json:"project_id"`
	}{key.ProjectID})
	if err != nil {
		return err
	}
	var response struct {
		Project struct {
			ID            string `json:"id"`
			RootPath      string `json:"root_path"`
			RepositoryURL string `json:"repository_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return fmt.Errorf("decode Den project: %w", err)
	}
	if response.Project.ID != key.ProjectID {
		return fmt.Errorf("Den project identity does not match %q", key.ProjectID)
	}
	_, err = c.resolveWorkspace(ctx, key.ProjectID, response.Project.RootPath, response.Project.RepositoryURL)
	return err
}

func (c *Client) resolveWorkspace(ctx context.Context, projectID, configured, repositoryURL string) (string, error) {
	configured = strings.TrimSpace(configured)
	repositoryURL = strings.TrimSpace(repositoryURL)
	identity := repositoryIdentity(repositoryURL)
	if repositoryURL != "" && identity == "" {
		return "", fmt.Errorf("%w for project %s: repository_url is not a supported Git remote URL; update Den project repository_url", review.ErrWorkspaceRequired, projectID)
	}
	if filepath.IsAbs(configured) {
		if checkout := matchingCheckout(ctx, configured, identity); checkout != "" {
			return checkout, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if identity == "" {
		return "", fmt.Errorf("%w for project %s: set Den project repository_url for discovery, or set Den project root_path to a valid absolute local Git checkout", review.ErrWorkspaceRequired, projectID)
	}
	matches := make(map[string]struct{})
	for _, root := range c.workspaceRoots {
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%w: cannot read workspace root %s: %v", review.ErrWorkspaceRequired, root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			candidate := filepath.Join(root, entry.Name())
			if _, err := os.Stat(filepath.Join(candidate, ".git")); err != nil {
				continue
			}
			if checkout := matchingCheckout(ctx, candidate, identity); checkout != "" {
				matches[checkout] = struct{}{}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(matches))
	for path := range matches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	switch len(paths) {
	case 1:
		return paths[0], nil
	case 0:
		return "", fmt.Errorf("%w for project %s (%s): searched workspace roots %v; clone the repository under a configured workspace root or set Den project root_path to an existing matching checkout", review.ErrWorkspaceRequired, projectID, identity, c.workspaceRoots)
	default:
		return "", fmt.Errorf("%w for project %s (%s): %s; set Den project root_path to select the checkout", review.ErrWorkspaceAmbiguous, projectID, identity, strings.Join(paths, ", "))
	}
}

// Explicit project paths may be subdirectories of a checkout. Discovery only
// supplies candidates with their own .git marker, including linked worktrees.
// Git calls are read-only.
func matchingCheckout(ctx context.Context, path, identity string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	_, err = exec.CommandContext(ctx, "git", "-C", real, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	if identity == "" {
		return real
	}
	remotes, err := exec.CommandContext(ctx, "git", "-C", real, "config", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(remotes), "\n") {
		fields := strings.SplitN(line, " ", 2)
		if len(fields) == 2 && repositoryIdentity(fields[1]) == identity {
			return real
		}
	}
	return ""
}

// Compare SSH and HTTPS spellings of the same remote. GitHub repository names
// are case-insensitive; paths on other Git hosts retain their case.
func repositoryIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		colon := strings.Index(raw, ":")
		if closing := strings.Index(raw, "]:"); closing >= 0 {
			colon = closing + 1
		}
		if colon <= 0 || colon == len(raw)-1 {
			return ""
		}
		authority := raw[:colon]
		if strings.ContainsAny(authority, "/\\") {
			return ""
		}
		raw = "ssh://" + authority + "/" + raw[colon+1:]
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "https", "http", "ssh", "git":
	default:
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port != "" && !(parsed.Scheme == "ssh" && port == "22") && !(parsed.Scheme == "https" && port == "443") && !(parsed.Scheme == "http" && port == "80") && !(parsed.Scheme == "git" && port == "9418") {
		host += ":" + port
	}
	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if path == "" {
		return ""
	}
	if host == "github.com" {
		path = strings.ToLower(path)
	}
	return host + "/" + path
}
