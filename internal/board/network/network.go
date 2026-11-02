// Package network shares crew-board boards with a few partner sites through
// one git repository: a signed push of this site's files, and a verified pull
// of everyone's (docs/crew-board.md, Shared boards across sites). Trust lives
// in this site's own config, never in the repo.
package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"crew-services/internal/board"
)

// Partner is another site of a network: its name, its SSH signing public key
// (pinned here), and how its mentions and replies may wake sessions here.
type Partner struct {
	Site       string `json:"site"`
	SigningKey string `json:"signing_key"`
	Wake       string `json:"wake,omitempty"`
}

// Config is one network as this site sees it.
type Config struct {
	Name string `json:"name"`
	// Repo is the shared repository: an SSH URL, or a path for a local test.
	Repo string `json:"repo"`
	// DeployKey is the SSH key git uses to reach Repo ("" for a path).
	DeployKey string `json:"deploy_key,omitempty"`
	// SigningKey is this site's SSH signing key (its .pub beside it).
	SigningKey string        `json:"signing_key"`
	Partners   []Partner     `json:"partners"`
	Branch     string        `json:"branch,omitempty"`
	PullEvery  time.Duration `json:"-"`
	// Dir is where the clone and the pinned signers file live.
	Dir string `json:"-"`
}

// LoadConfigs reads a networks file: {"networks": [Config…]}. Paths starting
// with ~/ are under home.
func LoadConfigs(path, home, stateDir string) ([]Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Networks []Config `json:"networks"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("networks file %s: %w", path, err)
	}
	expand := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return p
	}
	for i := range file.Networks {
		n := &file.Networks[i]
		n.DeployKey, n.SigningKey = expand(n.DeployKey), expand(n.SigningKey)
		if strings.HasPrefix(n.Repo, "~/") {
			n.Repo = expand(n.Repo)
		}
		n.Dir = filepath.Join(stateDir, n.Name)
	}
	return file.Networks, nil
}

// Info is what the board service needs to know about a network.
func (c Config) Info() board.NetworkInfo {
	partners := map[string]string{}
	for _, p := range c.Partners {
		wake := p.Wake
		if wake == "" {
			wake = "no_wake"
		}
		partners[p.Site] = wake
	}
	return board.NetworkInfo{Name: c.Name, Partners: partners}
}

// Applier adds files a pull read.
type Applier interface {
	ApplyRemote(ctx context.Context, f board.RemoteFile) error
}

// Syncer pushes this site's queued files to one network and pulls everyone
// else's.
type Syncer struct {
	cfg   Config
	site  string
	apply Applier
	store board.SharedStore
	now   func() time.Time
	log   *slog.Logger
	wake  chan struct{}
	mu    sync.Mutex
	// beforePush runs between a push's fetch and its push; tests use it to
	// make another site push first.
	beforePush func()
	// quiet is set while a reindex replays history.
	quiet bool
	// since is when this site joined the network.
	since time.Time
}

func NewSyncer(cfg Config, site string, apply Applier, store board.SharedStore, now func() time.Time, log *slog.Logger) *Syncer {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.PullEvery <= 0 {
		cfg.PullEvery = time.Minute
	}
	return &Syncer{cfg: cfg, site: site, apply: apply, store: store, now: now, log: log, wake: make(chan struct{}, 1)}
}

func (s *Syncer) Name() string { return s.cfg.Name }

// Poke asks for a sync pass now, for example after a write queued files.
func (s *Syncer) Poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Join records when this site started taking part in the network, the first
// time it does: posts written before then are history and notify nobody
// here; posts written after, even while this site was down, are not.
func (s *Syncer) Join(ctx context.Context) error {
	st, err := s.store.GetSyncState(ctx, s.cfg.Name)
	if err != nil && !errors.Is(err, board.ErrNotFound) {
		return err
	}
	if !st.JoinedAt.IsZero() {
		return nil
	}
	st.Network, st.JoinedAt, st.CheckedAt = s.cfg.Name, s.now().UTC(), s.now().UTC()
	return s.store.PutSyncState(ctx, st)
}

// Run syncs on every interval and whenever poked, until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	if err := s.Join(ctx); err != nil {
		s.log.Warn("network join", "network", s.cfg.Name, "error", err)
	}
	ticker := time.NewTicker(s.cfg.PullEvery)
	defer ticker.Stop()
	for {
		if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("network sync", "network", s.cfg.Name, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Pass pushes what is queued, then pulls what is new.
func (s *Syncer) Pass(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureClone(ctx); err != nil {
		return err
	}
	if err := s.push(ctx); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return s.pull(ctx)
}

// Reindex forgets how far the repo was read and reads it all again. Applying
// is idempotent, so this rebuilds what is missing, for example after the
// index was lost.
func (s *Syncer) Reindex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureClone(ctx); err != nil {
		return err
	}
	st, err := s.store.GetSyncState(ctx, s.cfg.Name)
	if err != nil && !errors.Is(err, board.ErrNotFound) {
		return err
	}
	st.Network, st.LastCommit = s.cfg.Name, ""
	if err := s.store.PutSyncState(ctx, st); err != nil {
		return err
	}
	// Replaying history rebuilds the index but notifies nobody.
	s.quiet = true
	defer func() { s.quiet = false }()
	return s.pull(ctx)
}

func (s *Syncer) repoDir() string     { return filepath.Join(s.cfg.Dir, "repo") }
func (s *Syncer) signersFile() string { return filepath.Join(s.cfg.Dir, "allowed_signers") }

// git runs git in the clone with this site's identity, its signing key, the
// pinned signers, and the deploy key for transport.
func (s *Syncer) git(ctx context.Context, dir string, args ...string) (string, error) {
	base := []string{"-c", "user.name=" + s.site, "-c", "user.email=" + s.site + "@crew-board.invalid",
		"-c", "gpg.format=ssh", "-c", "user.signingkey=" + s.cfg.SigningKey,
		"-c", "gpg.ssh.allowedSignersFile=" + s.signersFile(), "-c", "commit.gpgsign=true",
		"-c", "core.quotepath=off", "-c", "advice.detachedHead=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if s.cfg.DeployKey != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -i "+s.cfg.DeployKey+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ensureClone makes the clone if it is missing, and writes the pinned signers
// file from this site's config: its own key and each partner's, each under
// its site's name.
func (s *Syncer) ensureClone(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.Dir, 0o700); err != nil {
		return err
	}
	own, err := os.ReadFile(s.cfg.SigningKey + ".pub")
	if err != nil {
		return fmt.Errorf("this site's signing key: %w", err)
	}
	var signers strings.Builder
	fmt.Fprintf(&signers, "%s namespaces=\"git\" %s\n", s.site, strings.TrimSpace(string(own)))
	for _, p := range s.cfg.Partners {
		fmt.Fprintf(&signers, "%s namespaces=\"git\" %s\n", p.Site, strings.TrimSpace(p.SigningKey))
	}
	if err := os.WriteFile(s.signersFile(), []byte(signers.String()), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.repoDir(), ".git")); err == nil {
		return nil
	}
	_, err = s.git(ctx, s.cfg.Dir, "clone", "-q", s.cfg.Repo, s.repoDir())
	return err
}

// remoteHead is the network branch's newest commit, or "" while the repo is
// empty.
func (s *Syncer) remoteHead(ctx context.Context) (string, error) {
	if _, err := s.git(ctx, s.repoDir(), "fetch", "-q", "origin"); err != nil {
		return "", err
	}
	out, err := s.git(ctx, s.repoDir(), "rev-parse", "-q", "--verify", "refs/remotes/origin/"+s.cfg.Branch)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// push commits the queued files on top of the network's newest commit and
// pushes them. If another site pushed first, it starts again from their
// commit; the outbox, not the clone, holds what is unpushed, so nothing is
// lost.
func (s *Syncer) push(ctx context.Context) error {
	files, err := s.store.PendingOutbox(ctx, s.cfg.Name, 500)
	if err != nil || len(files) == 0 {
		return err
	}
	dir := s.repoDir()
	for attempt := 0; attempt < 5; attempt++ {
		head, err := s.remoteHead(ctx)
		if err != nil {
			return err
		}
		if head != "" {
			if _, err := s.git(ctx, dir, "checkout", "-q", "--detach", head); err != nil {
				return err
			}
			if _, err := s.git(ctx, dir, "reset", "-q", "--hard", head); err != nil {
				return err
			}
		}
		if _, err := s.git(ctx, dir, "clean", "-qfd"); err != nil {
			return err
		}
		var ids []int64
		wrote := 0
		for _, f := range files {
			ids = append(ids, f.ID)
			target := filepath.Join(dir, filepath.FromSlash(f.Path))
			if existing, err := os.ReadFile(target); err == nil {
				// Already there: the same file pushed before a lost answer, or
				// a board of that name another site made first.
				_ = existing
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(target, f.Content, 0o600); err != nil {
				return err
			}
			wrote++
		}
		if wrote == 0 {
			return s.store.OutboxDone(ctx, ids, head)
		}
		if _, err := s.git(ctx, dir, "add", "-A"); err != nil {
			return err
		}
		if _, err := s.git(ctx, dir, "commit", "-q", "-S", "-m", fmt.Sprintf("crew-board: %d %s from %s", wrote, map[bool]string{true: "file", false: "files"}[wrote == 1], s.site)); err != nil {
			return err
		}
		if s.beforePush != nil {
			hook := s.beforePush
			s.beforePush = nil
			hook()
		}
		if _, err := s.git(ctx, dir, "push", "-q", "origin", "HEAD:refs/heads/"+s.cfg.Branch); err != nil {
			s.log.Info("push lost a race; starting again from the newest commit", "network", s.cfg.Name, "attempt", attempt+1)
			continue
		}
		commit, err := s.git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		return s.store.OutboxDone(ctx, ids, strings.TrimSpace(commit))
	}
	return errors.New("gave up after 5 push races; will try again")
}

// pull reads every commit since the last one read, in order. A commit counts
// only if a pinned key signed it; within it, only added files count, each
// checked by the board service against its signer.
func (s *Syncer) pull(ctx context.Context) error {
	head, err := s.remoteHead(ctx)
	if err != nil {
		return err
	}
	st, err := s.store.GetSyncState(ctx, s.cfg.Name)
	if err != nil && !errors.Is(err, board.ErrNotFound) {
		return err
	}
	if head == "" {
		st.Network = s.cfg.Name
		return s.recordState(ctx, st, "empty")
	}
	st.Network = s.cfg.Name
	if st.JoinedAt.IsZero() {
		// The first read of this network: everything already in it is
		// history here.
		st.JoinedAt = s.now().UTC()
	}
	s.since = st.JoinedAt
	if st.LastCommit != "" && st.Status == "history_rewritten" {
		return errors.New("the network's history was rewritten; run reindex once it is safe")
	}
	rangeArg := head
	if st.LastCommit != "" {
		if st.LastCommit == head {
			return s.recordState(ctx, st, "ok")
		}
		if _, err := s.git(ctx, s.repoDir(), "merge-base", "--is-ancestor", st.LastCommit, head); err != nil {
			s.log.Warn("network history does not extend what was read; refusing it", "network", s.cfg.Name, "last", st.LastCommit, "head", head)
			_ = s.recordState(ctx, st, "history_rewritten")
			return errors.New("the network's history does not extend the commits already read")
		}
		rangeArg = st.LastCommit + ".." + head
	}
	out, err := s.git(ctx, s.repoDir(), "rev-list", "--reverse", "--topo-order", rangeArg)
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(out) {
		if err := s.readCommit(ctx, commit); err != nil {
			return err
		}
		st.LastCommit = commit
		if err := s.recordState(ctx, st, "ok"); err != nil {
			return err
		}
	}
	return s.recordState(ctx, st, "ok")
}

func (s *Syncer) recordState(ctx context.Context, st board.SyncState, status string) error {
	st.Status, st.CheckedAt = status, s.now().UTC()
	st.RepoBytes = dirBytes(filepath.Join(s.repoDir(), ".git"))
	return s.store.PutSyncState(ctx, st)
}

func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func (s *Syncer) flag(ctx context.Context, commit, path, reason string) {
	s.log.Warn("network file ignored", "network", s.cfg.Name, "commit", commit, "path", path, "reason", reason)
	_ = s.store.AddFlag(ctx, board.SyncFlag{Network: s.cfg.Name, Commit: commit, Path: path, Reason: reason, CreatedAt: s.now().UTC()})
}

// applyOrder applies a commit's files so each finds what it needs: blobs,
// then boards, threads, posts by revision, and events.
var applyOrder = map[string]int{"blob": 0, "board": 1, "thread": 2, "post": 3, "event": 4}

const maxFileBytes = 100 << 20

func (s *Syncer) readCommit(ctx context.Context, commit string) error {
	dir := s.repoDir()
	sig, err := s.git(ctx, dir, "log", "-1", "--format=%G?%x00%GS", commit)
	if err != nil {
		return err
	}
	status, signer, _ := strings.Cut(strings.TrimSpace(sig), "\x00")
	changes, err := s.git(ctx, dir, "diff-tree", "--no-commit-id", "--root", "-r", "--no-renames", "--name-status", "-z", commit)
	if err != nil {
		return err
	}
	type added struct {
		path string
		file board.RepoFile
	}
	var adds []added
	parts := strings.Split(strings.TrimSuffix(changes, "\x00"), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		kind, path := parts[i], parts[i+1]
		if status != "G" {
			s.flag(ctx, commit, path, "the commit is not signed by a pinned site key (signature status "+status+")")
			continue
		}
		if kind != "A" {
			s.flag(ctx, commit, path, "an existing file was changed or deleted ("+kind+"); files are only ever added")
			continue
		}
		file, ok := board.ClassifyPath(path)
		if !ok {
			s.flag(ctx, commit, path, "not part of the shared-board layout")
			continue
		}
		adds = append(adds, added{path, file})
	}
	sort.SliceStable(adds, func(i, j int) bool {
		a, b := adds[i].file, adds[j].file
		if applyOrder[a.Kind] != applyOrder[b.Kind] {
			return applyOrder[a.Kind] < applyOrder[b.Kind]
		}
		return a.Revision < b.Revision
	})
	for _, a := range adds {
		size, err := s.git(ctx, dir, "cat-file", "-s", commit+":"+a.path)
		if err != nil {
			return err
		}
		if n, _ := strconv.ParseInt(strings.TrimSpace(size), 10, 64); n > maxFileBytes {
			s.flag(ctx, commit, a.path, "the file is over the size cap")
			continue
		}
		data, err := s.git(ctx, dir, "cat-file", "blob", commit+":"+a.path)
		if err != nil {
			return err
		}
		err = s.apply.ApplyRemote(ctx, board.RemoteFile{Network: s.cfg.Name, Commit: commit, Signer: signer, Path: a.path, File: a.file, Data: []byte(data), Quiet: s.quiet, Since: s.since})
		var ignored *board.Ignored
		switch {
		case errors.As(err, &ignored):
			s.flag(ctx, commit, a.path, ignored.Reason)
		case err != nil:
			return fmt.Errorf("apply %s from %s: %w", a.path, commit, err)
		}
	}
	return nil
}
