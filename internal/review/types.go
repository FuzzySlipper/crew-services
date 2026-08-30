// Package review implements the bounded, runtime-neutral Den review job runner.
// It is deliberately separate from the crew messaging fabric.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	// ErrTaskNotReviewable is returned by the browser-facing manual admission
	// surface when the authoritative Den task is no longer in review. The
	// caller should refresh its capability rather than retrying the same click.
	ErrTaskNotReviewable = errors.New("task is no longer in review")
	// Adapter implementations classify Den's authoritative terminal responses.
	ErrStaleRound        = errors.New("Den review round is stale")
	ErrDenConflict       = errors.New("Den review finalization conflicts")
	ErrDenRejected       = errors.New("Den rejected the review finalization")
	ErrCapacity          = errors.New("reviewer runtime is at capacity")
	ErrAffinityBusy      = errors.New("retained reviewer is busy")
	ErrSubmissionChanged = errors.New("review submission changed while it was being advanced")
	ErrSubmissionStore   = errors.New("review submission store is not configured")
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
	Repository     string `json:"repository,omitempty"`
	Ref            string `json:"ref,omitempty"`
	CommitSHA      string `json:"commit_sha,omitempty"`
	Status         string `json:"status,omitempty"`
	Handle         string `json:"handle,omitempty"`
	TerminalReason string `json:"terminal_reason,omitempty"`
	FailureSummary string `json:"failure_summary,omitempty"`
}

type Admission struct {
	IdempotencyKey string       `json:"idempotency_key"`
	Key            Key          `json:"key"`
	Reviewer       string       `json:"reviewer"`
	Workspace      string       `json:"workspace,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	Gate           GateEvidence `json:"gate,omitempty"`
	PacketHandle   string       `json:"packet_handle,omitempty"`
	// ReviewPreamble is controller-owned prompt context for a manual
	// best-effort admission. It is intentionally persisted with the private
	// admission envelope, while Job.Projection never exposes it.
	ReviewPreamble string `json:"review_preamble,omitempty"`
}

type ManualReviewMode string

const (
	ManualReviewExact      ManualReviewMode = "exact"
	ManualReviewBestEffort ManualReviewMode = "best_effort"
)

type ManualReviewSource struct {
	Repository string `json:"repository"`
	CommitSHA  string `json:"commit_sha"`
	Ref        string `json:"ref"`
}

// ManualReviewCapability is the small read model used by Den Web to decide
// whether its contextual manual-review button should be shown. Den remains
// authoritative for task status; crew-review only reports the source evidence
// it can safely reuse from its durable submission ledger.
type ManualReviewCapability struct {
	Eligible bool                `json:"eligible"`
	Mode     ManualReviewMode    `json:"mode"`
	Label    string              `json:"label"`
	Detail   string              `json:"detail"`
	Source   *ManualReviewSource `json:"source,omitempty"`
}

// ManualReviewSubmissionRequest carries only browser transport metadata. The
// service resolves task status, review round, and source evidence itself.
type ManualReviewSubmissionRequest struct {
	ProjectID      string
	TaskID         int64
	IdempotencyKey string
	Reviewer       string
}

type ManualReviewReceipt struct {
	Mode    ManualReviewMode `json:"mode"`
	Status  string           `json:"status"`
	Message string           `json:"message"`
	JobID   string           `json:"job_id,omitempty"`
	RoundID int64            `json:"round_id,omitempty"`
}

// ManualReviewRequest is sent by the review-specific Den adapter to create or
// reuse a Den review round without fabricating source-control evidence.
type ManualReviewRequest struct {
	ProjectID string
	TaskID    int64
	Ref       string
	Reviewer  string
	Preamble  string
}

// TaskContext is the authoritative subset needed by the manual admission
// gate. The adapter separately retains the complete bounded task context in
// the private reviewer material.
type TaskContext struct {
	ProjectID            string
	TaskID               int64
	Status               string
	CurrentReviewRoundID int64
}

// SubmissionRequest is the runtime-neutral managed review entry point. It is
// deliberately the same small envelope callers already use for managed Den
// reviews; the service derives retry identity when no transport idempotency
// key is available.
type SubmissionRequest struct {
	ProjectID      string   `json:"project_id"`
	TaskID         int64    `json:"task_id"`
	Repository     string   `json:"repository"`
	CommitSHA      string   `json:"commit_sha"`
	Ref            string   `json:"ref"`
	RequiredChecks []string `json:"required_checks"`
	BaseCommit     string   `json:"base_commit,omitempty"`
	ReviewSummary  string   `json:"review_summary_md"`
	Reviewer       string   `json:"reviewer,omitempty"`

	// IdempotencyKey is transport metadata, not part of the public MCP tool
	// schema. A direct HTTP caller may supply Idempotency-Key; the Den gateway
	// uses the deterministic target identity derived by the service.
	IdempotencyKey string `json:"-"`
}

type ReviewRoundRef struct {
	ID        int64
	ProjectID string
	TaskID    int64
}

type GateRequest struct {
	ProjectID      string
	TaskID         int64
	Repository     string
	CommitSHA      string
	Ref            string
	RequiredChecks []string
	RequestedBy    string
}

type SubmissionPhase string

const (
	SubmissionAccepted      SubmissionPhase = "accepted"
	SubmissionRoundRecorded SubmissionPhase = "review_round_recorded"
	SubmissionGatePending   SubmissionPhase = "gate_pending"
	SubmissionGatePassed    SubmissionPhase = "gate_passed"
	SubmissionGateFailed    SubmissionPhase = "gate_failed"
	SubmissionJobAdmitted   SubmissionPhase = "job_admitted"
	SubmissionUnavailable   SubmissionPhase = "unavailable"
	SubmissionStale         SubmissionPhase = "stale"
)

func (p SubmissionPhase) Terminal() bool {
	return p == SubmissionGateFailed || p == SubmissionJobAdmitted || p == SubmissionStale
}

// SubmissionRecord is the durable handoff ledger between the caller, Den's
// review/gate authorities, and the local job queue. It contains no runtime
// worker or provider details.
type SubmissionRecord struct {
	ID             string
	IdempotencyKey string
	MaterialHash   string
	Request        SubmissionRequest
	Phase          SubmissionPhase
	ReviewRoundID  int64
	Gate           GateEvidence
	JobID          string
	Failure        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type SubmissionTransition struct {
	Phase         SubmissionPhase
	ReviewRoundID *int64
	Gate          *GateEvidence
	JobID         *string
	Failure       *string
}

// SubmissionReceipt is intentionally provider-neutral. It reports Den
// review/gate handles and the local durable job id, but never a native worker
// session or model identity.
type SubmissionReceipt struct {
	Schema             string          `json:"schema"`
	SchemaVersion      int             `json:"schema_version"`
	OK                 bool            `json:"ok"`
	Retryable          bool            `json:"retryable,omitempty"`
	SubmissionID       string          `json:"submission_id"`
	ProjectID          string          `json:"project_id"`
	TaskID             int64           `json:"task_id"`
	Repository         string          `json:"repository"`
	CommitSHA          string          `json:"commit_sha"`
	Ref                string          `json:"ref"`
	RequiredChecks     []string        `json:"required_checks"`
	BaseCommit         string          `json:"base_commit,omitempty"`
	Reviewer           string          `json:"reviewer"`
	Phase              SubmissionPhase `json:"phase"`
	ReviewRoundID      int64           `json:"review_round_id,omitempty"`
	GateID             string          `json:"gate_id,omitempty"`
	GateStatus         string          `json:"gate_status,omitempty"`
	GateTerminalReason string          `json:"gate_terminal_reason,omitempty"`
	JobID              string          `json:"job_id,omitempty"`
	ErrorCode          string          `json:"error_code,omitempty"`
	Error              string          `json:"error,omitempty"`
	Summary            string          `json:"summary"`
	Replayed           bool            `json:"replayed,omitempty"`
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
	if c.Verdict != "looks_good" && c.Verdict != "changes_requested" {
		return false
	}
	// Den creates every new finding as open. A looks_good decision carrying a
	// new finding can therefore never satisfy Den's no-unresolved-findings
	// invariant; non-blocking observations belong in notes instead.
	if c.Verdict == "looks_good" && len(c.NewFindings) > 0 {
		return false
	}
	// A changes_requested verdict is actionable only when it records a new
	// finding for this controller-bound review round. Prior resolutions describe
	// earlier findings and cannot stand in for a current-round finding.
	if c.Verdict == "changes_requested" && len(c.NewFindings) == 0 {
		return false
	}
	for _, finding := range c.NewFindings {
		if !validNewFinding(finding) {
			return false
		}
	}
	for _, resolution := range c.PriorResolutions {
		if !validFindingResolutionStatus(resolution.Status) {
			return false
		}
	}
	return true
}
func (c Completion) Valid() bool { return c.valid() }

func validNewFinding(finding NewFinding) bool {
	return validFindingCategory(finding.Category) && strings.TrimSpace(finding.Summary) != ""
}

func validFindingCategory(category string) bool {
	switch category {
	case "blocking_bug", "acceptance_gap", "test_weakness", "follow_up_candidate":
		return true
	default:
		return false
	}
}

func validFindingResolutionStatus(status string) bool {
	switch status {
	case "verified_fixed", "not_fixed", "superseded", "split_to_follow_up":
		return true
	default:
		return false
	}
}

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
	// Material is the bounded, validated Den structuredContent for this review.
	// It is private prompt input only: jobs and public pool projections must not
	// persist or expose it.
	Material json.RawMessage
}

func (c Context) ReviewableFor(k Key) bool { return c.Key == k && c.NextState == "source_review_ready" }

type DenReviewClient interface {
	GetReviewContext(context.Context, Key) (Context, error)
	FinalizeReview(context.Context, Finalization) (Receipt, error)
}

// ManualReviewDenClient is implemented only by the review-specific adapter.
// It keeps task authorization and commitless round creation out of the
// runtime-neutral messaging core.
type ManualReviewDenClient interface {
	DenReviewClient
	GetTaskContext(context.Context, TaskKey) (TaskContext, error)
	RequestManualReview(context.Context, ManualReviewRequest) (ReviewRoundRef, error)
}

// FinalizationValidator is an optional adapter capability for checking the
// exact downstream request contract before durable finalization is recorded.
type FinalizationValidator interface {
	ValidateFinalization(Finalization) error
}

// SubmissionDenClient is the additional narrow seam used only by the
// managed-submission boundary. Keeping it separate means existing job tests
// and alternate Den adapters do not need to implement submission orchestration
// until they opt into that route.
type SubmissionDenClient interface {
	RequestReview(context.Context, SubmissionRequest) (ReviewRoundRef, error)
	WatchGitHubChecks(context.Context, GateRequest) (GateEvidence, error)
	GetGitHubCheckGate(context.Context, GateRequest) (GateEvidence, error)
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
