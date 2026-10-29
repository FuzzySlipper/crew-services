package reviewden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"crew-services/internal/review"
)

// ResolveSubmissionSource uses the review host's matching checkout, never the
// caller's remembered SHA or the checkout's (possibly stale) HEAD. Fetching
// objects leaves local branches and working files alone. Den validates the
// resolved range against GitHub before it records the round.
func (c *Client) ResolveSubmissionSource(ctx context.Context, request review.SubmissionRequest) (review.ReviewSource, error) {
	data, err := c.call(ctx, "get_project", struct {
		ProjectID string `json:"project_id"`
	}{request.ProjectID})
	if err != nil {
		return review.ReviewSource{}, err
	}
	var response struct {
		Project struct {
			ID       string `json:"id"`
			RootPath string `json:"root_path"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return review.ReviewSource{}, fmt.Errorf("decode Den project for source resolution: %w", err)
	}
	if response.Project.ID != request.ProjectID {
		return review.ReviewSource{}, errors.New("Den project identity does not match source request")
	}
	identity := repositoryIdentity("https://github.com/" + request.Repository)
	workspace, err := c.resolveWorkspace(ctx, request.ProjectID, response.Project.RootPath, "https://github.com/"+request.Repository)
	if err != nil {
		return review.ReviewSource{}, err
	}
	remotes, err := gitCommand(ctx, "", workspace, "config", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return review.ReviewSource{}, fmt.Errorf("read source checkout remotes: %w", err)
	}
	var remote string
	for _, line := range strings.Split(string(remotes), "\n") {
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 || repositoryIdentity(fields[1]) != identity {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(fields[0], "remote."), ".url")
		if remote == "" || name == "origin" {
			remote = name
		}
	}
	if remote == "" {
		return review.ReviewSource{}, errors.New("no matching source repository remote")
	}
	return resolveGitSubmissionSource(ctx, workspace, remote, request)
}

type gitSubmissionRepository struct {
	workspace string
	remote    string
}

type taskSourceCommit struct {
	sha     string
	parents []string
}

func (g gitSubmissionRepository) run(ctx context.Context, args ...string) (string, error) {
	// Network commands (ls-remote, fetch) run here inside a caller's submit, so
	// they must never prompt and must abort a stalled transfer.
	output, err := gitCommand(ctx, "", g.workspace, append(append([]string(nil), lowSpeedArgs...), args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func resolveGitSubmissionSource(ctx context.Context, workspace, remote string, request review.SubmissionRequest) (review.ReviewSource, error) {
	source := review.ReviewSource{Repository: request.Repository, Ref: request.Ref, CommitSHA: request.CommitSHA, BaseCommit: request.BaseCommit}
	if source.CommitSHA != "" && source.BaseCommit != "" {
		return source, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	repository := gitSubmissionRepository{workspace: workspace, remote: remote}
	refSHA, err := repository.pushedRef(fetchCtx, request.Ref)
	if err != nil {
		return source, err
	}
	if err := repository.fetchHistory(fetchCtx, refSHA, request.CommitSHA); err != nil {
		return source, err
	}
	historyHead := source.CommitSHA
	if historyHead == "" {
		historyHead = refSHA
	}
	matches, err := repository.taskCommits(fetchCtx, historyHead, request.TaskID)
	if err != nil {
		return source, err
	}
	if source.CommitSHA == "" {
		source.CommitSHA = matches[0].sha
	}
	// Choosing one sibling in a merge would silently omit other task commits.
	for _, match := range matches {
		if _, err := repository.run(fetchCtx, "merge-base", "--is-ancestor", match.sha, source.CommitSHA); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				return source, err
			}
			return source, &review.SubmissionError{Code: "ambiguous_task_commits", Message: "task commits are on separate merge paths; supply commit_sha for the containing merge"}
		}
	}
	if source.BaseCommit == "" {
		parents := matches[len(matches)-1].parents
		if len(parents) == 0 {
			return source, &review.SubmissionError{Code: "base_derivation_failed", Message: "the earliest task commit has no parent; supply an explicit diff base"}
		}
		source.BaseCommit = parents[0]
	}
	return source, nil
}

// Resolve the advertised ref first and fetch its immutable object. FETCH_HEAD
// is shared between worktrees, so it is neither read nor written.
func (g gitSubmissionRepository) pushedRef(ctx context.Context, ref string) (string, error) {
	patterns := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		patterns = append(patterns, "refs/heads/"+ref, "refs/tags/"+ref)
	}
	output, err := g.run(ctx, append([]string{"ls-remote", "--exit-code", g.remote}, patterns...)...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", &review.SubmissionError{Code: "unknown_ref", Message: "ref does not exist in the pushed repository"}
		}
		return "", err
	}
	for _, pattern := range patterns {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == pattern {
				return fields[0], nil
			}
		}
	}
	return "", &review.SubmissionError{Code: "unknown_ref", Message: "ref did not resolve to an advertised remote commit"}
}

func (g gitSubmissionRepository) fetchHistory(ctx context.Context, refSHA, explicitSHA string) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head"}
	shallow, err := g.run(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if shallow == "true" {
		args = append(args, "--unshallow")
	}
	args = append(args, g.remote, refSHA)
	if explicitSHA != "" && explicitSHA != refSHA {
		args = append(args, explicitSHA)
	}
	if _, err := g.run(ctx, args...); err != nil {
		if explicitSHA != "" && (strings.Contains(err.Error(), "not our ref") || strings.Contains(err.Error(), "couldn't find remote ref")) {
			return &review.SubmissionError{Code: "unknown_commit", Message: "commit_sha does not exist in the pushed repository"}
		}
		return err
	}
	return nil
}

func (g gitSubmissionRepository) taskCommits(ctx context.Context, head string, taskID int64) ([]taskSourceCommit, error) {
	log, err := g.run(ctx, "log", "--topo-order", "--format=%H%x00%P%x00%s", head, "--")
	if err != nil {
		return nil, err
	}
	marker := fmt.Sprintf("(#%d)", taskID)
	var matches []taskSourceCommit
	for _, line := range strings.Split(log, "\n") {
		fields := strings.SplitN(line, "\x00", 3)
		if len(fields) == 3 && strings.Contains(fields[2], marker) {
			matches = append(matches, taskSourceCommit{sha: fields[0], parents: strings.Fields(fields[1])})
		}
	}
	if len(matches) == 0 {
		return nil, &review.SubmissionError{Code: "task_commits_not_found", Message: fmt.Sprintf("no commit subject contains %s on the submitted history; use that task marker or supply explicit source overrides", marker)}
	}
	return matches, nil
}
