// Package session owns durable playtest sessions independently of agent clients.
package session

import (
	"context"
	"encoding/json"
	"time"
)

type Backend interface {
	Acquire(context.Context, int, int, int, int) (map[string]any, error)
	Observe(context.Context, string) (map[string]any, error)
	Input(context.Context, string, []map[string]any) (map[string]any, error)
	Cancel(context.Context, string) (map[string]any, error)
	Status(context.Context, string) (map[string]any, error)
	Release(context.Context, string) (map[string]any, error)
}

type Profile struct {
	PresentationObservations bool              `json:"presentation_observations,omitempty"`
	InteractionQueries       bool              `json:"interaction_queries,omitempty"`
	Backend                  string            `json:"backend,omitempty"`
	Environment              string            `json:"environment,omitempty"`
	ID                       string            `json:"id"`
	Description              string            `json:"description"`
	URL                      string            `json:"url,omitempty"`
	Host                     *HostSpec         `json:"host,omitempty"`
	WindowTitle              string            `json:"window_title,omitempty"`
	Controls                 map[string]string `json:"controls"`
	Reset                    string            `json:"reset"`
}

// HostSpec asks the service to start a private product host for each session
// from the repository's ordinary .den-serve.json manifest. Every session gets
// its own world; stop and recover end that host. URL is then derived per session.
type HostSpec struct {
	Repo     string `json:"repo"`
	Manifest string `json:"manifest,omitempty"`
	// Path is appended to the host's loopback origin; default "/".
	Path string `json:"path,omitempty"`
}

// HostReleaser stops a session-owned product host after the backend is
// released. host is the identity the launcher reported in launch["host"]; it
// is authoritative even if the repository's manifest has since changed. A nil
// host means none was started.
type HostReleaser interface {
	ReleaseHost(ctx context.Context, sessionID string, p Profile, host map[string]any) (map[string]any, error)
}

// ProfileBackend selects an execution environment without changing session ownership.
type ProfileBackend interface {
	SelectProfile(Profile) error
}

type BrowserBackend interface {
	Browser(context.Context, string, json.RawMessage) (map[string]any, error)
}

type Launcher interface {
	Launch(context.Context, string, Profile) (map[string]any, error)
}

type Request struct {
	Op        string           `json:"op"`
	SessionID string           `json:"session_id,omitempty"`
	Game      string           `json:"game,omitempty"`
	Source    string           `json:"source,omitempty"`
	ScriptID  string           `json:"script_id,omitempty"`
	Data      json.RawMessage  `json:"data,omitempty"`
	Steps     []map[string]any `json:"steps,omitempty"`
	BudgetMS  int              `json:"budget_ms,omitempty"`
}

type Session struct {
	Profile         *Profile       `json:"profile,omitempty"`
	SlotID          string         `json:"slot_id,omitempty"`
	ID              string         `json:"id"`
	Game            string         `json:"game"`
	Phase           string         `json:"phase"`
	CreatedAt       time.Time      `json:"created_at"`
	LastError       string         `json:"last_error,omitempty"`
	Launch          map[string]any `json:"launch,omitempty"`
	ScriptID        string         `json:"script_id,omitempty"`
	PreviousSession string         `json:"previous_session,omitempty"`
}

type Script struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	Phase      string     `json:"phase"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Result     any        `json:"result,omitempty"`
	SourcePath string     `json:"source_path"`
	EventsPath string     `json:"events_path"`
	Checkpoint any        `json:"checkpoint,omitempty"`
	Calls      int        `json:"calls"`
}
