// Package hosting gives each playtest session its own product host, started
// from the repository's ordinary serve manifest. The backend launcher then
// opens that host; the world belongs to the session and ends with it.
package hosting

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"crew-services/internal/devserver"
	"crew-services/internal/playtest/session"
)

// Hosts is the subset of the devserver manager a session needs.
type Hosts interface {
	Up(context.Context, devserver.UpOptions) (devserver.UpResult, error)
	Stop(context.Context, devserver.StopOptions) (devserver.StopResult, error)
	Running(context.Context, devserver.StatusOptions) (devserver.SessionState, bool, error)
}

// EndReason looks up why a host ended, by the label it was launched with.
// It returns "" when the host left no reason (a crash leaves none).
type EndReason func(ctx context.Context, label string) string

// ManifestReader resolves the project a repository's manifest serves.
type ManifestReader func(repoRoot, explicitPath string) (project string, err error)

// Launcher starts a session's product host before delegating to Inner.
// Profiles without a host pass straight through.
type Launcher struct {
	Inner    session.Launcher
	Hosts    Hosts
	Manifest ManifestReader
	// Locks is shared by every slot so one checkout starts one host at a time.
	Locks *RepoLocks
	// Ended optionally explains a host that ended by itself.
	Ended EndReason
}

// Label is the owner label a playtest session's host is launched with.
func Label(sessionID string) string { return "crew-playtest:" + sessionID }

// RepoLocks serializes host startup per checkout. Products such as `rusty dev`
// build and stage into the checkout, so simultaneous first starts would race.
type RepoLocks struct {
	mu    sync.Mutex
	repos map[string]*sync.Mutex
}

// ManifestProject reads the project name from a repository's serve manifest.
func ManifestProject(cfg devserver.ManagerConfig) ManifestReader {
	return func(repoRoot, explicitPath string) (string, error) {
		path, err := devserver.FindManifest(repoRoot, explicitPath)
		if err != nil {
			return "", err
		}
		manifest, err := devserver.LoadServeManifest(path, repoRoot, cfg)
		if err != nil {
			return "", err
		}
		return manifest.Project, nil
	}
}

func (l *RepoLocks) forRepo(repo string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.repos == nil {
		l.repos = map[string]*sync.Mutex{}
	}
	key := filepath.Clean(repo)
	if l.repos[key] == nil {
		l.repos[key] = &sync.Mutex{}
	}
	return l.repos[key]
}

func (l *Launcher) Launch(ctx context.Context, id string, p session.Profile) (map[string]any, error) {
	if p.Host == nil {
		return l.Inner.Launch(ctx, id, p)
	}
	project, err := l.Manifest(p.Host.Repo, p.Host.Manifest)
	if err != nil {
		return nil, fmt.Errorf("host_unavailable: %w", err)
	}
	lock := l.Locks.forRepo(p.Host.Repo)
	if err := lockContext(ctx, lock); err != nil {
		return nil, fmt.Errorf("host_unavailable: waiting for another host of this checkout to start: %w", err)
	}
	up, err := l.Hosts.Up(ctx, devserver.UpOptions{Project: project, RepoRoot: p.Host.Repo, ManifestPath: p.Host.Manifest, Instance: id,
		Label: Label(id), Keep: session.LaunchOptionsFrom(ctx).Keep})
	lock.Unlock()
	if err != nil {
		if up.Session.PID <= 0 {
			return nil, fmt.Errorf("host_start_failed: %w", err)
		}
		// The broker started a process: report it so the session releases it.
		facts := hostFacts(up.Session)
		return map[string]any{"host": facts}, fmt.Errorf("host_start_failed: %w (logs: %v, %v)", err, facts["stdout_log"], facts["stderr_log"])
	}
	path := p.Host.Path
	if path == "" {
		path = "/"
	}
	p.URL = strings.TrimRight(up.Session.LocalURL, "/") + path
	launched, err := l.Inner.Launch(ctx, id, p)
	if launched == nil {
		launched = map[string]any{}
	}
	launched["host"] = hostFacts(up.Session)
	launched["session_url"] = p.URL
	return launched, err
}

// ReleaseHost stops the host recorded at launch. It never rereads the
// repository manifest: an edited or removed manifest must not redirect or
// block cleanup. A host that already exited is released.
func (l *Launcher) ReleaseHost(ctx context.Context, id string, p session.Profile, host map[string]any) (map[string]any, error) {
	if p.Host == nil || host == nil {
		return map[string]any{"stopped": false, "message": "no host was started for this session"}, nil
	}
	project, _ := host["project"].(string)
	repo, _ := host["repo_root"].(string)
	instance, _ := host["instance"].(string)
	if project == "" || repo == "" || instance == "" {
		return nil, fmt.Errorf("recorded host identity is incomplete: project %q, repo %q, instance %q", project, repo, instance)
	}
	stopped, err := l.Hosts.Stop(ctx, devserver.StopOptions{Project: project, RepoRoot: repo, Instance: instance})
	if err != nil {
		// Includes ErrSessionNotFound: a recorded host without its state is
		// not known to be gone, so it stays the session's to resolve.
		return nil, fmt.Errorf("stopping recorded host %s · %s: %w", project, instance, err)
	}
	receipt := hostFacts(stopped.Session)
	receipt["stopped"] = stopped.Stopped
	receipt["message"] = stopped.Message
	return receipt, nil
}

// HostEnded reports whether the host recorded at launch has exited. Like
// ReleaseHost it trusts only the recorded identity. A missing state record is
// an error, not an exit: the host is not known to be gone.
func (l *Launcher) HostEnded(ctx context.Context, id string, p session.Profile, host map[string]any) (bool, string, error) {
	if p.Host == nil || host == nil {
		return false, "", nil
	}
	project, _ := host["project"].(string)
	repo, _ := host["repo_root"].(string)
	instance, _ := host["instance"].(string)
	if project == "" || repo == "" || instance == "" {
		return false, "", fmt.Errorf("recorded host identity is incomplete: project %q, repo %q, instance %q", project, repo, instance)
	}
	_, running, err := l.Hosts.Running(ctx, devserver.StatusOptions{Project: project, RepoRoot: repo, Instance: instance})
	if err != nil || running {
		return false, "", err
	}
	reason := ""
	if l.Ended != nil {
		label, _ := host["label"].(string)
		if label == "" {
			label = Label(id)
		}
		reason = l.Ended(ctx, label)
	}
	if reason == "" {
		reason = "the product host exited"
	}
	return true, reason, nil
}

func hostFacts(s devserver.SessionState) map[string]any {
	return map[string]any{
		"project": s.Project, "instance": s.Instance, "repo_root": s.RepoRoot, "label": s.Label, "keep": s.Keep,
		"local_url": s.LocalURL, "lan_url": s.LANURL, "port": s.Port, "pid": s.PID,
		"status": s.Status, "launch_fingerprint": s.LaunchFingerprint,
		"stdout_log": s.StdoutLog, "stderr_log": s.StderrLog,
	}
}

func lockContext(ctx context.Context, lock *sync.Mutex) error {
	acquired := make(chan struct{})
	go func() {
		lock.Lock()
		select {
		case acquired <- struct{}{}:
		case <-ctx.Done():
			lock.Unlock()
		}
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
