package review

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

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
	mu             sync.Mutex
	running        int
	max            int
	start          chan struct{}
	release        chan struct{}
	completion     Completion
	acquired       int
	skipCompletion bool
	runErr         error
}

func (r *fakeRuntime) Acquire(context.Context, TaskKey, string, string) (Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acquired++
	return r.acquired, nil
}
func (r *fakeRuntime) Run(ctx context.Context, _ Worker, _ string, complete func(Completion) error) error {
	r.mu.Lock()
	r.running++
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
	if r.runErr != nil {
		e = r.runErr
	}
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return e
}
func (*fakeRuntime) Release(context.Context, Worker) error { return nil }
func (*fakeRuntime) Close() error                          { return nil }

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
	svc, e := New(store, den, runtime, "review profile")
	if e != nil {
		t.Fatal(e)
	}
	return svc, store, den, runtime, Admission{IdempotencyKey: "idem-1", Key: key, Reviewer: "reviewer", Workspace: "/repo"}
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
		"stale": {ErrStaleRound, Stale}, "conflict": {ErrDenConflict, Failed},
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
