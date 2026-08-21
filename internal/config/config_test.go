package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAcceptsLoopbackConfiguration(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{
		"-db", "state.db",
		"-listen", "[::1]:9000",
		"-lease-duration", "2m",
		"-ttl", "2h",
		"-retention", "24h",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.SQLitePath != "state.db" || cfg.ListenAddress != "[::1]:9000" {
		t.Fatalf("Parse() config = %#v", cfg)
	}
	if cfg.LeaseDuration != 2*time.Minute || cfg.TTLDuration != 2*time.Hour || cfg.Retention != 24*time.Hour {
		t.Fatalf("Parse() durations = %#v", cfg)
	}
}

func TestParseRejectsNonLoopbackAndUnboundedValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "public listener", args: []string{"-db", "state.db", "-listen", "0.0.0.0:8787"}, want: "loopback"},
		{name: "named port", args: []string{"-db", "state.db", "-listen", "127.0.0.1:http"}, want: "listen port"},
		{name: "zero port", args: []string{"-db", "state.db", "-listen", "127.0.0.1:0"}, want: "listen port"},
		{name: "out of range port", args: []string{"-db", "state.db", "-listen", "127.0.0.1:65536"}, want: "listen port"},
		{name: "missing database", args: []string{"-listen", "127.0.0.1:8787"}, want: "SQLite path"},
		{name: "unbounded lease", args: []string{"-db", "state.db", "-lease-duration", "25h"}, want: "lease duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse(%v) error = %v, want %q", tt.args, err, tt.want)
			}
		})
	}
}
