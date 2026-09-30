package devserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (c *ManagerConfig) Validate() error {
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("%w: state_dir is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.SessionRoot) == "" {
		return fmt.Errorf("%w: session_root is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.BindHost) == "" {
		return fmt.Errorf("%w: bind_host is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.ProbeHost) == "" {
		return fmt.Errorf("%w: probe_host is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.PublicHost) == "" {
		return fmt.Errorf("%w: public_host is required", ErrInvalidConfig)
	}
	if c.PortRange.Start <= 0 || c.PortRange.End < c.PortRange.Start {
		return fmt.Errorf("%w: port_range must be positive and ordered", ErrInvalidConfig)
	}
	if c.Timeouts.LockTimeout <= 0 ||
		c.Timeouts.StartupTimeout <= 0 ||
		c.Timeouts.HealthTimeout <= 0 ||
		c.Timeouts.HealthInterval <= 0 ||
		c.Timeouts.ShutdownTimeout <= 0 {
		return fmt.Errorf("%w: timeouts must be positive", ErrInvalidConfig)
	}
	return nil
}

func NormalizeConfig(cfg ManagerConfig) (ManagerConfig, error) {
	var err error
	cfg.StateDir, err = cleanPath("state_dir", cfg.StateDir)
	if err != nil {
		return ManagerConfig{}, err
	}
	cfg.SessionRoot, err = cleanPath("session_root", cfg.SessionRoot)
	if err != nil {
		return ManagerConfig{}, err
	}
	cfg.BindHost = valueOrDefault(cfg.BindHost, DefaultBindHost)
	cfg.ProbeHost = valueOrDefault(cfg.ProbeHost, DefaultProbeHost)
	cfg.PublicHost = valueOrDefault(cfg.PublicHost, PublicHostAuto)
	if err := cfg.Validate(); err != nil {
		return ManagerConfig{}, err
	}
	return cfg, nil
}

func cleanPath(name string, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidConfig, name)
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory for %s: %w", name, err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", name, err)
		}
		path = abs
	}
	return filepath.Clean(path), nil
}

func resolveRepoRoot(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolving current directory: %w", err)
		}
		path = cwd
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving repo root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("repo root does not exist: %s", abs)
		}
		return "", fmt.Errorf("stat repo root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo root is not a directory: %s", abs)
	}
	return filepath.Clean(abs), nil
}

func valueOrDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
