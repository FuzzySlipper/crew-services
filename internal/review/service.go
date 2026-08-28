package review

import (
	"context"
	"errors"
	"fmt"
)

type Service struct {
	store   Store
	den     DenReviewClient
	runtime ReviewerRuntime
	profile string
}

func New(store Store, den DenReviewClient, runtime ReviewerRuntime, profile string) (*Service, error) {
	if store == nil || den == nil || runtime == nil {
		return nil, errors.New("store, Den client, and runtime are required")
	}
	if profile == "" {
		return nil, errors.New("review profile is required")
	}
	return &Service{store, den, runtime, profile}, nil
}
func (s *Service) Admit(ctx context.Context, a Admission) (Job, bool, error) {
	return s.store.Admit(ctx, a)
}
func (s *Service) Get(ctx context.Context, id string) (Job, error)    { return s.store.Get(ctx, id) }
func (s *Service) Cancel(ctx context.Context, id string) (Job, error) { return s.store.Cancel(ctx, id) }
func (s *Service) Snapshot(ctx context.Context, n int) (Snapshot, error) {
	return s.store.Snapshot(ctx, n)
}
func (s *Service) Ready(ctx context.Context) error { return s.store.Ready(ctx) }

// RunOne advances one durable job. A process may call this loop from its own bounded runner.
func (s *Service) RunOne(ctx context.Context) (bool, error) {
	j, ok, e := s.store.Claim(ctx)
	if e != nil || !ok {
		return ok, e
	}
	if j.State == Finalizing {
		return true, s.reconcile(ctx, j)
	}
	return true, s.execute(ctx, j)
}
func (s *Service) execute(ctx context.Context, j Job) error {
	c, e := s.den.GetReviewContext(ctx, j.Admission.Key)
	if e != nil {
		return s.terminal(ctx, j, Failed, fmt.Sprintf("get review context: %v", e))
	}
	if !c.ReviewableFor(j.Admission.Key) {
		return s.terminal(ctx, j, Stale, "review round is no longer source_review_ready")
	}
	w, e := s.runtime.Acquire(ctx, j.Admission.Key.Task(), s.profile, first(c.Workspace, j.Admission.Workspace))
	if e != nil {
		return s.terminal(ctx, j, Failed, e.Error())
	}
	defer s.runtime.Release(context.Background(), w)
	completionStored := false
	prompt := fmt.Sprintf("Review Den project %q task %d review round %d (correlation %q). Load the current Den reviewer context; use only the controller-bound completion result.", j.Admission.Key.ProjectID, j.Admission.Key.TaskID, j.Admission.Key.ReviewRoundID, j.Admission.Key.CorrelationID)
	runErr := s.runtime.Run(ctx, w, prompt, func(candidate Completion) error {
		if !candidate.valid() {
			return errors.New("runtime returned invalid review verdict")
		}
		f := Finalization{Key: j.Admission.Key, Reviewer: j.Admission.Reviewer, Completion: candidate}
		_, e := s.store.PutFinalization(ctx, j.ID, f)
		if e == nil {
			completionStored = true
		}
		return e
	})
	current, getErr := s.store.Get(ctx, j.ID)
	if getErr != nil {
		return getErr
	}
	if current.State == Cancelled {
		return nil
	}
	if current.State != Finalizing {
		if runErr != nil {
			return s.terminal(ctx, j, Failed, runErr.Error())
		}
		if !completionStored {
			return s.terminal(ctx, j, Failed, "runtime completed without a structured completion")
		}
		return s.terminal(ctx, j, Failed, "runtime left job without finalization")
	}
	return s.reconcile(ctx, current)
}
func (s *Service) reconcile(ctx context.Context, j Job) error {
	if j.Finalization == nil {
		return errors.New("finalizing job missing stored material")
	}
	r, e := s.den.FinalizeReview(ctx, *j.Finalization)
	if e != nil {
		if errors.Is(e, ErrStaleRound) {
			return s.terminal(ctx, j, Stale, e.Error())
		}
		if errors.Is(e, ErrDenConflict) {
			return s.terminal(ctx, j, Failed, e.Error())
		}
		// An ambiguous transport failure retains the exact stored request in
		// finalizing for retry; Den's receipt is the only completion authority.
		_ = s.store.ReleaseFinalization(ctx, j.ID)
		return e
	}
	_, e = s.store.Complete(ctx, j.ID, r)
	return e
}
func (s *Service) terminal(ctx context.Context, j Job, state State, why string) error {
	_, e := s.store.Fail(ctx, j.ID, state, why)
	return e
}
func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
