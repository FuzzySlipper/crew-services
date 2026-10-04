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
	if cfg.Manager.Retention != devserver.DefaultRetention {
		t.Fatalf("Retention = %s, want the default %s", cfg.Manager.Retention, devserver.DefaultRetention)
	}
}

func TestConfigRetentionIsAPositiveDuration(t *testing.T) {
	base := "state_dir: /tmp/s\nsession_root: /tmp/r\nport_range: {start: 37300, end: 37450}\n" +
		"timeouts: {lock_timeout: 1s, startup_timeout: 1s, health_timeout: 1s, health_interval: 1s, shutdown_timeout: 1s}\n"
	for retention, valid := range map[string]bool{`"168h"`: true, `"soon"`: false, `"-1h"`: false} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(base+"retention: "+retention+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigFromPath(path)
		if (err == nil) != valid {
			t.Fatalf("retention %s: err = %v, want valid=%v", retention, err, valid)
		}
		if valid && cfg.Manager.Retention.Hours() != 168 {
			t.Fatalf("retention %s loaded as %s", retention, cfg.Manager.Retention)
		}
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
