// Package codexadapter is the Codex edge of the crew messaging fabric. Its
// driver delivers fabric messages into threads of the shared Codex app-server
// daemon (the one the TUI attaches to) and never owns a Codex process; its
// stdio client also runs crew-review's ephemeral reviewer workers.
package codexadapter

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"
	"time"
)

const defaultAdapterID = "crew-codex"

// Config is crew-codex's runtime configuration.
type Config struct {
	FabricURL     string
	AdapterID     string
	InstanceID    string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	ClaimDuration time.Duration
	// ControlListen is the loopback address of the hook API.
	ControlListen string
	// StatePath remembers which threads have session addresses, so mail can
	// still wake a thread after the daemon has unloaded it. Empty keeps it in
	// memory only.
	StatePath string
	// DaemonSocket is the shared Codex daemon's control socket.
	DaemonSocket string
	// Mappings pin named addresses to threads, in addition to the per-thread
	// s/... session addresses.
	Mappings []Mapping
}

// Mapping binds one named fabric address to one Codex thread.
type Mapping struct {
	Address  string
	ThreadID string
}

// Defaults returns ordinary local values.
func Defaults() Config {
	return Config{
		FabricURL:     "http://127.0.0.1:8787",
		AdapterID:     defaultAdapterID,
		InstanceID:    "crew-codex-local",
		LeaseDuration: 5 * time.Minute,
		PollInterval:  2 * time.Second,
		ClaimDuration: 45 * time.Second,
		ControlListen: "127.0.0.1:8788",
		DaemonSocket:  DefaultDaemonSocket(),
	}
}

// Parse reads command flags. -address may be repeated as ADDRESS=THREAD_ID.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	var mappings mappingFlags
	flags := flag.NewFlagSet("crew-codex", flag.ContinueOnError)
	flags.StringVar(&cfg.FabricURL, "fabric-url", cfg.FabricURL, "crew-messaging base URL")
	flags.StringVar(&cfg.AdapterID, "adapter-id", cfg.AdapterID, "stable fabric adapter ID")
	flags.StringVar(&cfg.InstanceID, "instance-id", cfg.InstanceID, "stable fabric adapter instance ID")
	flags.DurationVar(&cfg.LeaseDuration, "lease-duration", cfg.LeaseDuration, "fabric adapter lease duration")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "delivery pass interval")
	flags.DurationVar(&cfg.ClaimDuration, "claim-duration", cfg.ClaimDuration, "fabric delivery claim duration")
	flags.StringVar(&cfg.ControlListen, "control-listen", cfg.ControlListen, "loopback address of the hook API")
	flags.StringVar(&cfg.StatePath, "state", cfg.StatePath, "driver state JSON path (threads with session addresses)")
	flags.StringVar(&cfg.DaemonSocket, "codex-socket", cfg.DaemonSocket, "shared Codex daemon control socket")
	flags.Var(&mappings, "address", "pin a named ADDRESS=THREAD_ID; repeatable")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.Mappings = append([]Mapping(nil), mappings...)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.FabricURL) == "" {
		return errors.New("fabric URL is required")
	}
	if strings.TrimSpace(c.AdapterID) == "" {
		return errors.New("adapter ID is required")
	}
	if strings.TrimSpace(c.InstanceID) == "" {
		return errors.New("instance ID is required")
	}
	if strings.TrimSpace(c.DaemonSocket) == "" {
		return errors.New("Codex daemon socket is required")
	}
	if c.LeaseDuration <= 0 {
		return errors.New("lease duration must be greater than zero")
	}
	if c.PollInterval <= 0 {
		return errors.New("poll interval must be greater than zero")
	}
	if c.ClaimDuration <= 0 {
		return errors.New("claim duration must be greater than zero")
	}
	host, _, err := net.SplitHostPort(c.ControlListen)
	if err != nil {
		return fmt.Errorf("invalid control listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("control listen address must be a loopback address")
	}
	addresses := make(map[string]struct{}, len(c.Mappings))
	threads := make(map[string]struct{}, len(c.Mappings))
	for _, mapping := range c.Mappings {
		if strings.TrimSpace(mapping.Address) == "" || strings.TrimSpace(mapping.ThreadID) == "" {
			return errors.New("address mappings require ADDRESS=THREAD_ID")
		}
		if _, found := addresses[mapping.Address]; found {
			return fmt.Errorf("address %q was mapped more than once", mapping.Address)
		}
		if _, found := threads[mapping.ThreadID]; found {
			return fmt.Errorf("Codex thread %q was mapped more than once", mapping.ThreadID)
		}
		addresses[mapping.Address] = struct{}{}
		threads[mapping.ThreadID] = struct{}{}
	}
	return nil
}

type mappingFlags []Mapping

func (m *mappingFlags) String() string { return "" }
func (m *mappingFlags) Set(value string) error {
	address, threadID, ok := strings.Cut(value, "=")
	address, threadID = strings.TrimSpace(address), strings.TrimSpace(threadID)
	if !ok || address == "" || threadID == "" {
		return errors.New("address must have the form ADDRESS=THREAD_ID")
	}
	*m = append(*m, Mapping{Address: address, ThreadID: threadID})
	return nil
}
