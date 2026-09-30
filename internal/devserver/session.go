package devserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type SessionStore struct {
	root string
}

func NewSessionStore(sessionRoot string) *SessionStore {
	return &SessionStore{root: sessionRoot}
}

func SessionKey(project string, repoRoot string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(repoRoot)))
	return sanitizeKey(project) + "-" + hex.EncodeToString(sum[:])[:12]
}

// InstanceSessionKey names one of several simultaneous hosts of the same
// project checkout. An empty instance is the ordinary single session.
func InstanceSessionKey(project string, repoRoot string, instance string) string {
	key := SessionKey(project, repoRoot)
	if strings.TrimSpace(instance) == "" {
		return key
	}
	return key + "--" + sanitizeKey(instance)
}

func newSessionID(project string, now time.Time) string {
	return sanitizeKey(project) + "-" + now.UTC().Format("20060102T150405.000000000Z")
}

func sanitizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	re := regexp.MustCompile(`[^a-z0-9._-]+`)
	value = re.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-._")
	if value == "" {
		return "session"
	}
	return value
}

func (s *SessionStore) SessionDir(sessionKey string, sessionID string) string {
	return filepath.Join(s.root, sessionKey, sessionID)
}

func (s *SessionStore) CurrentPath(sessionKey string) string {
	return filepath.Join(s.root, sessionKey, "current.json")
}

func (s *SessionStore) WriteCurrent(session SessionState) error {
	if err := os.MkdirAll(filepath.Dir(session.StatePath), 0o700); err != nil {
		return fmt.Errorf("creating session state dir: %w", err)
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session state: %w", err)
	}
	tmp := session.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing session state temp file: %w", err)
	}
	if err := os.Rename(tmp, session.StatePath); err != nil {
		return fmt.Errorf("replacing session state: %w", err)
	}
	return nil
}

func (s *SessionStore) ReadCurrentByKey(sessionKey string) (SessionState, error) {
	return readSession(s.CurrentPath(sessionKey))
}

// FindInstance reads a named instance session; instances always need the repo.
func (s *SessionStore) FindInstance(project string, repoRoot string, instance string) (SessionState, error) {
	if strings.TrimSpace(instance) == "" {
		return s.FindCurrent(project, repoRoot)
	}
	if strings.TrimSpace(repoRoot) == "" {
		return SessionState{}, fmt.Errorf("%w: instance %q needs a repo root", ErrSessionNotFound, instance)
	}
	return s.ReadCurrentByKey(InstanceSessionKey(project, repoRoot, instance))
}

func (s *SessionStore) FindCurrent(project string, repoRoot string) (SessionState, error) {
	if strings.TrimSpace(repoRoot) != "" {
		return s.ReadCurrentByKey(SessionKey(project, repoRoot))
	}
	sessions, err := s.List()
	if err != nil {
		return SessionState{}, err
	}
	var matches []SessionState
	for _, session := range sessions {
		if session.Project == project && session.Instance == "" {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return SessionState{}, ErrSessionNotFound
	}
	if len(matches) > 1 {
		return SessionState{}, fmt.Errorf("%w: multiple sessions for %q; pass -repo", ErrSessionNotFound, project)
	}
	return matches[0], nil
}

func (s *SessionStore) List() ([]SessionState, error) {
	var sessions []SessionState
	if _, err := os.Stat(s.root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat session root: %w", err)
	}
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(path) != "current.json" {
			return nil
		}
		session, err := readSession(path)
		if err != nil {
			return err
		}
		sessions = append(sessions, session)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return sessions, nil
}

func readSession(path string) (SessionState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionState{}, ErrSessionNotFound
		}
		return SessionState{}, fmt.Errorf("reading session state: %w", err)
	}
	var session SessionState
	if err := json.Unmarshal(data, &session); err != nil {
		return SessionState{}, fmt.Errorf("parsing session state: %w", err)
	}
	return session, nil
}
