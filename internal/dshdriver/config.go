package dshdriver

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"
	"time"
)

// Config is crew-dsh's runtime configuration.
type Config struct {
	FabricURL     string
	AdapterID     string
	InstanceID    string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	ClaimDuration time.Duration
	// ControlListen is the loopback address of the send/who API.
	ControlListen string
	// DSHUnit is the systemd user unit running `dsh web`; its journal holds
	// the launch token.
	DSHUnit string
	// DSHURL overrides the web host URL logged with the token.
	DSHURL string
	// ReconcileWait is how long the driver waits after an ambiguous prompt
	// before it looks for the request in the session. The host takes an item
	// out of the inbox about 0.1s before it writes the user message, and a
	// repeat in between is not deduplicated.
	ReconcileWait time.Duration
	// Recent limits discovery to sessions updated within this window.
	Recent time.Duration
}

// Defaults returns ordinary local values.
func Defaults() Config {
	return Config{
		FabricURL:     "http://127.0.0.1:8787",
		AdapterID:     "crew-dsh",
		InstanceID:    "crew-dsh-local",
		LeaseDuration: 5 * time.Minute,
		PollInterval:  2 * time.Second,
		ClaimDuration: 45 * time.Second,
		ControlListen: "127.0.0.1:8793",
		DSHUnit:       "dsh-web.service",
		ReconcileWait: time.Second,
		Recent:        14 * 24 * time.Hour,
	}
}

// Parse reads command flags.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	flags := flag.NewFlagSet("crew-dsh", flag.ContinueOnError)
	flags.StringVar(&cfg.FabricURL, "fabric-url", cfg.FabricURL, "crew-messaging base URL")
	flags.StringVar(&cfg.AdapterID, "adapter-id", cfg.AdapterID, "stable fabric adapter ID")
	flags.StringVar(&cfg.InstanceID, "instance-id", cfg.InstanceID, "stable fabric adapter instance ID")
	flags.DurationVar(&cfg.LeaseDuration, "lease-duration", cfg.LeaseDuration, "fabric adapter lease duration")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "delivery pass interval")
	flags.DurationVar(&cfg.ClaimDuration, "claim-duration", cfg.ClaimDuration, "fabric delivery claim duration")
	flags.StringVar(&cfg.ControlListen, "control-listen", cfg.ControlListen, "loopback address of the send/who API")
	flags.StringVar(&cfg.DSHUnit, "dsh-unit", cfg.DSHUnit, "systemd user unit running dsh web (its journal holds the launch token)")
	flags.StringVar(&cfg.DSHURL, "dsh-url", cfg.DSHURL, "web host URL (default: the one dsh web logged)")
	flags.DurationVar(&cfg.ReconcileWait, "reconcile-wait", cfg.ReconcileWait, "wait after an ambiguous prompt before checking the session for it")
	flags.DurationVar(&cfg.Recent, "recent", cfg.Recent, "give addresses to sessions updated within this window")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.FabricURL) == "" {
		return errors.New("fabric URL is required")
	}
	if strings.TrimSpace(c.AdapterID) == "" || strings.TrimSpace(c.InstanceID) == "" {
		return errors.New("adapter ID and instance ID are required")
	}
	if c.LeaseDuration <= 0 || c.PollInterval <= 0 || c.ClaimDuration <= 0 || c.Recent <= 0 {
		return errors.New("lease duration, poll interval, claim duration and recent must be greater than zero")
	}
	if c.ReconcileWait < 200*time.Millisecond {
		return errors.New("reconcile wait must be at least 200ms (the host's inbox-to-history window is about 100ms)")
	}
	host, _, err := net.SplitHostPort(c.ControlListen)
	if err != nil {
		return fmt.Errorf("invalid control listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("control listen address must be a loopback address")
	}
	return nil
}
