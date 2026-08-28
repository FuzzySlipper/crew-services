package review

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Service struct {
	store      Store
	den        DenReviewClient
	runtime    ReviewerRuntime
	profile    string
	clock      Clock
	mu         sync.Mutex
	affinities map[TaskKey]*affinity
}
type affinity struct {
	worker    Worker
	expiresAt time.Time
	busy      bool
}
type Option func(*Service)

func WithClock(clock Clock) Option {
	return func(s *Service) {
		if clock != nil {
			s.clock = clock
		}
	}
}

func New(store Store, den DenReviewClient, runtime ReviewerRuntime, profile string, options ...Option) (*Service, error) {
	if store == nil || den == nil || runtime == nil {
		return nil, errors.New("store, Den client, and runtime are required")
	}
	if profile == "" {
		return nil, errors.New("review profile is required")
	}
	s := &Service{store: store, den: den, runtime: runtime, profile: profile, clock: SystemClock{}, affinities: map[TaskKey]*affinity{}}
	for _, option := range options {
		option(s)
	}
	return s, nil
}
func (s *Service) Admit(ctx context.Context, a Admission) (Job, bool, error) {
	s.reapExpired(ctx)
	job, replayed, err := s.store.Admit(ctx, a)
	if err == nil && !replayed {
		s.mu.Lock()
		if retained := s.affinities[a.Key.Task()]; retained != nil && !retained.busy {
			retained.expiresAt = s.clock.Now().Add(12 * time.Hour)
		}
		s.mu.Unlock()
	}
	return job, replayed, err
}
func (s *Service) Get(ctx context.Context, id string) (Job, error)    { return s.store.Get(ctx, id) }
func (s *Service) Cancel(ctx context.Context, id string) (Job, error) { return s.store.Cancel(ctx, id) }
func (s *Service) Snapshot(ctx context.Context, n int) (Snapshot, error) {
	s.reapExpired(ctx)
	v, err := s.store.Snapshot(ctx, n)
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	for key, value := range s.affinities {
		v.Retained = append(v.Retained, RetainedAffinity{ProjectID: key.ProjectID, TaskID: key.TaskID, ExpiresAt: value.expiresAt})
	}
	s.mu.Unlock()
	return v, nil
}
func (s *Service) Ready(ctx context.Context) error { return s.store.Ready(ctx) }

// RunOne advances one durable job. A process may call this loop from its own bounded runner.
func (s *Service) RunOne(ctx context.Context) (bool, error) {
	s.reapExpired(ctx)
	var j Job
	var ok bool
	var e error
	for _, task := range s.idleAffinityTasks() {
		j, ok, e = s.store.ClaimPreferred(ctx, task)
		if e != nil || ok {
			break
		}
	}
	if !ok && e == nil {
		j, ok, e = s.store.Claim(ctx)
	}
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
	w, reused, e := s.workerFor(ctx, j.Admission.Key.Task(), first(c.Workspace, j.Admission.Workspace))
	if e != nil {
		if errors.Is(e, ErrCapacity) || errors.Is(e, ErrAffinityBusy) {
			_, requeueErr := s.store.Requeue(ctx, j.ID)
			return requeueErr
		}
		return s.terminal(ctx, j, Failed, e.Error())
	}
	retain := false
	defer func() {
		if retain {
			s.mu.Lock()
			if current := s.affinities[j.Admission.Key.Task()]; current != nil && current.worker == w {
				current.busy = false
			}
			s.mu.Unlock()
			return
		}
		s.releaseWorker(w, j.Admission.Key.Task(), reused)
	}()
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
	err := s.reconcile(ctx, current)
	if err == nil {
		completed, readErr := s.store.Get(ctx, j.ID)
		if readErr != nil {
			return readErr
		}
		if completed.Receipt != nil && completed.Receipt.Verdict == "changes_requested" {
			s.retain(j.Admission.Key.Task(), w, j.CreatedAt)
			retain = true
		}
	}
	return err
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
func (s *Service) workerFor(ctx context.Context, key TaskKey, workspace string) (Worker, bool, error) {
	s.mu.Lock()
	if retained := s.affinities[key]; retained != nil {
		if retained.busy {
			s.mu.Unlock()
			return nil, true, ErrAffinityBusy
		}
		retained.busy = true
		worker := retained.worker
		s.mu.Unlock()
		return worker, true, nil
	}
	s.mu.Unlock()
	worker, err := s.runtime.Acquire(ctx, key, s.profile, workspace)
	return worker, false, err
}
func (s *Service) retain(key TaskKey, worker Worker, admitted time.Time) {
	s.mu.Lock()
	deadline := admitted.Add(12 * time.Hour)
	if existing := s.affinities[key]; existing != nil && existing.worker == worker && existing.expiresAt.After(deadline) {
		deadline = existing.expiresAt
	}
	s.affinities[key] = &affinity{worker: worker, expiresAt: deadline}
	s.mu.Unlock()
}
func (s *Service) releaseWorker(worker Worker, key TaskKey, reused bool) {
	if reused {
		s.mu.Lock()
		if current := s.affinities[key]; current != nil && current.worker == worker {
			delete(s.affinities, key)
		}
		s.mu.Unlock()
	}
	_ = s.runtime.Release(context.Background(), worker)
}
func (s *Service) reapExpired(ctx context.Context) {
	now := s.clock.Now()
	var workers []Worker
	s.mu.Lock()
	for key, value := range s.affinities {
		if !value.busy && !value.expiresAt.After(now) {
			delete(s.affinities, key)
			workers = append(workers, value.worker)
		}
	}
	s.mu.Unlock()
	for _, worker := range workers {
		_ = s.runtime.Release(ctx, worker)
	}
}
func (s *Service) ReleaseAffinity(ctx context.Context, key TaskKey) error {
	s.mu.Lock()
	value := s.affinities[key]
	if value == nil {
		s.mu.Unlock()
		return ErrNotFound
	}
	if value.busy {
		s.mu.Unlock()
		return ErrAffinityBusy
	}
	delete(s.affinities, key)
	s.mu.Unlock()
	return s.runtime.Release(ctx, value.worker)
}
func (s *Service) Close() error {
	s.mu.Lock()
	workers := make([]Worker, 0, len(s.affinities))
	for _, value := range s.affinities {
		workers = append(workers, value.worker)
	}
	s.affinities = map[TaskKey]*affinity{}
	s.mu.Unlock()
	for _, worker := range workers {
		_ = s.runtime.Release(context.Background(), worker)
	}
	return s.runtime.Close()
}
func (s *Service) idleAffinityTasks() []TaskKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]TaskKey, 0, len(s.affinities))
	for key, value := range s.affinities {
		if !value.busy {
			tasks = append(tasks, key)
		}
	}
	return tasks
}
