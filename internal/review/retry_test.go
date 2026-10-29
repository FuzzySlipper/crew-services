package review

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) count(substring string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
}

func TestReviewDeadlineRequeuesOnceThenFails(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	logs := &logLines{}
	WithReviewDeadline(30 * time.Millisecond)(svc)
	WithLogger(logs.logf)(svc)
	// The fake run blocks until its context ends, like a hung turn.
	runtime.release = make(chan struct{})
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err = svc.RunOne(context.Background()); !errors.Is(err, ErrReviewDeadline) {
		t.Fatalf("first attempt error = %v, want ErrReviewDeadline", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("deadline freed the lane after %s", took)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Queued {
		t.Fatalf("after first deadline job=%+v err=%v, want queued", got, err)
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatalf("second attempt error = %v", err)
	}
	got, err = svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Failed || !got.InfrastructureFailure() || !strings.Contains(got.Failure, "review_deadline: 2 attempts") {
		t.Fatalf("after second deadline job=%+v err=%v, want a rerunnable failure", got, err)
	}
	if runtime.acquired != 2 || runtime.released != 2 {
		t.Fatalf("acquired=%d released=%d, want one worker per attempt, all released", runtime.acquired, runtime.released)
	}
	if logs.count("claimed") != 2 || logs.count("requeued after review_deadline attempt 1/2") != 1 || logs.count("failed after") != 1 {
		t.Fatalf("log lines = %q", logs.lines)
	}
}

func TestOverloadedModelRequeuesWithBackoffThenFails(t *testing.T) {
	for _, message := range []string{
		"Codex turn failed without complete_review: exceeded retry limit, last status: 429 Too Many Requests",
		"Codex turn failed without complete_review: Selected model is at capacity. Please try a different model.",
	} {
		t.Run(message, func(t *testing.T) {
			svc, store, _, runtime, a := fixture(t, 1)
			defer store.Close()
			clock := svc.clock.(*testClock)
			runtime.skipCompletion = true
			runtime.runErr = MarkRetryable(errors.New(message), ErrRuntimeOverloaded)
			j, _, err := svc.Admit(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt < overloadRetry.attempts; attempt++ {
				if _, err = svc.RunOne(context.Background()); !errors.Is(err, ErrRuntimeOverloaded) {
					t.Fatalf("attempt %d error = %v", attempt, err)
				}
				delay := overloadRetry.delay(attempt)
				if want := time.Minute << (attempt - 1); delay != want {
					t.Fatalf("attempt %d backoff = %s, want %s", attempt, delay, want)
				}
				clock.Advance(delay - time.Second)
				if ran, err := svc.RunOne(context.Background()); ran || err != nil {
					t.Fatalf("attempt %d ran=%v err=%v before its backoff passed", attempt, ran, err)
				}
				clock.Advance(time.Second)
			}
			if _, err = svc.RunOne(context.Background()); err != nil {
				t.Fatalf("last attempt error = %v", err)
			}
			got, err := svc.Get(context.Background(), j.ID)
			if err != nil || got.State != Failed || !got.InfrastructureFailure() || !strings.Contains(got.Failure, "model_overloaded: 4 attempts") || !strings.Contains(got.Failure, message) {
				t.Fatalf("exhausted job=%+v err=%v", got, err)
			}
			if runtime.acquired != overloadRetry.attempts {
				t.Fatalf("acquired %d workers, want %d", runtime.acquired, overloadRetry.attempts)
			}
		})
	}
}

func TestMissingCompletionRequeuesThenFails(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	clock := svc.clock.(*testClock)
	runtime.skipCompletion = true
	runtime.runErr = MarkRetryable(errors.New("Codex turn completed without complete_review"), ErrNoCompletion)
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < incompleteRetry.attempts; attempt++ {
		if _, err = svc.RunOne(context.Background()); !errors.Is(err, ErrNoCompletion) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
		got, _ := svc.Get(context.Background(), j.ID)
		if got.State != Queued {
			t.Fatalf("attempt %d state = %s, want queued", attempt, got.State)
		}
		clock.Advance(incompleteRetry.delay(attempt))
	}
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Failed || !got.InfrastructureFailure() || !strings.Contains(got.Failure, "no_completion: 3 attempts") {
		t.Fatalf("exhausted job=%+v err=%v", got, err)
	}
}

func TestMissingCompletionThenSuccessCompletes(t *testing.T) {
	svc, store, _, runtime, a := fixture(t, 1)
	defer store.Close()
	clock := svc.clock.(*testClock)
	runtime.skipCompletion = true
	j, _, err := svc.Admit(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RunOne(context.Background()); !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("first attempt error = %v", err)
	}
	runtime.skipCompletion = false
	clock.Advance(incompleteRetry.delay(1))
	if _, err = svc.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), j.ID)
	if err != nil || got.State != Succeeded || len(svc.infraAttempts) != 0 {
		t.Fatalf("job=%+v err=%v counts=%v", got, err, svc.infraAttempts)
	}
}

// notingDen is a submission Den that records task-thread notes.
type notingDen struct {
	*submissionDen
	mu    sync.Mutex
	notes []string
}

func (d *notingDen) PostTaskNote(_ context.Context, key TaskKey, content string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notes = append(d.notes, fmt.Sprintf("%s/%d: %s", key.ProjectID, key.TaskID, content))
	return nil
}

func TestFinalFailureNotifiesOnceAndResubmitReruns(t *testing.T) {
	den := &notingDen{submissionDen: &submissionDen{watchGate: GateEvidence{Status: "passed", Handle: "43", TerminalReason: "checks_passed"}}}
	clock := &testClock{now: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := &fakeRuntime{completion: Completion{Verdict: "looks_good"}, skipCompletion: true}
	service, err := New(store, den, runtime, "review profile", WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	request := submissionRequestForTest()
	request.NotifyAddress = "claude:session-7"
	first, _, err := service.SubmitTaskForReview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= incompleteRetry.attempts; attempt++ {
		clock.Advance(time.Hour)
		_, _ = service.RunOne(context.Background())
	}
	failed, err := service.Get(context.Background(), first.JobID)
	if err != nil || failed.State != Failed || !failed.InfrastructureFailure() {
		t.Fatalf("job before resubmit=%+v err=%v", failed, err)
	}
	notices, err := store.ListSubmissionNotices(context.Background(), first.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	var reviewFailed []Notice
	for _, notice := range notices {
		if notice.Kind == NoticeReviewFailed {
			reviewFailed = append(reviewFailed, notice)
		}
	}
	if len(reviewFailed) != 1 {
		t.Fatalf("review_failed notices = %+v, want exactly one", notices)
	}
	notice := reviewFailed[0]
	if notice.Recipient != "claude:session-7" || notice.State != NoticePending ||
		!strings.Contains(notice.Body, "task #7416") || !strings.Contains(notice.Body, "without a structured completion") ||
		!strings.Contains(notice.Body, "a plain resubmit re-runs this review") || !strings.Contains(notice.Body, submissionTestSHA) {
		t.Fatalf("notice = %+v", notice)
	}
	if len(den.notes) != 1 || !strings.Contains(den.notes[0], "dsh-crew/7416: crew-review: the review of task #7416") {
		t.Fatalf("Den notes = %q, want one on the task thread", den.notes)
	}
	// Failing the job again is a no-op and reports nothing more.
	if err := service.terminal(context.Background(), failed, Failed, "again"); err != nil {
		t.Fatal(err)
	}
	if notices, _ := store.ListSubmissionNotices(context.Background(), first.SubmissionID); len(notices) != len(reviewFailed) || len(den.notes) != 1 {
		t.Fatalf("second fail reported again: notices=%d notes=%d", len(notices), len(den.notes))
	}

	again, replayed, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || !replayed || again.JobID != first.JobID {
		t.Fatalf("resubmission=%+v replayed=%v err=%v", again, replayed, err)
	}
	runtime.skipCompletion = false
	clock.Advance(time.Minute)
	if ran, err := service.RunOne(context.Background()); err != nil || !ran {
		t.Fatalf("rerun ran=%v err=%v", ran, err)
	}
	done, err := service.Get(context.Background(), first.JobID)
	if err != nil || done.State != Succeeded {
		t.Fatalf("rerun job=%+v err=%v", done, err)
	}
}

func TestFailureWithoutNotifyAddressStillPostsDenNote(t *testing.T) {
	den := &notingDen{submissionDen: &submissionDen{watchGate: GateEvidence{Status: "passed", Handle: "43", TerminalReason: "checks_passed"}}}
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := &fakeRuntime{skipCompletion: true, runErr: errors.New("noninteractive review cannot satisfy Codex interaction")}
	service, err := New(store, den, runtime, "review profile")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	notices, err := store.ListSubmissionNotices(context.Background(), first.SubmissionID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("notices = %+v err=%v, want none without a notify address", notices, err)
	}
	if len(den.notes) != 1 || !strings.Contains(den.notes[0], "does not re-run this kind of failure") || !strings.Contains(den.notes[0], "/v1/review-jobs/"+first.JobID+"/retry") {
		t.Fatalf("Den notes = %q", den.notes)
	}
}
