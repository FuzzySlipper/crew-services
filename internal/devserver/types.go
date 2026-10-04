package devserver

import (
	"errors"
	"time"
)

const (
	DefaultBindHost     = "0.0.0.0"
	DefaultProbeHost    = "127.0.0.1"
	PublicHostAuto      = "auto"
	DefaultTarget       = "default"
	SessionSchemaV0     = "den-serve-session/v0"
	SessionSchemaV1     = "den-serve-session/v1"
	FingerprintSchemaV1 = "den-serve-launch-fingerprint/v1"
	DefaultManifestName = ".den-serve.json"
)

type ManagerConfig struct {
	StateDir    string
	SessionRoot string
	BindHost    string
	ProbeHost   string
	PublicHost  string
	PortRange   PortRange
	Timeouts    TimeoutConfig
	// Retention is how long an ended session's directory and logs are kept.
	Retention time.Duration
}

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type TimeoutConfig struct {
	LockTimeout     time.Duration
	StartupTimeout  time.Duration
	HealthTimeout   time.Duration
	HealthInterval  time.Duration
	ShutdownTimeout time.Duration
}

type ServeManifest struct {
	Project             string
	RepoRoot            string
	ManifestPath        string
	Target              string
	Command             string
	BindHost            string
	ProbeHost           string
	PublicHost          string
	PreferredPort       int
	PortRange           *PortRange
	HealthPath          string
	ReadyText           string
	IdentityHeader      string
	IdentityHeaderValue string
	ReusePolicy         ReusePolicy
	StartupTimeout      time.Duration
	HealthInterval      time.Duration
	Environment         map[string]string
	FingerprintPaths    []string
	// InstanceArgs is appended to Command for an instance session, so a host
	// that locks per project can run beside others of the same checkout.
	InstanceArgs string
	// KeepArgs is appended to Command when the caller asks the host to stay
	// up without activity.
	KeepArgs string
	// StopCommand asks the host to stop itself before its process group is
	// signalled; it is rendered at launch and kept with the session.
	StopCommand string
}

// ExpectedIdentityHeaderValue returns the configured host identity value. The
// project remains the backwards-compatible default for manifests whose server
// identifies itself with its Den project ID.
func (m ServeManifest) ExpectedIdentityHeaderValue() string {
	if m.IdentityHeaderValue != "" {
		return m.IdentityHeaderValue
	}
	return m.Project
}

type ReusePolicy string

const (
	ReusePolicyBrokerOwned ReusePolicy = "broker_owned"
	ReusePolicyExplicit    ReusePolicy = "explicit"
	ReusePolicyNever       ReusePolicy = "never"
)

func (p ReusePolicy) IsValid() bool {
	switch p {
	case ReusePolicyBrokerOwned, ReusePolicyExplicit, ReusePolicyNever:
		return true
	}
	return false
}

type UpOptions struct {
	Project            string
	RepoRoot           string
	ManifestPath       string
	PublicHostOverride string
	// Instance runs an additional, separately owned host of the same project.
	Instance string
	// Label names the host's owner for the manifest's {label}; it defaults to
	// den-serve:<instance>.
	Label string
	// Keep appends the manifest's keepArgs.
	Keep bool
}

type StatusOptions struct {
	Project  string
	RepoRoot string
	Instance string
}

type StopOptions struct {
	Project  string
	RepoRoot string
	Instance string
}

type UpResult struct {
	Session   SessionState
	Started   bool
	Reused    bool
	Restarted bool
}

type StopResult struct {
	Session SessionState
	Stopped bool
	Message string
}

type SessionState struct {
	SchemaVersion      string            `json:"schema_version"`
	SessionID          string            `json:"session_id"`
	SessionKey         string            `json:"session_key"`
	Project            string            `json:"project"`
	Instance           string            `json:"instance,omitempty"`
	Label              string            `json:"label,omitempty"`
	Keep               bool              `json:"keep,omitempty"`
	Target             string            `json:"target"`
	RepoRoot           string            `json:"repo_root"`
	ManifestPath       string            `json:"manifest_path"`
	ManifestHash       string            `json:"manifest_hash"`
	LaunchFingerprint  LaunchFingerprint `json:"launch_fingerprint"`
	CurrentFingerprint LaunchFingerprint `json:"current_fingerprint"`
	Stale              bool              `json:"stale"`
	StaleReason        string            `json:"stale_reason,omitempty"`
	FingerprintError   string            `json:"fingerprint_error,omitempty"`
	Command            string            `json:"command"`
	StopCommand        string            `json:"stop_command,omitempty"`
	BindHost           string            `json:"bind_host"`
	ProbeHost          string            `json:"probe_host"`
	PublicHost         string            `json:"public_host,omitempty"`
	PublicHostOverride string            `json:"public_host_override,omitempty"`
	Port               int               `json:"port"`
	LocalURL           string            `json:"local_url"`
	LANURL             string            `json:"lan_url,omitempty"`
	HealthURL          string            `json:"health_url"`
	PID                int               `json:"pid,omitempty"`
	Ownership          string            `json:"ownership"`
	ReuseSource        string            `json:"reuse_source"`
	Status             string            `json:"status"`
	Health             HealthResult      `json:"health"`
	StartedAt          time.Time         `json:"started_at"`
	LastCheckedAt      time.Time         `json:"last_checked_at"`
	StdoutLog          string            `json:"stdout_log"`
	StderrLog          string            `json:"stderr_log"`
	StatePath          string            `json:"state_path"`
	SessionDir         string            `json:"session_dir"`
}

type LaunchFingerprint struct {
	SchemaVersion        string `json:"schema_version"`
	Value                string `json:"value"`
	RepoHead             string `json:"repo_head,omitempty"`
	RepoDirty            bool   `json:"repo_dirty"`
	LaunchDefinitionHash string `json:"launch_definition_hash"`
	SourceHash           string `json:"source_hash"`
}

type HealthResult struct {
	URL            string `json:"url"`
	StatusCode     int    `json:"status_code"`
	ReadyTextFound bool   `json:"ready_text_found"`
	HeaderMatched  bool   `json:"header_matched"`
	Matched        bool   `json:"matched"`
	Error          string `json:"error,omitempty"`
}

var (
	ErrInvalidConfig   = errors.New("invalid devserver broker config")
	ErrInvalidManifest = errors.New("invalid devserver manifest")
	ErrNoPortAvailable = errors.New("no devserver broker port available")
	ErrSessionNotFound = errors.New("den-serve session not found")
	ErrSessionStale    = errors.New("den-serve session is stale")
)
