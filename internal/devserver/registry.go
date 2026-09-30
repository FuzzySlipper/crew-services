package devserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type LeaseRegistry struct {
	dir      string
	path     string
	lockPath string
	lockFile *os.File
}

type leaseFile struct {
	Leases []LeaseRecord `json:"leases"`
}

type LeaseRecord struct {
	SessionID  string    `json:"session_id"`
	SessionKey string    `json:"session_key"`
	Project    string    `json:"project"`
	Target     string    `json:"target"`
	RepoRoot   string    `json:"repo_root"`
	BindHost   string    `json:"bind_host"`
	ProbeHost  string    `json:"probe_host"`
	Port       int       `json:"port"`
	LocalURL   string    `json:"local_url"`
	LANURL     string    `json:"lan_url,omitempty"`
	Command    string    `json:"command"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
}

func NewLeaseRegistry(stateDir string) *LeaseRegistry {
	return &LeaseRegistry{
		dir:      stateDir,
		path:     filepath.Join(stateDir, "leases.json"),
		lockPath: filepath.Join(stateDir, "leases.lock"),
	}
}

func (r *LeaseRegistry) Lock(ctx context.Context, timeout time.Duration) error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("creating broker state dir: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		file, err := os.OpenFile(r.lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			r.lockFile = file
			_, _ = fmt.Fprintf(file, "pid=%d acquired_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("acquiring broker lock: %w", err)
		}
		if stale, staleErr := r.removeStaleLock(); staleErr != nil {
			return staleErr
		} else if stale {
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("broker lease lock timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *LeaseRegistry) removeStaleLock() (bool, error) {
	ownerPID, ok, err := readLockOwnerPID(r.lockPath)
	if err != nil {
		return false, err
	}
	if !ok || processAlive(ownerPID) {
		return false, nil
	}
	if err := os.Remove(r.lockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("removing stale broker lock: %w", err)
	}
	return true, nil
}

func readLockOwnerPID(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading broker lock: %w", err)
	}
	for _, field := range strings.Fields(string(data)) {
		rawPID, ok := strings.CutPrefix(field, "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(rawPID)
		if err != nil || pid <= 0 {
			return 0, false, nil
		}
		return pid, true, nil
	}
	return 0, false, nil
}

func (r *LeaseRegistry) Unlock() error {
	if r.lockFile != nil {
		if err := r.lockFile.Close(); err != nil {
			return fmt.Errorf("closing broker lock: %w", err)
		}
		r.lockFile = nil
	}
	if err := os.Remove(r.lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing broker lock: %w", err)
	}
	return nil
}

func (r *LeaseRegistry) Load() ([]LeaseRecord, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading lease registry: %w", err)
	}
	var file leaseFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing lease registry: %w", err)
	}
	return file.Leases, nil
}

func (r *LeaseRegistry) Save(leases []LeaseRecord) error {
	data, err := json.MarshalIndent(leaseFile{Leases: leases}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding lease registry: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing lease registry temp file: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("replacing lease registry: %w", err)
	}
	return nil
}

func pruneDeadLeases(leases []LeaseRecord) []LeaseRecord {
	filtered := leases[:0]
	for _, lease := range leases {
		if processGroupAlive(lease.PID) {
			filtered = append(filtered, lease)
		}
	}
	return filtered
}

func upsertLease(leases []LeaseRecord, lease LeaseRecord) []LeaseRecord {
	for index := range leases {
		if leases[index].SessionKey == lease.SessionKey {
			leases[index] = lease
			return leases
		}
	}
	return append(leases, lease)
}

func removeLease(leases []LeaseRecord, sessionKey string) []LeaseRecord {
	filtered := leases[:0]
	for _, lease := range leases {
		if lease.SessionKey != sessionKey {
			filtered = append(filtered, lease)
		}
	}
	return filtered
}

func findLeaseForPort(leases []LeaseRecord, probeHost string, port int) *LeaseRecord {
	for index := range leases {
		if leases[index].ProbeHost == probeHost && leases[index].Port == port {
			return &leases[index]
		}
	}
	return nil
}

func findLeaseForSession(leases []LeaseRecord, sessionKey string) *LeaseRecord {
	for index := range leases {
		if leases[index].SessionKey == sessionKey {
			return &leases[index]
		}
	}
	return nil
}
