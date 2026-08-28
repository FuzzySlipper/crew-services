package review

import "context"

type Store interface {
	Admit(context.Context, Admission) (Job, bool, error)
	Get(context.Context, string) (Job, error)
	Claim(context.Context) (Job, bool, error)
	ClaimPreferred(context.Context, TaskKey) (Job, bool, error)
	PutFinalization(context.Context, string, Finalization) (Job, error)
	Complete(context.Context, string, Receipt) (Job, error)
	ReleaseFinalization(context.Context, string) error
	Fail(context.Context, string, State, string) (Job, error)
	Cancel(context.Context, string) (Job, error)
	Requeue(context.Context, string) (Job, error)
	Recover(context.Context) error
	Snapshot(context.Context, int) (Snapshot, error)
	Ready(context.Context) error
	Close() error
}

type Snapshot struct {
	Backend  string             `json:"backend"`
	Capacity int                `json:"capacity"`
	Queued   int                `json:"queued"`
	Running  int                `json:"running"`
	Recent   []Projection       `json:"recent"`
	Retained []RetainedAffinity `json:"retained_affinities"`
}
