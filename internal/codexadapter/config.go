// Package codexadapter contains the Codex-specific edge adapter. It deliberately
// depends on the runtime-neutral HTTP boundary rather than on the hub's stores.
package codexadapter

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

const defaultAdapterID = "crew-codex"

// Config is the intentionally small runtime configuration for crew-codex.
// Mappings are explicit because discovering every thread would make an
// observational adapter unexpectedly adopt native runtime state.
type Config struct {
	FabricURL     string
	AdapterID     string
	InstanceID    string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	ClaimDuration time.Duration
	Command       string
	CommandArgs   []string
	Mappings      []Mapping
}

// Mapping binds one public fabric address to one existing Codex thread.
type Mapping struct {
	Address  string
	ThreadID string
}

// Defaults returns ordinary local development values. The child is the current
// Codex CLI App Server command and deliberately has no version constraint.
func Defaults() Config {
	return Config{
		FabricURL:     "http://127.0.0.1:8787",
		AdapterID:     defaultAdapterID,
		InstanceID:    "crew-codex-local",
		LeaseDuration: 5 * time.Minute,
		PollInterval:  2 * time.Second,
		ClaimDuration: 45 * time.Second,
		Command:       "codex",
		CommandArgs:   []string{"app-server", "--stdio"},
	}
}

// Parse reads command flags. -address may be repeated as ADDRESS=THREAD_ID.
func Parse(args []string) (Config, error) {
	cfg := Defaults()
	var mappings mappingFlags
	var commandArgs stringList
	flags := flag.NewFlagSet("crew-codex", flag.ContinueOnError)
	flags.StringVar(&cfg.FabricURL, "fabric-url", cfg.FabricURL, "crew-services base URL")
	flags.StringVar(&cfg.AdapterID, "adapter-id", cfg.AdapterID, "stable fabric adapter ID")
	flags.StringVar(&cfg.InstanceID, "instance-id", cfg.InstanceID, "stable fabric adapter instance ID")
	flags.DurationVar(&cfg.LeaseDuration, "lease-duration", cfg.LeaseDuration, "fabric adapter lease duration")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "canonical thread reconciliation interval")
	flags.DurationVar(&cfg.ClaimDuration, "claim-duration", cfg.ClaimDuration, "fabric delivery claim duration")
	flags.StringVar(&cfg.Command, "codex-command", cfg.Command, "Codex App Server executable")
	flags.Var(&commandArgs, "codex-arg", "override App Server arguments; repeatable")
	flags.Var(&mappings, "address", "explicit ADDRESS=THREAD_ID mapping; repeatable")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if len(commandArgs) > 0 {
		cfg.CommandArgs = append([]string(nil), commandArgs...)
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
	if strings.TrimSpace(c.Command) == "" {
		return errors.New("Codex command is required")
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
	if len(c.Mappings) == 0 {
		return errors.New("at least one -address ADDRESS=THREAD_ID mapping is required")
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

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, " ") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("Codex argument must not be empty")
	}
	*s = append(*s, value)
	return nil
}
