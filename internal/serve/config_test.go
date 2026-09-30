package serve

import (
	"os"
	"path/filepath"
	"testing"

	"crew-services/internal/devserver"
)

func TestLoadConfigDefaultsToLanFacingBindAndLoopbackProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
state_dir: "~/.cache/den-serve/state"
session_root: "~/.cache/den-serve/sessions"
public_host: "auto"
port_range:
  start: 37300
  end: 37450
timeouts:
  lock_timeout: "10s"
  startup_timeout: "45s"
  health_timeout: "2s"
  health_interval: "250ms"
  shutdown_timeout: "5s"
`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatalf("LoadConfigFromPath() error = %v", err)
	}
	if cfg.Manager.BindHost != devserver.DefaultBindHost {
		t.Fatalf("BindHost = %q, want %q", cfg.Manager.BindHost, devserver.DefaultBindHost)
	}
	if cfg.Manager.ProbeHost != devserver.DefaultProbeHost {
		t.Fatalf("ProbeHost = %q, want %q", cfg.Manager.ProbeHost, devserver.DefaultProbeHost)
	}
	if cfg.StatusPage.BindHost != devserver.DefaultBindHost || cfg.StatusPage.Port != 37299 {
		t.Fatalf("StatusPage = %#v, want 0.0.0.0:37299", cfg.StatusPage)
	}
}

func TestDefaultConfigSupportsOneCommandWorkflow(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Manager.BindHost != devserver.DefaultBindHost {
		t.Fatalf("BindHost = %q, want %q", cfg.Manager.BindHost, devserver.DefaultBindHost)
	}
	if cfg.Manager.ProbeHost != devserver.DefaultProbeHost {
		t.Fatalf("ProbeHost = %q, want %q", cfg.Manager.ProbeHost, devserver.DefaultProbeHost)
	}
	if cfg.Manager.PortRange.Start != 30300 || cfg.Manager.PortRange.End != 30450 {
		t.Fatalf("PortRange = %#v, want 30300-30450 below the ephemeral range", cfg.Manager.PortRange)
	}
	if got := cfg.StatusPage.Address(); got != "0.0.0.0:37299" {
		t.Fatalf("StatusPage.Address() = %q, want 0.0.0.0:37299", got)
	}
}
