package review

import "context"

type Store interface {
	Admit(context.Context, Admission) (Job, bool, error)
	// AdmitRound returns the existing job for a Den review round when one is
	// already present. A round is the Den-owned unit of review execution; this
	// guard prevents two submission retries from creating two local jobs.
	AdmitRound(context.Context, Admission) (Job, bool, error)
	Get(context.Context, string) (Job, error)
	Claim(context.Context) (Job, bool, error)
	ClaimPreferred(context.Context, TaskKey) (Job, bool, error)
	PutFinalization(context.Context, string, Finalization) (Job, error)
	Complete(context.Context, string, Receipt) (Job, error)
	ReleaseFinalization(context.Context, string) error
	Fail(context.Context, string, State, string) (Job, error)
	Cancel(context.Context, string) (Job, error)
	Requeue(context.Context, string) (Job, error)
	// RetryFailed deliberately permits only an operator-directed retry of a
	// terminal failed job. It keeps the original durable identity while
	// clearing material that belongs to the failed attempt.
	RetryFailed(context.Context, string) (Job, error)
	Recover(context.Context) error
	Snapshot(context.Context, int) (Snapshot, error)
	Ready(context.Context) error
	Close() error
}

type SubmissionStore interface {
	AdmitSubmission(context.Context, SubmissionRequest, string, string) (SubmissionRecord, bool, error)
	GetSubmission(context.Context, string) (SubmissionRecord, error)
	TransitionSubmission(context.Context, string, SubmissionPhase, SubmissionTransition) (SubmissionRecord, error)
}

// ManualReviewSubmissionStore is an additive capability. Keeping it
// separate preserves the existing managed-submission seam for alternate
// adapters that have not opted into the browser-facing capability read model.
type ManualReviewSubmissionStore interface {
	SubmissionStore
	LatestReusableSubmission(context.Context, string, int64) (SubmissionRecord, bool, error)
}

type Snapshot struct {
	Backend    string             `json:"backend"`
	Capacity   int                `json:"capacity"`
	Queued     int                `json:"queued"`
	Running    int                `json:"running"`
	Finalizing int                `json:"finalizing"`
	Active     []Projection       `json:"active"`
	Recent     []Projection       `json:"recent"`
	Retained   []RetainedAffinity `json:"retained_affinities"`
}
