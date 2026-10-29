package review

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// notingDen is a submission Den that also posts task-thread notes.
type notingDen struct {
	*submissionDen
	noteMu  sync.Mutex
	notes   []string
	noteErr error
}

func (d *notingDen) PostTaskNote(_ context.Context, key TaskKey, content string) error {
	d.noteMu.Lock()
	defer d.noteMu.Unlock()
	if d.noteErr != nil {
		return d.noteErr
	}
	d.notes = append(d.notes, key.ProjectID+"#"+strconv.FormatInt(key.TaskID, 10)+": "+content)
	return nil
}

func (d *notingDen) postedNotes() []string {
	d.noteMu.Lock()
	defer d.noteMu.Unlock()
	return append([]string(nil), d.notes...)
}

func pendingGate(attempt int64) GateEvidence {
	return GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "pending", Handle: "42", Attempt: attempt}
}

func timedOutGate(attempt int64) GateEvidence {
	return GateEvidence{
		Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "timed_out", Handle: "42", Attempt: attempt,
		TerminalReason: "github_checks_timeout",
		FailedChecks:   []GateCheck{{Name: "verify", Conclusion: "in_progress", URL: "https://github.com/owner/repo/actions/runs/1/job/2"}},
	}
}

func gateTimeoutFixture(t *testing.T, den *notingDen) (*Service, *SQLiteStore, *fakeFabric, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)}
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), clock, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fabric := newFakeFabric()
	notifier, err := NewFabricNotifier(fabric, clock, FabricNotifierConfig{InstanceID: "crew-review-test"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(store, den, &fakeRuntime{}, "review profile", WithClock(clock), WithNotifier(notifier))
	if err != nil {
		t.Fatal(err)
	}
	return service, store, fabric, clock
}

func TestFirstGateTimeoutReRegistersOnceWithoutNotice(t *testing.T) {
	ctx := context.Background()
	den := &notingDen{submissionDen: titledDen(&submissionDen{watchGate: pendingGate(1), readGates: []GateEvidence{timedOutGate(1)}})}
	service, store, fabric, _ := gateTimeoutFixture(t, den)
	first, _, err := service.SubmitTaskForReview(ctx, notifyingRequest())
	if err != nil || first.Phase != SubmissionGatePending || den.watchCalls != 1 {
		t.Fatalf("first=%+v err=%v watches=%d", first, err, den.watchCalls)
	}

	// The background advance sees the first timeout and re-registers the
	// gate, which Den reopens as attempt 2, instead of failing it.
	den.mu.Lock()
	den.watchGate = pendingGate(2)
	den.mu.Unlock()
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSubmission(ctx, first.SubmissionID)
	if err != nil || record.Phase != SubmissionGatePending || record.GateAutoRechecks != 1 || record.Gate.Attempt != 2 || den.watchCalls != 2 {
		t.Fatalf("after first timeout record=%+v err=%v watches=%d", record, err, den.watchCalls)
	}
	if notices, _ := store.ListSubmissionNotices(ctx, first.SubmissionID); len(notices) != 0 {
		t.Fatalf("first timeout produced notices %+v", notices)
	}

	// The reopened gate passes: the reviewer is admitted without a resubmit.
	den.mu.Lock()
	den.readGates = []GateEvidence{{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "passed", Handle: "42", Attempt: 2, TerminalReason: "checks_passed"}}
	den.mu.Unlock()
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	record, err = store.GetSubmission(ctx, first.SubmissionID)
	if err != nil || record.Phase != SubmissionJobAdmitted || record.JobID == "" || den.watchCalls != 2 {
		t.Fatalf("after pass record=%+v err=%v watches=%d", record, err, den.watchCalls)
	}
	if _, err := service.SendNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if accepted := fabric.acceptedOperations(); len(accepted) != 0 || len(den.postedNotes()) != 0 {
		t.Fatalf("passing retry sent notices %v notes %v", accepted, den.postedNotes())
	}
}

func TestSecondGateTimeoutReportsOnceToSubmitterAndTaskThread(t *testing.T) {
	ctx := context.Background()
	den := &notingDen{submissionDen: titledDen(&submissionDen{watchGate: pendingGate(1), readGates: []GateEvidence{timedOutGate(1)}})}
	service, store, fabric, _ := gateTimeoutFixture(t, den)
	first, _, err := service.SubmitTaskForReview(ctx, notifyingRequest())
	if err != nil {
		t.Fatal(err)
	}
	den.mu.Lock()
	den.watchGate = pendingGate(2)
	den.mu.Unlock()
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}

	// The reopened gate times out too: no second automatic re-register.
	den.mu.Lock()
	den.readGates = []GateEvidence{timedOutGate(2)}
	den.mu.Unlock()
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSubmission(ctx, first.SubmissionID)
	if err != nil || record.Phase != SubmissionGateFailed || record.GateAutoRechecks != 1 || den.watchCalls != 2 {
		t.Fatalf("after second timeout record=%+v err=%v watches=%d", record, err, den.watchCalls)
	}
	// A terminal submission is not advanced again, so nothing duplicates.
	if examined, err := service.AdvanceSubmissions(ctx); err != nil || examined {
		t.Fatalf("gate_failed submission re-advanced: examined=%v err=%v", examined, err)
	}
	for range 2 {
		if _, err := service.SendNotices(ctx); err != nil {
			t.Fatal(err)
		}
	}

	prefix := "submission/" + first.SubmissionID + "/gate_timed_out/42.2"
	accepted := fabric.acceptedOperations()
	if len(accepted) != 1 || accepted[0] != prefix {
		t.Fatalf("fabric accepted %v, want only %s", accepted, prefix)
	}
	notes := den.postedNotes()
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "dsh-crew#7416: ") || !strings.Contains(notes[0], "timed out") ||
		!strings.Contains(notes[0], "verify (in_progress)") || !strings.Contains(notes[0], "re-registered this gate once") {
		t.Fatalf("task notes=%q", notes)
	}
	notices, err := store.ListSubmissionNotices(ctx, first.SubmissionID)
	if err != nil || len(notices) != 2 {
		t.Fatalf("outbox=%+v err=%v", notices, err)
	}
	for _, notice := range notices {
		if notice.Kind != NoticeGateTimedOut || notice.State != NoticeSent {
			t.Fatalf("notice %+v", notice)
		}
	}
}

func TestFinalGateTimeoutWithoutNotifyAddressStillNotesTheTask(t *testing.T) {
	ctx := context.Background()
	den := &notingDen{submissionDen: titledDen(&submissionDen{watchGate: pendingGate(1), readGates: []GateEvidence{timedOutGate(1)}})}
	service, store, fabric, clock := gateTimeoutFixture(t, den)
	first, _, err := service.SubmitTaskForReview(ctx, submissionRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	// The re-registration itself reports the gate timed out again.
	den.mu.Lock()
	den.watchGate = timedOutGate(2)
	den.mu.Unlock()
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSubmission(ctx, first.SubmissionID)
	if err != nil || record.Phase != SubmissionGateFailed || record.GateAutoRechecks != 1 {
		t.Fatalf("record=%+v err=%v", record, err)
	}

	// Den is briefly unable to take the note; it is retried with backoff.
	den.noteMu.Lock()
	den.noteErr = errors.New("Den unavailable")
	den.noteMu.Unlock()
	if _, err := service.SendNotices(ctx); err == nil {
		t.Fatal("deferred task note was not reported")
	}
	den.noteMu.Lock()
	den.noteErr = nil
	den.noteMu.Unlock()
	clock.Advance(noticeRetryBase)
	if _, err := service.SendNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if notes := den.postedNotes(); len(notes) != 1 || len(fabric.acceptedOperations()) != 0 {
		t.Fatalf("notes=%q fabric=%v", notes, fabric.acceptedOperations())
	}
}

func TestNotifyAddressCannotTargetDenTaskNotes(t *testing.T) {
	if _, err := normalizeNotifyAddress("den-task:dsh-crew/1"); err == nil {
		t.Fatal("den-task notify address accepted")
	}
}
