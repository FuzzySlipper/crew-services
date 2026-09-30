package devserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Manager struct {
	cfg        ManagerConfig
	httpClient *http.Client
	clock      func() time.Time
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg: normalized,
		httpClient: &http.Client{
			Timeout: normalized.Timeouts.HealthTimeout,
		},
		clock: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (m *Manager) Up(ctx context.Context, options UpOptions) (UpResult, error) {
	if strings.TrimSpace(options.Project) == "" {
		return UpResult{}, errors.New("project is required")
	}
	repoRoot, err := resolveRepoRoot(options.RepoRoot)
	if err != nil {
		return UpResult{}, err
	}
	manifestPath, err := FindManifest(repoRoot, options.ManifestPath)
	if err != nil {
		return UpResult{}, err
	}
	manifest, err := LoadServeManifest(manifestPath, repoRoot, m.cfg)
	if err != nil {
		return UpResult{}, err
	}
	if manifest.Project != options.Project {
		return UpResult{}, fmt.Errorf("%w: requested project %q but manifest project is %q", ErrInvalidManifest, options.Project, manifest.Project)
	}
	if strings.TrimSpace(options.PublicHostOverride) != "" {
		manifest.PublicHost = options.PublicHostOverride
	}
	intendedFingerprint, err := ResolveLaunchFingerprint(ctx, manifest)
	if err != nil {
		return UpResult{}, fmt.Errorf("fingerprinting intended launch: %w", err)
	}
	sessionKey := InstanceSessionKey(manifest.Project, repoRoot, options.Instance)
	registry := NewLeaseRegistry(m.cfg.StateDir)
	if err := registry.Lock(ctx, m.cfg.Timeouts.LockTimeout); err != nil {
		return UpResult{}, err
	}
	defer registry.Unlock()
	leases, err := registry.Load()
	if err != nil {
		return UpResult{}, err
	}
	leases = pruneDeadLeases(leases)
	store := NewSessionStore(m.cfg.SessionRoot)
	restartSource := ""
	if existing, ok := m.currentSession(ctx, store, manifest, sessionKey); ok {
		existing.CurrentFingerprint = intendedFingerprint
		existing.FingerprintError = ""
		existing.Stale = existing.LaunchFingerprint.Value != intendedFingerprint.Value
		if existing.Stale {
			existing.StaleReason = fingerprintDriftReason(existing.LaunchFingerprint, intendedFingerprint)
		} else {
			existing.StaleReason = ""
		}
		canReuse := existing.Health.Matched && !existing.Stale && manifest.ReusePolicy != ReusePolicyNever
		if canReuse && (existing.Ownership == "broker_owned" || manifest.ReusePolicy == ReusePolicyExplicit) {
			existing.SchemaVersion = SessionSchemaV1
			existing.LastCheckedAt = m.clock()
			existing.Status = "running"
			if err := store.WriteCurrent(existing); err != nil {
				return UpResult{}, err
			}
			return UpResult{Session: existing, Reused: true}, nil
		}
		if existing.Ownership == "unowned" && existing.Health.Matched && existing.Stale && manifest.ReusePolicy == ReusePolicyExplicit {
			existing.Status = "stale"
			existing.LastCheckedAt = m.clock()
			if err := store.WriteCurrent(existing); err != nil {
				return UpResult{}, err
			}
			return UpResult{Session: existing}, fmt.Errorf(
				"%w: unowned session cannot be restarted safely; restart the external host, then run den-serve up again",
				ErrSessionStale,
			)
		}
		if existing.Ownership == "broker_owned" && existing.PID > 0 && processGroupAlive(existing.PID) {
			switch {
			case existing.Stale:
				restartSource = "restarted_stale"
			case !existing.Health.Matched:
				restartSource = "restarted_unhealthy"
			default:
				restartSource = "restarted_reuse_disabled"
			}
			if err := StopProcessGroup(existing.PID, m.cfg.Timeouts.ShutdownTimeout); err != nil {
				return UpResult{}, fmt.Errorf("stopping previous broker-owned process group: %w", err)
			}
			leases = removeLease(leases, existing.SessionKey)
		}
	}
	port, reused, reuseSource, health, err := m.selectPortOrReuse(ctx, manifest, leases, options.Instance)
	if err != nil {
		_ = registry.Save(leases)
		return UpResult{}, err
	}
	now := m.clock()
	sessionID := newSessionID(manifest.Project, now)
	sessionDir := store.SessionDir(sessionKey, sessionID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return UpResult{}, fmt.Errorf("creating session dir: %w", err)
	}
	publicHost := ResolvePublicHost(manifest.PublicHost)
	local := localURL(manifest.ProbeHost, port)
	lan := publicURL(publicHost, port)
	values := templateContext{
		project:    manifest.Project,
		repoRoot:   manifest.RepoRoot,
		bindHost:   manifest.BindHost,
		probeHost:  manifest.ProbeHost,
		port:       port,
		localURL:   local,
		publicURL:  lan,
		sessionDir: sessionDir,
	}
	command := renderTemplate(manifest.Command, values)
	session := SessionState{
		SchemaVersion:      SessionSchemaV1,
		SessionID:          sessionID,
		SessionKey:         sessionKey,
		Instance:           strings.TrimSpace(options.Instance),
		Project:            manifest.Project,
		Target:             manifest.Target,
		RepoRoot:           manifest.RepoRoot,
		ManifestPath:       manifest.ManifestPath,
		LaunchFingerprint:  intendedFingerprint,
		CurrentFingerprint: intendedFingerprint,
		Command:            command,
		BindHost:           manifest.BindHost,
		ProbeHost:          manifest.ProbeHost,
		PublicHost:         publicHost,
		PublicHostOverride: strings.TrimSpace(options.PublicHostOverride),
		Port:               port,
		LocalURL:           local,
		LANURL:             lan,
		HealthURL:          healthURL(manifest.ProbeHost, port, manifest.HealthPath),
		ReuseSource:        reuseSource,
		Status:             "running",
		Health:             health,
		StartedAt:          now,
		LastCheckedAt:      now,
		StdoutLog:          filepath.Join(sessionDir, "server.stdout.log"),
		StderrLog:          filepath.Join(sessionDir, "server.stderr.log"),
		StatePath:          store.CurrentPath(sessionKey),
		SessionDir:         sessionDir,
	}
	hash, err := ManifestHash(manifest.ManifestPath)
	if err != nil {
		return UpResult{}, err
	}
	session.ManifestHash = hash
	if reused {
		if lease := findLeaseForPort(leases, manifest.ProbeHost, port); lease != nil {
			session.PID = lease.PID
			session.Ownership = "broker_owned"
		} else {
			session.Ownership = "unowned"
		}
		if err := store.WriteCurrent(session); err != nil {
			return UpResult{}, err
		}
		return UpResult{Session: session, Reused: true}, nil
	}
	env := map[string]string{
		"HOST":                  manifest.BindHost,
		"PORT":                  strconv.Itoa(port),
		"BASE_URL":              local,
		"DEN_SERVE_LOCAL_URL":   local,
		"DEN_SERVE_PUBLIC_URL":  lan,
		"DEN_SERVE_SESSION_DIR": sessionDir,
	}
	for key, value := range manifest.Environment {
		env[key] = renderTemplate(value, values)
	}
	process, err := startManagedProcess(command, manifest.RepoRoot, env, session.StdoutLog, session.StderrLog)
	if err != nil {
		_ = registry.Save(leases)
		return UpResult{}, err
	}
	session.PID = process.PID
	session.Ownership = "broker_owned"
	session.ReuseSource = "started"
	if restartSource != "" {
		session.ReuseSource = restartSource
	}
	health, err = waitForHealth(ctx, m.httpClient, manifest, port)
	session.Health = health
	if err != nil {
		_ = StopProcessGroup(process.PID, m.cfg.Timeouts.ShutdownTimeout)
		session.Status = "failed"
		_ = store.WriteCurrent(session)
		_ = registry.Save(leases)
		return UpResult{Session: session}, err
	}
	launchedFingerprint, err := ResolveLaunchFingerprint(ctx, manifest)
	if err != nil {
		_ = StopProcessGroup(process.PID, m.cfg.Timeouts.ShutdownTimeout)
		session.Status = "failed"
		session.FingerprintError = err.Error()
		_ = store.WriteCurrent(session)
		_ = registry.Save(leases)
		return UpResult{Session: session}, fmt.Errorf("fingerprinting completed launch: %w", err)
	}
	session.LaunchFingerprint = launchedFingerprint
	session.CurrentFingerprint = launchedFingerprint
	lease := LeaseRecord{
		SessionID:  session.SessionID,
		SessionKey: session.SessionKey,
		Project:    session.Project,
		Target:     session.Target,
		RepoRoot:   session.RepoRoot,
		BindHost:   session.BindHost,
		ProbeHost:  session.ProbeHost,
		Port:       session.Port,
		LocalURL:   session.LocalURL,
		LANURL:     session.LANURL,
		Command:    session.Command,
		PID:        session.PID,
		StartedAt:  session.StartedAt,
	}
	leases = upsertLease(leases, lease)
	if err := registry.Save(leases); err != nil {
		_ = StopProcessGroup(process.PID, m.cfg.Timeouts.ShutdownTimeout)
		return UpResult{}, err
	}
	if err := store.WriteCurrent(session); err != nil {
		return UpResult{}, err
	}
	return UpResult{Session: session, Started: true, Restarted: restartSource != ""}, nil
}

func (m *Manager) Status(ctx context.Context, options StatusOptions) (SessionState, error) {
	store := NewSessionStore(m.cfg.SessionRoot)
	repoRoot := strings.TrimSpace(options.RepoRoot)
	if repoRoot != "" {
		resolved, err := resolveRepoRoot(repoRoot)
		if err != nil {
			return SessionState{}, err
		}
		repoRoot = resolved
	}
	session, err := store.FindInstance(options.Project, repoRoot, options.Instance)
	if err != nil {
		return SessionState{}, err
	}
	return m.refreshSession(ctx, store, session)
}

func (m *Manager) List(ctx context.Context) ([]SessionState, error) {
	store := NewSessionStore(m.cfg.SessionRoot)
	sessions, err := store.List()
	if err != nil {
		return nil, err
	}
	refreshed := make([]SessionState, 0, len(sessions))
	for _, session := range sessions {
		current, err := m.refreshSession(ctx, store, session)
		if err != nil {
			refreshed = append(refreshed, session)
			continue
		}
		refreshed = append(refreshed, current)
	}
	return refreshed, nil
}

func (m *Manager) Stop(ctx context.Context, options StopOptions) (StopResult, error) {
	store := NewSessionStore(m.cfg.SessionRoot)
	repoRoot := strings.TrimSpace(options.RepoRoot)
	if repoRoot != "" {
		resolved, err := resolveRepoRoot(repoRoot)
		if err != nil {
			return StopResult{}, err
		}
		repoRoot = resolved
	}
	session, err := store.FindInstance(options.Project, repoRoot, options.Instance)
	if err != nil {
		return StopResult{}, err
	}
	if session.Ownership != "broker_owned" || session.PID <= 0 {
		session.Status = "unowned"
		session.LastCheckedAt = m.clock()
		if err := store.WriteCurrent(session); err != nil {
			return StopResult{}, err
		}
		return StopResult{Session: session, Message: "session is not broker-owned; leaving process untouched"}, nil
	}
	if err := StopProcessGroup(session.PID, m.cfg.Timeouts.ShutdownTimeout); err != nil {
		return StopResult{}, err
	}
	registry := NewLeaseRegistry(m.cfg.StateDir)
	if err := registry.Lock(ctx, m.cfg.Timeouts.LockTimeout); err != nil {
		return StopResult{}, err
	}
	leases, loadErr := registry.Load()
	if loadErr == nil {
		_ = registry.Save(removeLease(leases, session.SessionKey))
	}
	_ = registry.Unlock()
	session.Status = "stopped"
	session.LastCheckedAt = m.clock()
	if err := store.WriteCurrent(session); err != nil {
		return StopResult{}, err
	}
	return StopResult{Session: session, Stopped: true, Message: "stopped broker-owned process group"}, nil
}

func (m *Manager) currentSession(ctx context.Context, store *SessionStore, manifest *ServeManifest, sessionKey string) (SessionState, bool) {
	session, err := store.ReadCurrentByKey(sessionKey)
	if err != nil {
		return SessionState{}, false
	}
	switch session.Ownership {
	case "broker_owned":
		if session.PID <= 0 || !processGroupAlive(session.PID) {
			return SessionState{}, false
		}
	case "unowned":
	default:
		return SessionState{}, false
	}
	health := checkHealth(ctx, m.httpClient, manifest, session.Port)
	session.Health = health
	return session, true
}

func (m *Manager) refreshSession(ctx context.Context, store *SessionStore, session SessionState) (SessionState, error) {
	manifest := &ServeManifest{
		Project:        session.Project,
		ProbeHost:      session.ProbeHost,
		HealthPath:     strings.TrimPrefix(strings.TrimPrefix(session.HealthURL, "http://"+session.ProbeHost+":"+strconv.Itoa(session.Port)), ""),
		ReadyText:      "",
		IdentityHeader: "",
	}
	var manifestLoadErr error
	if session.ManifestPath != "" {
		loaded, err := LoadServeManifest(session.ManifestPath, session.RepoRoot, m.cfg)
		if err == nil {
			manifest = loaded
			if session.PublicHostOverride != "" {
				manifest.PublicHost = session.PublicHostOverride
			}
		} else {
			manifestLoadErr = fmt.Errorf("loading persisted manifest: %w", err)
		}
	}
	currentFingerprint, fingerprintErr := ResolveLaunchFingerprint(ctx, manifest)
	if manifestLoadErr != nil {
		session.FingerprintError = manifestLoadErr.Error()
	} else if fingerprintErr != nil {
		session.FingerprintError = fingerprintErr.Error()
	} else {
		session.CurrentFingerprint = currentFingerprint
		session.FingerprintError = ""
		session.Stale = session.LaunchFingerprint.Value != currentFingerprint.Value
		if session.Stale {
			session.StaleReason = fingerprintDriftReason(session.LaunchFingerprint, currentFingerprint)
		} else {
			session.StaleReason = ""
		}
	}
	health := HealthResult{URL: healthURL(manifest.ProbeHost, session.Port, manifest.HealthPath)}
	if manifestLoadErr != nil {
		health.Error = manifestLoadErr.Error()
	} else {
		health = checkHealth(ctx, m.httpClient, manifest, session.Port)
	}
	session.Health = health
	session.LastCheckedAt = m.clock()
	switch {
	case session.PID > 0 && !processGroupAlive(session.PID):
		session.Status = "stopped"
	case health.Matched && session.Stale:
		session.Status = "stale"
	case health.Matched:
		session.Status = "running"
	case session.PID == 0 && session.Ownership == "unowned":
		session.Status = "unowned"
	default:
		session.Status = "unhealthy"
	}
	if session.StatePath != "" {
		if err := store.WriteCurrent(session); err != nil {
			return SessionState{}, err
		}
	}
	return session, nil
}

func (m *Manager) selectPortOrReuse(ctx context.Context, manifest *ServeManifest, leases []LeaseRecord, instance string) (int, bool, string, HealthResult, error) {
	blocked := make(map[int]bool)
	for _, lease := range leases {
		if lease.ProbeHost == manifest.ProbeHost && processGroupAlive(lease.PID) {
			blocked[lease.Port] = true
		}
	}
	// An instance never adopts the preferred port: a healthy host there is the
	// ordinary session or another instance, whose world it must not share.
	if manifest.PreferredPort > 0 && strings.TrimSpace(instance) == "" {
		port := manifest.PreferredPort
		if portInUse(manifest.ProbeHost, port, m.cfg.Timeouts.HealthTimeout) {
			health := checkHealth(ctx, m.httpClient, manifest, port)
			lease := findLeaseForPort(leases, manifest.ProbeHost, port)
			if health.Matched && manifest.ReusePolicy != ReusePolicyNever {
				if lease != nil && processGroupAlive(lease.PID) && lease.Project == manifest.Project && lease.RepoRoot == manifest.RepoRoot {
					return port, true, "broker_owned", health, nil
				}
				if manifest.ReusePolicy == ReusePolicyExplicit {
					return port, true, "explicit", health, nil
				}
			}
			blocked[port] = true
		} else {
			return port, false, "", HealthResult{URL: healthURL(manifest.ProbeHost, port, manifest.HealthPath)}, nil
		}
	}
	portRange := manifest.EffectivePortRange(m.cfg)
	port, err := findFreePort(manifest.BindHost, portRange, blocked)
	if err != nil {
		return 0, false, "", HealthResult{}, err
	}
	return port, false, "", HealthResult{URL: healthURL(manifest.ProbeHost, port, manifest.HealthPath)}, nil
}
