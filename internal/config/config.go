// Package config loads the small, local-only service configuration.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	maxLeaseDuration = 24 * time.Hour
	maxTTLDuration   = 365 * 24 * time.Hour
	maxRetention     = 365 * 24 * time.Hour
)

// Config contains the runtime settings supported by the first runnable slice.
type Config struct {
	ListenAddress string
	SQLitePath    string
	LeaseDuration time.Duration
	TTLDuration   time.Duration
	Retention     time.Duration
}

// Defaults returns local development defaults. SQLitePath remains empty so a
// caller must explicitly choose where durable state is stored.
func Defaults() Config {
	return Config{
		ListenAddress: "127.0.0.1:8787",
		LeaseDuration: 5 * time.Minute,
		TTLDuration:   24 * time.Hour,
		Retention:     7 * 24 * time.Hour,
	}
}

// Parse reads command-line settings into a typed Config.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	flags := flag.NewFlagSet("crew-messaging", flag.ContinueOnError)
	flags.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "loopback listen address")
	flags.StringVar(&cfg.SQLitePath, "db", "", "SQLite database path (required)")
	flags.DurationVar(&cfg.LeaseDuration, "lease-duration", cfg.LeaseDuration, "maximum lease duration")
	flags.DurationVar(&cfg.TTLDuration, "ttl", cfg.TTLDuration, "maximum message lifetime")
	flags.DurationVar(&cfg.Retention, "retention", cfg.Retention, "terminal record retention")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects unsafe listeners and unbounded local settings.
func (c Config) Validate() error {
	if err := validateLoopbackAddress(c.ListenAddress); err != nil {
		return err
	}
	if strings.TrimSpace(c.SQLitePath) == "" {
		return errors.New("SQLite path is required")
	}
	if err := validateDuration("lease duration", c.LeaseDuration, maxLeaseDuration); err != nil {
		return err
	}
	if err := validateDuration("TTL", c.TTLDuration, maxTTLDuration); err != nil {
		return err
	}
	return validateDuration("retention", c.Retention, maxRetention)
}

func validateLoopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("listen address must be host:port: %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address must use a loopback IP: %q", address)
	}
	if !validPort(port) {
		return fmt.Errorf("listen port must be a number from 1 through 65535: %q", port)
	}
	return nil
}

func validPort(port string) bool {
	if port == "" {
		return false
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value > 0
}

func validateDuration(name string, value, maximum time.Duration) error {
	if value <= 0 || value > maximum {
		return fmt.Errorf("%s must be greater than zero and at most %s", name, maximum)
	}
	return nil
}
