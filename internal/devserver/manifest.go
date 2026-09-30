package devserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type manifestFile struct {
	Project string            `json:"project"`
	Serve   serveManifestFile `json:"serve"`
}

type serveManifestFile struct {
	Command             string            `json:"command"`
	Host                string            `json:"host"`
	BindHost            string            `json:"bindHost"`
	ProbeHost           string            `json:"probeHost"`
	PublicHost          string            `json:"publicHost"`
	PreferredPort       int               `json:"preferredPort"`
	PortRange           *portRangeFile    `json:"portRange"`
	HealthURL           string            `json:"healthUrl"`
	ReadyText           string            `json:"readyText"`
	IdentityHeader      string            `json:"identityHeader"`
	IdentityHeaderValue string            `json:"identityHeaderValue"`
	ReusePolicy         string            `json:"reusePolicy"`
	StartupTimeout      string            `json:"startupTimeout"`
	HealthInterval      string            `json:"healthInterval"`
	Environment         map[string]string `json:"env"`
	FingerprintPaths    []string          `json:"fingerprintPaths"`
}

type portRangeFile struct {
	Start int `json:"start" yaml:"start"`
	End   int `json:"end" yaml:"end"`
}

func FindManifest(repoRoot string, explicitPath string) (string, error) {
	if strings.TrimSpace(explicitPath) != "" {
		path, err := filepath.Abs(explicitPath)
		if err != nil {
			return "", fmt.Errorf("resolving manifest path: %w", err)
		}
		return filepath.Clean(path), nil
	}
	candidates := []string{
		".den-serve.json",
		"den-serve.json",
		".den-playwright.json",
		".playwright-service.json",
		"den-playwright.json",
	}
	for _, name := range candidates {
		path := filepath.Join(repoRoot, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: no manifest found under %s", ErrInvalidManifest, repoRoot)
}

func LoadServeManifest(path string, repoRoot string, cfg ManagerConfig) (*ServeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	var file manifestFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", path, err)
	}
	manifest, err := file.toManifest(path, repoRoot, cfg)
	if err != nil {
		return nil, err
	}
	if err := manifest.validate(); err != nil {
		return nil, err
	}
	return manifest, nil
}

func ManifestHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading manifest for hash: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (f manifestFile) toManifest(path string, repoRoot string, cfg ManagerConfig) (*ServeManifest, error) {
	serve, err := f.Serve.toManifest(cfg)
	if err != nil {
		return nil, err
	}
	serve.Project = strings.TrimSpace(f.Project)
	serve.RepoRoot = filepath.Clean(repoRoot)
	serve.ManifestPath = filepath.Clean(path)
	serve.Target = DefaultTarget
	return &serve, nil
}

func (f serveManifestFile) toManifest(cfg ManagerConfig) (ServeManifest, error) {
	startupTimeout := cfg.Timeouts.StartupTimeout
	if strings.TrimSpace(f.StartupTimeout) != "" {
		parsed, err := time.ParseDuration(f.StartupTimeout)
		if err != nil {
			return ServeManifest{}, fmt.Errorf("%w: parsing serve.startupTimeout: %w", ErrInvalidManifest, err)
		}
		startupTimeout = parsed
	}
	healthInterval := cfg.Timeouts.HealthInterval
	if strings.TrimSpace(f.HealthInterval) != "" {
		parsed, err := time.ParseDuration(f.HealthInterval)
		if err != nil {
			return ServeManifest{}, fmt.Errorf("%w: parsing serve.healthInterval: %w", ErrInvalidManifest, err)
		}
		healthInterval = parsed
	}
	portRange := (*PortRange)(nil)
	if f.PortRange != nil {
		portRange = &PortRange{Start: f.PortRange.Start, End: f.PortRange.End}
	}
	bindHost := valueOrDefault(f.BindHost, valueOrDefault(f.Host, cfg.BindHost))
	return ServeManifest{
		Command:             strings.TrimSpace(f.Command),
		BindHost:            bindHost,
		ProbeHost:           valueOrDefault(f.ProbeHost, cfg.ProbeHost),
		PublicHost:          valueOrDefault(f.PublicHost, cfg.PublicHost),
		PreferredPort:       f.PreferredPort,
		PortRange:           portRange,
		HealthPath:          valueOrDefault(f.HealthURL, "/"),
		ReadyText:           f.ReadyText,
		IdentityHeader:      f.IdentityHeader,
		IdentityHeaderValue: strings.TrimSpace(f.IdentityHeaderValue),
		ReusePolicy:         ReusePolicy(valueOrDefault(f.ReusePolicy, string(ReusePolicyBrokerOwned))),
		StartupTimeout:      startupTimeout,
		HealthInterval:      healthInterval,
		Environment:         copyMap(f.Environment),
		FingerprintPaths:    append([]string(nil), f.FingerprintPaths...),
	}, nil
}

func (m *ServeManifest) validate() error {
	if strings.TrimSpace(m.Project) == "" {
		return fmt.Errorf("%w: project is required", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.RepoRoot) == "" {
		return fmt.Errorf("%w: repo root is required", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.Command) == "" {
		return fmt.Errorf("%w: serve.command is required", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.BindHost) == "" {
		return fmt.Errorf("%w: serve bind host is required", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.ProbeHost) == "" {
		return fmt.Errorf("%w: serve probe host is required", ErrInvalidManifest)
	}
	if m.PreferredPort < 0 {
		return fmt.Errorf("%w: serve.preferredPort cannot be negative", ErrInvalidManifest)
	}
	if m.PortRange != nil && (m.PortRange.Start <= 0 || m.PortRange.End < m.PortRange.Start) {
		return fmt.Errorf("%w: serve.portRange must be positive and ordered", ErrInvalidManifest)
	}
	if !m.ReusePolicy.IsValid() {
		return fmt.Errorf("%w: serve.reusePolicy is invalid", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.ReadyText) == "" && strings.TrimSpace(m.IdentityHeader) == "" {
		return fmt.Errorf("%w: serve.readyText or serve.identityHeader is required", ErrInvalidManifest)
	}
	if strings.TrimSpace(m.IdentityHeaderValue) != "" && strings.TrimSpace(m.IdentityHeader) == "" {
		return fmt.Errorf("%w: serve.identityHeaderValue requires serve.identityHeader", ErrInvalidManifest)
	}
	if m.StartupTimeout <= 0 || m.HealthInterval <= 0 {
		return fmt.Errorf("%w: serve timeouts must be positive", ErrInvalidManifest)
	}
	for _, path := range m.FingerprintPaths {
		clean := filepath.Clean(strings.TrimSpace(path))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: serve.fingerprintPaths entries must be relative paths below the repo root", ErrInvalidManifest)
		}
	}
	return nil
}

func (m *ServeManifest) EffectivePortRange(cfg ManagerConfig) PortRange {
	if m.PortRange != nil {
		return *m.PortRange
	}
	return cfg.PortRange
}

func copyMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	target := make(map[string]string, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}
