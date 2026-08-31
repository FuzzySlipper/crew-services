package review

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fakeDen struct {
	mu       sync.Mutex
	contexts map[int64]Context
	receipts map[int64]Receipt
	calls    int
	fail     bool
	finalErr error
	started  chan struct{}
	release  chan struct{}
}

func (d *fakeDen) GetReviewContext(_ context.Context, k Key) (Context, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contexts[k.ReviewRoundID]
	if !ok {
		return Context{}, errors.New("missing context")
	}
	return c, nil
}
func (d *fakeDen) FinalizeReview(_ context.Context, f Finalization) (Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.started != nil {
		d.started <- struct{}{}
	}
	if d.release != nil {
		<-d.release
	}
	if d.finalErr != nil {
		return Receipt{}, d.finalErr
	}
	if d.fail {
		return Receipt{}, errors.New("response lost")
	}
	if r, ok := d.receipts[f.Key.ReviewRoundID]; ok {
		return r, nil
	}
	r := Receipt{Schema: "den_review.completion_receipt.v1", FinalizationID: f.Key.ReviewRoundID, ProjectID: f.Key.ProjectID, TaskID: f.Key.TaskID, ReviewRoundID: f.Key.ReviewRoundID, State: "complete", ResultingTaskStatus: "done", Verdict: f.Completion.Verdict, LastError: strconv.FormatInt(f.Key.ReviewRoundID, 10)}
	d.receipts[f.Key.ReviewRoundID] = r
	return r, nil
}

type fakeRuntime struct {
	mu              sync.Mutex
	running         int
	max             int
	start           chan struct{}
	release         chan struct{}
	completion      Completion
	acquired        int
	released        int
	skipCompletion  bool
	runErr          error
	completed       chan struct{}
	afterCompletion chan struct{}
	prompt          string
	capacity        int
	releasedWorkers []Worker
	releaseErr      error
	workspaces      []string
}

func (r *fakeRuntime) Acquire(_ context.Context, _ TaskKey, _ string, workspace string) (Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capacity > 0 && r.acquired-r.released >= r.capacity {
		return nil, ErrCapacity
	}
	r.acquired++
	r.workspaces = append(r.workspaces, workspace)
	return r.acquired, nil
}
func (r *fakeRuntime) Run(ctx context.Context, _ Worker, prompt string, complete func(Completion) error) error {
	r.mu.Lock()
	r.running++
	r.prompt = prompt
	if r.running > r.max {
		r.max = r.running
	}
	r.mu.Unlock()
	if r.start != nil {
		r.start <- struct{}{}
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var e error
	if !r.skipCompletion {
		e = complete(r.completion)
	}
	if r.completed != nil {
		r.completed <- struct{}{}
	}
	if r.afterCompletion != nil {
		select {
		case <-r.afterCompletion:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.runErr != nil {
		e = r.runErr
	}
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return e
}
func (r *fakeRuntime) Release(_ context.Context, worker Worker) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.releaseErr != nil {
		return r.releaseErr
	}
	r.released++
	r.releasedWorkers = append(r.releasedWorkers, worker)
	return nil
}
func (*fakeRuntime) Close() error { return nil }

func (r *fakeRuntime) Prompt() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prompt
}

func fixture(t *testing.T, capacity int) (*Service, *SQLiteStore, *fakeDen, *fakeRuntime, Admission) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	store, e := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "crew-review.db"), clock, capacity)
	if e != nil {
		t.Fatal(e)
	}
	key := Key{ProjectID: "dsh-crew", TaskID: 7413, ReviewRoundID: 1, CorrelationID: "correlation-1"}
	den := &fakeDen{contexts: map[int64]Context{1: {Key: key, NextState: "source_review_ready", Workspace: "/repo"}}, receipts: map[int64]Receipt{}}
	runtime := &fakeRuntime{completion: Completion{Verdict: "looks_good"}}
	svc, e := New(store, den, runtime, "review profile", WithClock(clock))
	if e != nil {
		t.Fatal(e)
	}
	return svc, store, den, runtime, Admission{IdempotencyKey: "idem-1", Key: key, Reviewer: "reviewer", Workspace: "/repo"}
}

func changesRequestedCompletion() Completion {
	return Completion{Verdict: "changes_requested", NewFindings: []NewFinding{{Category: "blocking_bug", Summary: "current round regression"}}}
}

func TestChangesRequestedCompletionRequiresCurrentRoundNewFinding(t *testing.T) {
	cases := []struct {
		name       string
		completion Completion
		want       bool
	}{
		{name: "looks good", completion: Completion{Verdict: "looks_good"}, want: true},
		{name: "no finding", completion: Completion{Verdict: "changes_requested"}},
		{name: "prior not fixed only", completion: Completion{Verdict: "changes_requested", PriorResolutions: []FindingResolution{{FindingID: 1, Status: "not_fixed", VerificationNote: "still fails"}}}},
		{name: "missing summary", completion: Completion{Verdict: "changes_requested", NewFindings: []NewFinding{{Category: "blocking_bug"}}}},
		{name: "new current finding", completion: changesRequestedCompletion(), want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.completion.Valid(); got != testCase.want {
				t.Fatalf("Valid() = %v, want %v for %+v", got, testCase.want, testCase.completion)
			}
		})
	}
}

func TestInvalidChangesRequestedDoesNotReachDenFinalization(t *testing.T) {
	svc, store, den, runtime, admission := fixture(t, 1)
	defer store.Close()
	runtime.completion = Completion{
		Verdict:          "changes_requested",
		PriorResolutions: []FindingResolution{{FindingID: 1, Status: "not_fixed", VerificationNote: "still fails"}},
	}
	job, _, err := svc.Admit(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil || got.State != Failed || den.calls != 0 {
		t.Fatalf("job=%+v Den finalizations=%d err=%v", got, den.calls, err)
	}
}

func TestAdmissionConflictAndSafeProjection(t *testing.T) {
	svc, store, _, _, a := fixture(t, 1)
	defer store.Close()
	first, replay, e := svc.Admit(context.Background(), a)
	if e != nil || replay {
		t.Fatalf("first admit=%+v replay=%v err=%v", first, replay, e)
	}
	again, replay, e := svc.Admit(context.Background(), a)
	if e != nil || !replay || again.ID != first.ID {
		t.Fatalf("replay=%+v %v %v", again, replay, e)
	}
	a.Workspace = "different"
	if _, _, e = svc.Admit(context.Background(), a); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflict err=%v", e)
	}
	p := first.Projection()
	if p.Key.TaskID != 7413 || p.ID == "" {
		t.Fatalf("projection=%+v", p)
	}
}

func TestSnapshotReportsConfiguredBackendWithoutWorkerIdentity(t *testing.T) {
	_, store, den, runtime, _ := fixture(t, 1)
	defer store.Close()
	svc, err := New(store, den, runtime, "profile", WithBackend(" codex "))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Backend != "codex" {
		t.Fatalf("snapshot backend = %q, want codex", snapshot.Backend)
	}
	if len(snapshot.Recent) != 0 || len(snapshot.Retained) != 0 {
		t.Fatalf("empty projection unexpectedly exposed worker data: %+v", snapshot)
	}
}

func TestFinalizationRecoveryAndCancellation(t *testing.T) {
	svc, store, den, _, a := fixture(t, 1)
	defer store.Close()
	j, _, e := svc.Admit(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = store.Claim(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = store.PutFinalization(context.Background(), j.ID, Finalization{Key: a.Key, Reviewer: a.Reviewer, Completion: Completion{Verdict: "looks_good"}}); e != nil {
		t.Fatal(e)
	}
	snapshot, e := svc.Snapshot(context.Background(), 5)
	if e != nil || snapshot.Finalizing != 1 || len(snapshot.Active) != 1 || snapshot.Active[0].State != Finalizing {
		t.Fatalf("finalizing job is not visible in pool projection: snapshot=%+v err=%v", snapshot, e)
	}
	if e = store.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	if ran, e := svc.RunOne(context.Background()); !ran || e != nil {
		t.Fatalf("reconcile ran=%v err=%v", ran, e)
	}
	done, e := svc.Get(context.Background(), j.ID)
	if e != nil || done.State != Succeeded || den.calls != 1 {
		t.Fatalf("done=%+v calls=%d err=%v", done, den.calls, e)
	}
	cancel, _, e := svc.Admit(context.Background(), Admission{IdempotencyKey: "cancel", Key: Key{ProjectID: "dsh-crew", TaskID: 2, ReviewRoundID: 2, CorrelationID: "c2"}, Reviewer: "reviewer"})
	if e != nil {
		t.Fatal(e)
	}
	if got, e := svc.Cancel(context.Background(), cancel.ID); e != nil || got.State != Cancelled {
		t.Fatalf("cancel=%+v %v", got, e)
	}
	if _, e = svc.Cancel(context.Background(), j.ID); !errors.Is(e, ErrTooLate) {
		t.Fatalf("late cancel=%v", e)
	}
}

func TestTaskSerializationAndAggregateCapacity(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 1)
	defer store.Close()
	a2 := a
	a2.IdempotencyKey = "idem-2"
	a2.Key.TaskID = 99
	a2.Key.ReviewRoundID = 2
	a2.Key.CorrelationID = "c2"
	den.contexts[2] = Context{Key: a2.Key, NextState: "source_review_ready"}
	if _, _, e := svc.Admit(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, _, e := svc.Admit(context.Background(), a2); e != nil {
		t.Fatal(e)
	}
	runtime.start = make(chan struct{}, 2)
	runtime.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := svc.RunOne(context.Background()); done <- e }()
	<-runtime.start
	if ran, e := svc.RunOne(context.Background()); e != nil || ran {
		t.Fatalf("capacity ran=%v err=%v", ran, e)
	}
	runtime.release <- struct{}{}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	runtime.release = make(chan struct{}, 1)
	runtime.release <- struct{}{}
	if ran, e := svc.RunOne(context.Background()); e != nil || !ran {
		t.Fatalf("second ran=%v err=%v", ran, e)
	}
	if runtime.max != 1 {
		t.Fatalf("max concurrent runtime=%d", runtime.max)
	}
}

func TestStaleContextPreventsRuntime(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 1)
	defer store.Close()
	den.contexts[a.Key.ReviewRoundID] = Context{Key: a.Key, NextState: "gate_pending"}
	j, _, e := svc.Admit(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	_, e = svc.RunOne(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	got, e := svc.Get(context.Background(), j.ID)
	if e != nil || got.State != Stale || runtime.acquired != 0 {
		t.Fatalf("job=%+v acquired=%d err=%v", got, runtime.acquired, e)
	}
}

func TestRuntimePromptCarriesAuthoritativeBoundedDenContext(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 1)
	defer store.Close()
	material := []byte(`{"schema":"den_review.reviewer_context.v1","prior_findings":[{"id":"R7413-1","body":"check the stale claim"}],"rereview_packet":{"round":2,"addresses":["R7413-1"],"request":"verify the fix"}}`)
	den.contexts[a.Key.ReviewRoundID] = Context{
		Key:       a.Key,
		NextState: "source_review_ready",
		Workspace: "/repo",
		Material:  material,
	}
	if _, _, err := svc.Admit(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := runtime.Prompt()
	if !strings.Contains(prompt, "authoritative bounded Den reviewer context") || !strings.Contains(prompt, "Do not attempt a second Den fetch") {
		t.Fatalf("prompt did not identify the authoritative handoff: %q", prompt)
	}
	if !strings.Contains(prompt, "<den_reviewer_context>\n"+string(material)+"\n</den_reviewer_context>") {
		t.Fatalf("prompt did not preserve exact Den material: %q", prompt)
	}
	if !strings.Contains(prompt, `"prior_findings"`) || !strings.Contains(prompt, `"rereview_packet"`) {
		t.Fatalf("prompt omitted prior findings or rereview packet: %q", prompt)
	}
}

func TestLateRuntimeResultCannotResurrectCancelledJob(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	runtime.start = make(chan struct{}, 1)
	runtime.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := svc.RunOne(context.Background()); done <- err }()
	<-runtime.start
	if _, err := svc.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	runtime.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Cancelled {
		t.Fatalf("late result changed job=%+v err=%v", got, err)
	}
}

func TestProcessCancellationRequeuesInFlightJob(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	runtime.start = make(chan struct{}, 1)
	runtime.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := svc.RunOne(ctx)
		done <- runErr
	}()
	<-runtime.start
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOne cancellation error = %v", err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Queued {
		t.Fatalf("cancelled job state = %s, want queued", got.State)
	}
}

func TestRuntimeRestartRequeuesInFlightJob(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	runtime.skipCompletion = true
	runtime.runErr = fmt.Errorf("DSH restarted: %w", ErrRuntimeUnavailable)
	if _, err = svc.RunOne(context.Background()); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("RunOne error = %v, want ErrRuntimeUnavailable", err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Queued {
		t.Fatalf("job state = %s, want queued", got.State)
	}
}

func TestRuntimeWithoutCompletionFailsAndErrorAfterCompletionReconciles(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 1)
	defer store.Close()
	runtime.skipCompletion = true
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Failed || got.Failure != "runtime completed without a structured completion" {
		t.Fatalf("missing completion job=%+v err=%v", got, err)
	}
	runtime.skipCompletion = false
	runtime.runErr = errors.New("turn stream ended after tool result")
	a.IdempotencyKey, a.Key.ReviewRoundID, a.Key.CorrelationID = "idem-2", 2, "c2"
	den.contexts[2] = Context{Key: a.Key, NextState: "source_review_ready"}
	j, _, err = svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatalf("stored completion must reconcile despite run error: %v", err)
	}
	got, err = svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Succeeded {
		t.Fatalf("after completion error job=%+v err=%v", got, err)
	}
}

func TestFinalizingStaleConflictAndResponseLoss(t *testing.T) {
	for name, outcome := range map[string]struct {
		err  error
		want State
	}{
		"stale": {ErrStaleRound, Stale}, "conflict": {ErrDenConflict, Failed}, "rejected": {ErrDenRejected, Failed},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, den, _, a := fixture(t, 1)
			defer store.Close()
			den.finalErr = outcome.err
			j, _, e := svc.Admit(context.Background(), a)
			if e != nil {
				t.Fatal(e)
			}
			_, _, e = store.Claim(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			_, e = store.PutFinalization(context.Background(), j.ID, Finalization{Key: a.Key, Reviewer: a.Reviewer, Completion: Completion{Verdict: "looks_good"}})
			if e != nil {
				t.Fatal(e)
			}
			if e = store.ReleaseFinalization(context.Background(), j.ID); e != nil {
				t.Fatal(e)
			}
			if _, e = svc.RunOne(context.Background()); e != nil {
				t.Fatal(e)
			}
			got, e := svc.Get(context.Background(), j.ID)
			if e != nil || got.State != outcome.want {
				t.Fatalf("got=%+v err=%v", got, e)
			}
		})
	}
	svc, store, den, _, a := fixture(t, 1)
	defer store.Close()
	den.fail = true
	j, _, e := svc.Admit(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = store.Claim(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.PutFinalization(context.Background(), j.ID, Finalization{Key: a.Key, Reviewer: a.Reviewer, Completion: Completion{Verdict: "looks_good"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.ReleaseFinalization(context.Background(), j.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.RunOne(context.Background()); e == nil {
		t.Fatal("response loss must remain retryable")
	}
	den.fail = false
	if _, e = svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	got, e := svc.Get(context.Background(), j.ID)
	if e != nil || got.State != Succeeded {
		t.Fatalf("retry=%+v err=%v", got, e)
	}
}

func TestFinalizationClaimAndSameTaskSerialization(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 2)
	defer store.Close()
	j, _, e := svc.Admit(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = store.Claim(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.PutFinalization(context.Background(), j.ID, Finalization{Key: a.Key, Reviewer: a.Reviewer, Completion: Completion{Verdict: "looks_good"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.ReleaseFinalization(context.Background(), j.ID); e != nil {
		t.Fatal(e)
	}
	den.started = make(chan struct{}, 1)
	den.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := svc.RunOne(context.Background()); done <- e }()
	<-den.started
	if ran, e := svc.RunOne(context.Background()); e != nil || ran {
		t.Fatalf("concurrent finalizer ran=%v err=%v", ran, e)
	}
	den.release <- struct{}{}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	den.started = nil
	den.release = nil
	a1 := a
	a1.IdempotencyKey = "task-first"
	a1.Key.ReviewRoundID = 3
	a1.Key.CorrelationID = "c3"
	a2 := a1
	a2.IdempotencyKey = "task-second"
	a2.Key.ReviewRoundID = 4
	a2.Key.CorrelationID = "c4"
	den.contexts[3] = Context{Key: a1.Key, NextState: "source_review_ready"}
	den.contexts[4] = Context{Key: a2.Key, NextState: "source_review_ready"}
	if _, _, e := svc.Admit(context.Background(), a1); e != nil {
		t.Fatal(e)
	}
	if _, _, e := svc.Admit(context.Background(), a2); e != nil {
		t.Fatal(e)
	}
	runtime.start = make(chan struct{}, 1)
	runtime.release = make(chan struct{})
	turn := make(chan error, 1)
	go func() { _, e := svc.RunOne(context.Background()); turn <- e }()
	<-runtime.start
	if ran, e := svc.RunOne(context.Background()); e != nil || ran {
		t.Fatalf("same task ran=%v err=%v", ran, e)
	}
	runtime.release <- struct{}{}
	if e := <-turn; e != nil {
		t.Fatal(e)
	}
}

func TestRuntimeCallbackOwnsFinalizationUntilTurnCompletes(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 2)
	defer store.Close()
	runtime.completion = changesRequestedCompletion()
	runtime.completed = make(chan struct{}, 1)
	runtime.afterCompletion = make(chan struct{})
	if _, _, err := svc.Admit(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ownerDone := make(chan error, 1)
	go func() { _, err := svc.RunOne(context.Background()); ownerDone <- err }()
	<-runtime.completed // PutFinalization completed, but the runtime turn has not.
	if ran, err := svc.RunOne(context.Background()); err != nil || ran {
		t.Fatalf("concurrent runner stole finalization: ran=%v err=%v", ran, err)
	}
	close(runtime.afterCompletion)
	if err := <-ownerDone; err != nil {
		t.Fatal(err)
	}
	if runtime.released != 0 {
		t.Fatalf("changes_requested worker was released: releases=%d", runtime.released)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snapshot.Retained) != 1 || snapshot.Retained[0].ProjectID != a.Key.ProjectID || snapshot.Retained[0].TaskID != a.Key.TaskID {
		t.Fatalf("changes_requested affinity was not retained: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRunningRestartAndWrongContextKey(t *testing.T) {
	svc, store, den, runtime, a := fixture(t, 1)
	defer store.Close()
	j, _, e := svc.Admit(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = store.Claim(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	got, e := svc.Get(context.Background(), j.ID)
	if e != nil || got.State != Queued {
		t.Fatalf("restart=%+v err=%v", got, e)
	}
	den.contexts[a.Key.ReviewRoundID] = Context{Key: Key{ProjectID: a.Key.ProjectID, TaskID: a.Key.TaskID, ReviewRoundID: 99, CorrelationID: a.Key.CorrelationID}, NextState: "source_review_ready"}
	if _, e = svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	got, e = svc.Get(context.Background(), j.ID)
	if e != nil || got.State != Stale || runtime.acquired != 0 {
		t.Fatalf("wrong context=%+v acquired=%d err=%v", got, runtime.acquired, e)
	}
}

func TestChangesRequestedRetainsRereviewRefreshesAndExpiryReleases(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := Key{ProjectID: "dsh", TaskID: 1, ReviewRoundID: 1, CorrelationID: "c1"}
	den := &fakeDen{contexts: map[int64]Context{1: {Key: key, NextState: "source_review_ready", Workspace: "/repo"}}, receipts: map[int64]Receipt{}}
	runtime := &fakeRuntime{completion: changesRequestedCompletion()}
	svc, err := New(store, den, runtime, "profile", WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	a := Admission{IdempotencyKey: "one", Key: key, Reviewer: "reviewer"}
	if _, _, err = svc.Admit(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.acquired != 1 || runtime.released != 0 {
		t.Fatalf("initial acquired=%d released=%d", runtime.acquired, runtime.released)
	}
	clock.Advance(time.Hour)
	a.IdempotencyKey = "two"
	a.Key.ReviewRoundID = 2
	a.Key.CorrelationID = "c2"
	den.contexts[2] = Context{Key: a.Key, NextState: "source_review_ready", Workspace: "/repo"}
	if _, replay, err := svc.Admit(context.Background(), a); err != nil || replay {
		t.Fatalf("rereview replay=%v err=%v", replay, err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.acquired != 1 {
		t.Fatalf("rereview acquired new worker %d", runtime.acquired)
	}
	snap, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snap.Retained) != 1 || snap.Retained[0].TaskID != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	clock.Advance(12 * time.Hour)
	if _, err = svc.Snapshot(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if runtime.released != 1 {
		t.Fatalf("expiry releases=%d", runtime.released)
	}
}

func TestChangedWorkspaceReleasesIdleAffinityAndAcquiresFreshWorker(t *testing.T) {
	svc, store, den, runtime, firstAdmission := fixture(t, 2)
	defer store.Close()
	runtime.completion = changesRequestedCompletion()
	den.contexts[firstAdmission.Key.ReviewRoundID] = Context{Key: firstAdmission.Key, NextState: "source_review_ready", Workspace: "/old-checkout"}
	firstAdmission.Workspace = "/old-checkout"
	if _, _, err := svc.Admit(context.Background(), firstAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}

	secondAdmission := firstAdmission
	secondAdmission.IdempotencyKey = "second-round"
	secondAdmission.Key.ReviewRoundID = 2
	secondAdmission.Key.CorrelationID = "second-correlation"
	secondAdmission.Workspace = "/old-checkout" // persisted admission is not current checkout authority.
	den.contexts[2] = Context{Key: secondAdmission.Key, NextState: "source_review_ready", Workspace: "/new-checkout"}
	if _, _, err := svc.Admit(context.Background(), secondAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.acquired != 2 || runtime.released != 1 || len(runtime.releasedWorkers) != 1 || runtime.releasedWorkers[0] != Worker(1) {
		t.Fatalf("changed checkout did not release old worker and acquire fresh: acquired=%d released=%d released_workers=%#v", runtime.acquired, runtime.released, runtime.releasedWorkers)
	}
	if got, want := runtime.workspaces, []string{"/old-checkout", "/new-checkout"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("acquire workspaces = %#v, want %#v", got, want)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snapshot.Retained) != 1 || snapshot.Retained[0].Workspace != "/new-checkout" {
		t.Fatalf("new affinity = %+v, err=%v", snapshot, err)
	}
}

func TestChangedWorkspaceDoesNotReuseOrReleaseBusyAffinity(t *testing.T) {
	svc, store, _, runtime, admission := fixture(t, 1)
	defer store.Close()
	key := admission.Key.Task()
	svc.retain(key, Worker("old-worker"), "/old-checkout", svc.clock.Now())
	svc.mu.Lock()
	svc.affinities[key].busy = true
	svc.mu.Unlock()
	if _, reused, err := svc.workerFor(context.Background(), key, "/new-checkout"); !errors.Is(err, ErrAffinityBusy) || !reused {
		t.Fatalf("changed checkout with busy affinity reused=%v err=%v", reused, err)
	}
	if runtime.acquired != 0 || runtime.released != 0 {
		t.Fatalf("busy affinity touched runtime: acquired=%d released=%d", runtime.acquired, runtime.released)
	}
}

func TestLooksGoodReleasesRatherThanRetains(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	runtime.completion = Completion{Verdict: "looks_good"}
	if _, _, err := svc.Admit(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.released != 1 {
		t.Fatalf("looks good release=%d", runtime.released)
	}
	snap, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snap.Retained) != 0 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestIdleAffinityYieldsCapacityToUnrelatedQueuedReview(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := Key{ProjectID: "dsh", TaskID: 1, ReviewRoundID: 1, CorrelationID: "first"}
	second := Key{ProjectID: "dsh", TaskID: 2, ReviewRoundID: 2, CorrelationID: "second"}
	den := &fakeDen{contexts: map[int64]Context{
		1: {Key: first, NextState: "source_review_ready"},
		2: {Key: second, NextState: "source_review_ready"},
	}, receipts: map[int64]Receipt{}}
	runtime := &fakeRuntime{capacity: 1, completion: changesRequestedCompletion()}
	svc, err := New(store, den, runtime, "profile", WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = svc.Admit(context.Background(), Admission{IdempotencyKey: "first", Key: first, Reviewer: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.completion = Completion{Verdict: "looks_good"}
	job, _, err := svc.Admit(context.Background(), Admission{IdempotencyKey: "second", Key: second, Reviewer: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), job.ID)
	if err != nil || got.State != Succeeded {
		t.Fatalf("unrelated review = %+v, err=%v", got, err)
	}
	if runtime.acquired != 2 || runtime.released != 2 {
		t.Fatalf("acquired=%d released=%d", runtime.acquired, runtime.released)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snapshot.Retained) != 0 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRetainedTaskClaimBeatsOlderNewTask(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	store, e := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	first := Key{ProjectID: "dsh", TaskID: 1, ReviewRoundID: 1, CorrelationID: "a"}
	den := &fakeDen{contexts: map[int64]Context{1: {Key: first, NextState: "source_review_ready"}}, receipts: map[int64]Receipt{}}
	runtime := &fakeRuntime{completion: changesRequestedCompletion()}
	svc, e := New(store, den, runtime, "profile", WithClock(clock))
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = svc.Admit(context.Background(), Admission{IdempotencyKey: "first", Key: first, Reviewer: "r"}); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	newTask := Key{ProjectID: "dsh", TaskID: 2, ReviewRoundID: 2, CorrelationID: "new"}
	den.contexts[2] = Context{Key: newTask, NextState: "source_review_ready"}
	older, _, e := svc.Admit(context.Background(), Admission{IdempotencyKey: "older", Key: newTask, Reviewer: "r"})
	if e != nil {
		t.Fatal(e)
	}
	rereview := first
	rereview.ReviewRoundID = 3
	rereview.CorrelationID = "again"
	den.contexts[3] = Context{Key: rereview, NextState: "source_review_ready"}
	newer, _, e := svc.Admit(context.Background(), Admission{IdempotencyKey: "rereview", Key: rereview, Reviewer: "r"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	got, e := svc.Get(context.Background(), newer.ID)
	if e != nil || got.State != Succeeded {
		t.Fatalf("rereview=%+v err=%v", got, e)
	}
	got, e = svc.Get(context.Background(), older.ID)
	if e != nil || got.State != Queued {
		t.Fatalf("older new task=%+v err=%v", got, e)
	}
	if runtime.acquired != 1 {
		t.Fatalf("rereview did not reuse worker: acquired=%d", runtime.acquired)
	}
}

func TestReplayDoesNotRefreshAffinityDeadlineOrRestartPersistIt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	store, e := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	key := Key{ProjectID: "dsh", TaskID: 1, ReviewRoundID: 1, CorrelationID: "a"}
	den := &fakeDen{contexts: map[int64]Context{1: {Key: key, NextState: "source_review_ready", Workspace: "/old-checkout"}}, receipts: map[int64]Receipt{}}
	runtime := &fakeRuntime{completion: changesRequestedCompletion()}
	svc, e := New(store, den, runtime, "profile", WithClock(clock))
	if e != nil {
		t.Fatal(e)
	}
	a := Admission{IdempotencyKey: "one", Key: key, Reviewer: "r"}
	if _, _, e = svc.Admit(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	before, _ := svc.Snapshot(context.Background(), 5)
	clock.Advance(time.Hour)
	if _, replayed, e := svc.Admit(context.Background(), a); e != nil || !replayed {
		t.Fatalf("replay=%v err=%v", replayed, e)
	}
	after, _ := svc.Snapshot(context.Background(), 5)
	if !after.Retained[0].ExpiresAt.Equal(before.Retained[0].ExpiresAt) {
		t.Fatal("replay refreshed TTL")
	}
	fresh, e := New(store, den, runtime, "profile", WithClock(clock))
	if e != nil {
		t.Fatal(e)
	}
	snap, e := fresh.Snapshot(context.Background(), 5)
	if e != nil || len(snap.Retained) != 0 {
		t.Fatalf("restart affinity=%+v err=%v", snap, e)
	}
	// An affinity owns an opaque in-process worker and is deliberately not
	// durable. After a restart, the next round must acquire a new worker even
	// when its task key is unchanged (and here its checkout also changed).
	next := key
	next.ReviewRoundID = 2
	next.CorrelationID = "after-restart"
	den.contexts[next.ReviewRoundID] = Context{Key: next, NextState: "source_review_ready", Workspace: "/new-checkout"}
	if _, _, e = fresh.Admit(context.Background(), Admission{IdempotencyKey: "after-restart", Key: next, Reviewer: "r", Workspace: "/old-checkout"}); e != nil {
		t.Fatal(e)
	}
	if _, e = fresh.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	if runtime.acquired != 2 || len(runtime.workspaces) != 2 || runtime.workspaces[1] != "/new-checkout" {
		t.Fatalf("restart reused worker or stale checkout: acquired=%d workspaces=%#v", runtime.acquired, runtime.workspaces)
	}
}

func TestManualAffinityReleaseRejectsBusyThenReleasesIdle(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	runtime.completion = changesRequestedCompletion()
	if _, _, e := svc.Admit(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.RunOne(context.Background()); e != nil {
		t.Fatal(e)
	}
	key := a.Key.Task()
	svc.mu.Lock()
	svc.affinities[key].busy = true
	svc.mu.Unlock()
	if e := svc.ReleaseAffinity(context.Background(), key); !errors.Is(e, ErrAffinityBusy) {
		t.Fatalf("busy release=%v", e)
	}
	svc.mu.Lock()
	svc.affinities[key].busy = false
	svc.mu.Unlock()
	if e := svc.ReleaseAffinity(context.Background(), key); e != nil {
		t.Fatal(e)
	}
	if runtime.released != 1 {
		t.Fatalf("released=%d", runtime.released)
	}
}

func TestRetryFailedRequeuesExactJobAndDiscardsAffinity(t *testing.T) {
	svc, store, _, runtime, admission := fixture(t, 1)
	defer store.Close()
	job, _, err := svc.Admit(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutFinalization(context.Background(), job.ID, Finalization{Key: admission.Key, Reviewer: admission.Reviewer, Completion: Completion{Verdict: "looks_good"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Fail(context.Background(), job.ID, Failed, "reviewer turn completed without complete_review"); err != nil {
		t.Fatal(err)
	}
	poisoned := Worker("failed-reviewer")
	svc.retain(admission.Key.Task(), poisoned, "/repo", svc.clock.Now())

	retried, err := svc.RetryFailed(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != job.ID || retried.State != Queued || retried.Failure != "" || retried.Finalization != nil || retried.Receipt != nil {
		t.Fatalf("retried job = %+v", retried)
	}
	if runtime.released != 1 || len(runtime.releasedWorkers) != 1 || runtime.releasedWorkers[0] != poisoned {
		t.Fatalf("released workers = %#v", runtime.releasedWorkers)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snapshot.Retained) != 0 {
		t.Fatalf("retry left stale affinity: snapshot=%+v err=%v", snapshot, err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := svc.Get(context.Background(), job.ID)
	if err != nil || completed.State != Succeeded || runtime.acquired != 1 {
		t.Fatalf("fresh retry result = %+v acquired=%d err=%v", completed, runtime.acquired, err)
	}
}

func TestRetryFailedRejectsBusyAffinityAndDoubleRetry(t *testing.T) {
	svc, store, _, runtime, admission := fixture(t, 1)
	defer store.Close()
	job, _, err := svc.Admit(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Fail(context.Background(), job.ID, Failed, "failed"); err != nil {
		t.Fatal(err)
	}
	svc.retain(admission.Key.Task(), Worker("busy-reviewer"), "/repo", svc.clock.Now())
	svc.mu.Lock()
	svc.affinities[admission.Key.Task()].busy = true
	svc.mu.Unlock()
	if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrAffinityBusy) {
		t.Fatalf("busy retry error = %v", err)
	}
	stillFailed, err := svc.Get(context.Background(), job.ID)
	if err != nil || stillFailed.State != Failed || runtime.released != 0 {
		t.Fatalf("busy retry mutated job=%+v released=%d err=%v", stillFailed, runtime.released, err)
	}
	svc.mu.Lock()
	svc.affinities[admission.Key.Task()].busy = false
	svc.mu.Unlock()
	if _, err = svc.RetryFailed(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrTooLate) {
		t.Fatalf("double retry error = %v", err)
	}
	if _, err = svc.RetryFailed(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing retry error = %v", err)
	}
}

func TestRetryFailedRejectsNonFailedJobs(t *testing.T) {
	for _, target := range []State{Queued, Running, Finalizing, Succeeded} {
		t.Run(string(target), func(t *testing.T) {
			svc, store, _, _, admission := fixture(t, 1)
			defer store.Close()
			job, _, err := svc.Admit(context.Background(), admission)
			if err != nil {
				t.Fatal(err)
			}
			if target == Queued {
				if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrTooLate) {
					t.Fatalf("queued retry error = %v", err)
				}
				return
			}
			if _, _, err = store.Claim(context.Background()); err != nil {
				t.Fatal(err)
			}
			if target == Running {
				if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrTooLate) {
					t.Fatalf("running retry error = %v", err)
				}
				return
			}
			finalization := Finalization{Key: admission.Key, Reviewer: admission.Reviewer, Completion: Completion{Verdict: "looks_good"}}
			if _, err = store.PutFinalization(context.Background(), job.ID, finalization); err != nil {
				t.Fatal(err)
			}
			if target == Finalizing {
				if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrTooLate) {
					t.Fatalf("finalizing retry error = %v", err)
				}
				return
			}
			if _, err = store.Complete(context.Background(), job.ID, Receipt{Verdict: "looks_good"}); err != nil {
				t.Fatal(err)
			}
			if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, ErrTooLate) {
				t.Fatalf("succeeded retry error = %v", err)
			}
		})
	}
}

func TestRetryFailedLeavesJobAndAffinityIntactWhenReleaseFails(t *testing.T) {
	svc, store, _, runtime, admission := fixture(t, 1)
	defer store.Close()
	job, _, err := svc.Admit(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Fail(context.Background(), job.ID, Failed, "failed"); err != nil {
		t.Fatal(err)
	}
	runtime.releaseErr = errors.New("runtime release failed")
	svc.retain(admission.Key.Task(), Worker("failed-reviewer"), "/repo", svc.clock.Now())
	if _, err = svc.RetryFailed(context.Background(), job.ID); !errors.Is(err, runtime.releaseErr) {
		t.Fatalf("retry error = %v", err)
	}
	stillFailed, err := svc.Get(context.Background(), job.ID)
	if err != nil || stillFailed.State != Failed {
		t.Fatalf("release failure changed job=%+v err=%v", stillFailed, err)
	}
	snapshot, err := svc.Snapshot(context.Background(), 5)
	if err != nil || len(snapshot.Retained) != 1 {
		t.Fatalf("release failure lost affinity: snapshot=%+v err=%v", snapshot, err)
	}
}
