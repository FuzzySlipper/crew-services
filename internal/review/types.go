// Package review implements the bounded, runtime-neutral Den review job runner.
// It is deliberately separate from the crew messaging fabric.
package review

import (
	"context"
	"errors"
	"time"
)

type State string

const (
	Queued     State = "queued"
	Running    State = "running"
	Finalizing State = "finalizing"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Cancelled  State = "cancelled"
	Stale      State = "stale"
)

func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled || s == Stale }

var (
	ErrConflict = errors.New("review job idempotency conflict")
	ErrNotFound = errors.New("review job not found")
	ErrTooLate  = errors.New("review job is already finalizing or terminal")
	// Adapter implementations classify Den's authoritative terminal responses.
	ErrStaleRound   = errors.New("Den review round is stale")
	ErrDenConflict  = errors.New("Den review finalization conflicts")
	ErrCapacity     = errors.New("reviewer runtime is at capacity")
	ErrAffinityBusy = errors.New("retained reviewer is busy")
)

// Key is Den's logical review identity. Source evidence is intentionally not part of it.
type Key struct {
	ProjectID     string `json:"project_id"`
	TaskID        int64  `json:"task_id"`
	ReviewRoundID int64  `json:"review_round_id"`
	CorrelationID string `json:"correlation_id"`
}

func (k Key) valid() bool {
	return k.ProjectID != "" && k.TaskID > 0 && k.ReviewRoundID > 0 && k.CorrelationID != ""
}
func (k Key) Task() TaskKey { return TaskKey{ProjectID: k.ProjectID, TaskID: k.TaskID} }

type TaskKey struct {
	ProjectID string
	TaskID    int64
}

type GateEvidence struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	Status     string `json:"status,omitempty"`
	Handle     string `json:"handle,omitempty"`
}

type Admission struct {
	IdempotencyKey string       `json:"idempotency_key"`
	Key            Key          `json:"key"`
	Reviewer       string       `json:"reviewer"`
	Workspace      string       `json:"workspace,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	Gate           GateEvidence `json:"gate,omitempty"`
	PacketHandle   string       `json:"packet_handle,omitempty"`
}

type Completion struct {
	Verdict          string              `json:"verdict"`
	Notes            string              `json:"notes,omitempty"`
	Evidence         string              `json:"evidence,omitempty"`
	NewFindings      []NewFinding        `json:"new_findings,omitempty"`
	PriorResolutions []FindingResolution `json:"prior_finding_resolutions,omitempty"`
}

// These are the compact controller-owned shapes Den currently accepts. The
// runtime supplies content only; job identity comes from Finalization.Key.
type FindingResolution struct {
	FindingID        int64  `json:"finding_id"`
	Status           string `json:"status"`
	VerificationNote string `json:"verification_note"`
}
type NewFinding struct {
	Category       string   `json:"category"`
	Summary        string   `json:"summary"`
	Notes          string   `json:"notes,omitempty"`
	FileReferences []string `json:"file_references,omitempty"`
	TestCommands   []string `json:"test_commands,omitempty"`
}

func (c Completion) valid() bool {
	return c.Verdict == "looks_good" || c.Verdict == "changes_requested"
}
func (c Completion) Valid() bool { return c.valid() }

type Finalization struct {
	Key        Key        `json:"key"`
	Reviewer   string     `json:"reviewer"`
	Completion Completion `json:"completion"`
}

type Receipt struct {
	Schema              string `json:"schema,omitempty"`
	SchemaVersion       int    `json:"schema_version,omitempty"`
	FinalizationID      int64  `json:"finalization_id,omitempty"`
	ProjectID           string `json:"project_id,omitempty"`
	TaskID              int64  `json:"task_id,omitempty"`
	ReviewRoundID       int64  `json:"review_round_id,omitempty"`
	Verdict             string `json:"verdict"`
	State               string `json:"state,omitempty"`
	ResultingTaskStatus string `json:"resulting_task_status,omitempty"`
	PacketID            int64  `json:"packet_id,omitempty"`
	PacketMessageID     *int64 `json:"packet_message_id,omitempty"`
	MessageID           *int64 `json:"message_id,omitempty"`
	LastErrorStep       string `json:"last_error_step,omitempty"`
	LastError           string `json:"last_error,omitempty"`
}

type Job struct {
	ID           string        `json:"id"`
	Admission    Admission     `json:"admission"`
	State        State         `json:"state"`
	Failure      string        `json:"failure,omitempty"`
	Finalization *Finalization `json:"-"`
	Receipt      *Receipt      `json:"receipt,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// Projection intentionally excludes runtime worker identifiers, prompts and transcripts.
type Projection struct {
	ID        string    `json:"id"`
	Key       Key       `json:"key"`
	State     State     `json:"state"`
	Failure   string    `json:"failure,omitempty"`
	Receipt   *Receipt  `json:"receipt,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (j Job) Projection() Projection {
	return Projection{j.ID, j.Admission.Key, j.State, j.Failure, j.Receipt, j.CreatedAt, j.UpdatedAt}
}

type Context struct {
	Key       Key
	NextState string
	Profile   string
	Workspace string
}

func (c Context) ReviewableFor(k Key) bool { return c.Key == k && c.NextState == "source_review_ready" }

type DenReviewClient interface {
	GetReviewContext(context.Context, Key) (Context, error)
	FinalizeReview(context.Context, Finalization) (Receipt, error)
}
type Worker interface{}
type ReviewerRuntime interface {
	Acquire(context.Context, TaskKey, string, string) (Worker, error)
	Run(context.Context, Worker, string, func(Completion) error) error
	Release(context.Context, Worker) error
	Close() error
}
type Clock interface{ Now() time.Time }
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type RetainedAffinity struct {
	ProjectID string    `json:"project_id"`
	TaskID    int64     `json:"task_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// UnavailableRuntime makes the command safe to run for admission/readback before #7414.
type UnavailableRuntime struct{}

func (UnavailableRuntime) Acquire(context.Context, TaskKey, string, string) (Worker, error) {
	return nil, errors.New("reviewer runtime is not configured")
}
func (UnavailableRuntime) Run(context.Context, Worker, string, func(Completion) error) error {
	return errors.New("reviewer runtime is not configured")
}
func (UnavailableRuntime) Release(context.Context, Worker) error { return nil }
func (UnavailableRuntime) Close() error                          { return nil }
