package serve

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"crew-services/internal/devserver"

	"gopkg.in/yaml.v3"
)

const defaultStatusPagePort = 37299

type Config struct {
	Manager    devserver.ManagerConfig
	StatusPage StatusPageConfig
}

type StatusPageConfig struct {
	BindHost string
	Port     int
}

func (c StatusPageConfig) Address() string {
	return net.JoinHostPort(c.BindHost, strconv.Itoa(c.Port))
}

func (c StatusPageConfig) AddressForHost(host string) string {
	return net.JoinHostPort(host, strconv.Itoa(c.Port))
}

func DefaultConfig() (Config, error) {
	manager, err := devserver.NormalizeConfig(devserver.ManagerConfig{
		StateDir:    "~/.cache/den-serve/state",
		SessionRoot: "~/.cache/den-serve/sessions",
		BindHost:    devserver.DefaultBindHost,
		ProbeHost:   devserver.DefaultProbeHost,
		PublicHost:  devserver.PublicHostAuto,
		// Below Linux's default ephemeral range (32768-60999): a managed port
		// there can be taken by an outgoing connection while a host builds.
		PortRange: devserver.PortRange{
			Start: 30300,
			End:   30450,
		},
		Timeouts: devserver.TimeoutConfig{
			LockTimeout:     10 * time.Second,
			StartupTimeout:  45 * time.Second,
			HealthTimeout:   2 * time.Second,
			HealthInterval:  250 * time.Millisecond,
			ShutdownTimeout: 5 * time.Second,
		},
	})
	if err != nil {
		return Config{}, err
	}
	return Config{
		Manager: manager,
		StatusPage: StatusPageConfig{
			BindHost: devserver.DefaultBindHost,
			Port:     defaultStatusPagePort,
		},
	}, nil
}

type configFile struct {
	StateDir    string         `yaml:"state_dir"`
	SessionRoot string         `yaml:"session_root"`
	BindHost    string         `yaml:"bind_host"`
	ProbeHost   string         `yaml:"probe_host"`
	PublicHost  string         `yaml:"public_host"`
	PortRange   portRangeFile  `yaml:"port_range"`
	Timeouts    timeoutFile    `yaml:"timeouts"`
	StatusPage  statusPageFile `yaml:"status_page"`
}

type statusPageFile struct {
	BindHost string `yaml:"bind_host"`
	Port     int    `yaml:"port"`
}

type portRangeFile struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

type timeoutFile struct {
	LockTimeout     string `yaml:"lock_timeout"`
	StartupTimeout  string `yaml:"startup_timeout"`
	HealthTimeout   string `yaml:"health_timeout"`
	HealthInterval  string `yaml:"health_interval"`
	ShutdownTimeout string `yaml:"shutdown_timeout"`
}

func LoadConfigFromPath(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading den-serve config %s: %w", path, err)
	}
	var file configFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return Config{}, fmt.Errorf("parsing den-serve config %s: %w", path, err)
	}
	manager, err := file.toManagerConfig()
	if err != nil {
		return Config{}, err
	}
	manager, err = devserver.NormalizeConfig(manager)
	if err != nil {
		return Config{}, err
	}
	statusPage, err := file.StatusPage.toConfig()
	if err != nil {
		return Config{}, err
	}
	return Config{Manager: manager, StatusPage: statusPage}, nil
}

func (f configFile) toManagerConfig() (devserver.ManagerConfig, error) {
	timeouts, err := f.Timeouts.toConfig()
	if err != nil {
		return devserver.ManagerConfig{}, err
	}
	return devserver.ManagerConfig{
		StateDir:    f.StateDir,
		SessionRoot: f.SessionRoot,
		BindHost:    valueOrDefault(f.BindHost, devserver.DefaultBindHost),
		ProbeHost:   valueOrDefault(f.ProbeHost, devserver.DefaultProbeHost),
		PublicHost:  valueOrDefault(f.PublicHost, devserver.PublicHostAuto),
		PortRange: devserver.PortRange{
			Start: f.PortRange.Start,
			End:   f.PortRange.End,
		},
		Timeouts: timeouts,
	}, nil
}

func (f statusPageFile) toConfig() (StatusPageConfig, error) {
	port := f.Port
	if port == 0 {
		port = defaultStatusPagePort
	}
	if port < 1 || port > 65535 {
		return StatusPageConfig{}, fmt.Errorf("status_page.port must be between 1 and 65535")
	}
	return StatusPageConfig{
		BindHost: valueOrDefault(f.BindHost, devserver.DefaultBindHost),
		Port:     port,
	}, nil
}

func (f timeoutFile) toConfig() (devserver.TimeoutConfig, error) {
	lockTimeout, err := parseRequiredDuration("timeouts.lock_timeout", f.LockTimeout)
	if err != nil {
		return devserver.TimeoutConfig{}, err
	}
	startupTimeout, err := parseRequiredDuration("timeouts.startup_timeout", f.StartupTimeout)
	if err != nil {
		return devserver.TimeoutConfig{}, err
	}
	healthTimeout, err := parseRequiredDuration("timeouts.health_timeout", f.HealthTimeout)
	if err != nil {
		return devserver.TimeoutConfig{}, err
	}
	healthInterval, err := parseRequiredDuration("timeouts.health_interval", f.HealthInterval)
	if err != nil {
		return devserver.TimeoutConfig{}, err
	}
	shutdownTimeout, err := parseRequiredDuration("timeouts.shutdown_timeout", f.ShutdownTimeout)
	if err != nil {
		return devserver.TimeoutConfig{}, err
	}
	return devserver.TimeoutConfig{
		LockTimeout:     lockTimeout,
		StartupTimeout:  startupTimeout,
		HealthTimeout:   healthTimeout,
		HealthInterval:  healthInterval,
		ShutdownTimeout: shutdownTimeout,
	}, nil
}

func parseRequiredDuration(name string, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return parsed, nil
}

func valueOrDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
