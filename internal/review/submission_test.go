package review

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

const submissionTestSHA = "0123456789abcdef0123456789abcdef01234567"

type submissionDen struct {
	mu sync.Mutex

	roundID     int64
	watchGate   GateEvidence
	readGates   []GateEvidence
	contextNext string

	requestCalls int
	watchCalls   int
	readCalls    int
	contextCalls int
	lastRequest  SubmissionRequest

	requestErr error
	watchErr   error
	readErr    error
	contextErr error
}

func (d *submissionDen) GetReviewContext(_ context.Context, key Key) (Context, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.contextCalls++
	if d.contextErr != nil {
		return Context{}, d.contextErr
	}
	next := d.contextNext
	if next == "" {
		next = "source_review_ready"
	}
	return Context{Key: key, NextState: next, Workspace: "/repo"}, nil
}

func (*submissionDen) FinalizeReview(context.Context, Finalization) (Receipt, error) {
	return Receipt{}, nil
}

func (d *submissionDen) RequestReview(_ context.Context, request SubmissionRequest) (ReviewRoundRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requestCalls++
	d.lastRequest = request
	if d.requestErr != nil {
		return ReviewRoundRef{}, d.requestErr
	}
	if d.roundID == 0 {
		d.roundID = 7
	}
	return ReviewRoundRef{ID: d.roundID, ProjectID: request.ProjectID, TaskID: request.TaskID}, nil
}

func (d *submissionDen) WatchGitHubChecks(_ context.Context, _ GateRequest) (GateEvidence, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watchCalls++
	if d.watchErr != nil {
		return GateEvidence{}, d.watchErr
	}
	return d.watchGate, nil
}

func (d *submissionDen) GetGitHubCheckGate(_ context.Context, _ GateRequest) (GateEvidence, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readCalls++
	if d.readErr != nil {
		return GateEvidence{}, d.readErr
	}
	if len(d.readGates) > 0 {
		gate := d.readGates[0]
		d.readGates = d.readGates[1:]
		return gate, nil
	}
	return d.watchGate, nil
}

func submissionRequestForTest() SubmissionRequest {
	return SubmissionRequest{
		ProjectID:      "dsh-crew",
		TaskID:         7416,
		Repository:     "owner/repo",
		CommitSHA:      submissionTestSHA,
		Ref:            "main",
		RequiredChecks: []string{"lint", "go test ./...", "lint"},
		BaseCommit:     "fedcba9876543210fedcba9876543210fedcba98",
		ReviewSummary:  "Implemented the managed review handoff.",
		Reviewer:       "@reviewer",
	}
}

func submissionFixture(t *testing.T, den DenReviewClient) (*Service, *SQLiteStore, *fakeRuntime) {
	t.Helper()
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	service, err := New(store, den, runtime, "review profile")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return service, store, runtime
}

func TestSubmitTaskForReviewPendingThenPassReplaysAndAdmitsOneJob(t *testing.T) {
	den := &submissionDen{
		watchGate: GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "pending", Handle: "41"},
		readGates: []GateEvidence{{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "passed", Handle: "41", TerminalReason: "checks_passed"}},
	}
	service, store, _ := submissionFixture(t, den)
	defer store.Close()
	request := submissionRequestForTest()

	first, replayed, err := service.SubmitTaskForReview(context.Background(), request)
	if err != nil || replayed || !first.OK || !first.Retryable || first.Phase != SubmissionGatePending || first.ReviewRoundID != 7 {
		t.Fatalf("pending receipt=%+v replayed=%v err=%v", first, replayed, err)
	}
	if den.requestCalls != 1 || den.watchCalls != 1 || den.readCalls != 0 {
		t.Fatalf("initial Den calls request=%d watch=%d read=%d", den.requestCalls, den.watchCalls, den.readCalls)
	}
	if den.lastRequest.ReviewSummary != request.ReviewSummary || den.lastRequest.CommitSHA != request.CommitSHA {
		t.Fatalf("request passed to Den=%+v", den.lastRequest)
	}

	second, replayed, err := service.SubmitTaskForReview(context.Background(), request)
	if err != nil || !replayed || !second.OK || second.Retryable || second.Phase != SubmissionJobAdmitted || second.JobID == "" {
		t.Fatalf("passed receipt=%+v replayed=%v err=%v", second, replayed, err)
	}
	if den.requestCalls != 1 || den.watchCalls != 1 || den.readCalls != 1 || den.contextCalls != 1 {
		t.Fatalf("retry Den calls request=%d watch=%d read=%d context=%d", den.requestCalls, den.watchCalls, den.readCalls, den.contextCalls)
	}

	third, replayed, err := service.SubmitTaskForReview(context.Background(), request)
	if err != nil || !replayed || third.SubmissionID != second.SubmissionID || third.JobID != second.JobID {
		t.Fatalf("terminal replay=%+v replayed=%v err=%v", third, replayed, err)
	}
	var jobs int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM crew_review_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("job count=%d, want one job for one review round", jobs)
	}

	request.ReviewSummary = "Changed facts must not silently reuse the submission."
	if _, _, err := service.SubmitTaskForReview(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed submission error=%v, want conflict", err)
	}
}

func TestSubmitTaskForReviewGateFailureIsTerminal(t *testing.T) {
	den := &submissionDen{watchGate: GateEvidence{
		Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "failed", Handle: "42",
		TerminalReason: "required_checks_missing", FailureSummary: "lint was not observed",
	}}
	service, store, runtime := submissionFixture(t, den)
	defer store.Close()
	receipt, replayed, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || replayed || receipt.OK || receipt.Phase != SubmissionGateFailed || receipt.ErrorCode != "github_gate_failed" || receipt.GateTerminalReason != "required_checks_missing" {
		t.Fatalf("failure receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
	if receipt.Error == "" || runtime.acquired != 0 || den.contextCalls != 0 {
		t.Fatalf("failure receipt/error or runtime state receipt=%+v acquired=%d contexts=%d", receipt, runtime.acquired, den.contextCalls)
	}
	replayedReceipt, replayed, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || !replayed || replayedReceipt.SubmissionID != receipt.SubmissionID || replayedReceipt.ErrorCode != "github_gate_failed" || replayedReceipt.Retryable || den.watchCalls != 1 {
		t.Fatalf("terminal failure replay=%+v replayed=%v calls=%d err=%v", replayedReceipt, replayed, den.watchCalls, err)
	}
}

func TestSubmitTaskForReviewUnavailableBackendIsActionableAndRetryable(t *testing.T) {
	service, store, _ := submissionFixture(t, &fakeDen{contexts: map[int64]Context{}, receipts: map[int64]Receipt{}})
	defer store.Close()
	receipt, replayed, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || replayed || receipt.OK || !receipt.Retryable || receipt.ErrorCode != "review_submission_backend_unavailable" || receipt.Phase != SubmissionUnavailable {
		t.Fatalf("unavailable receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
}

func TestSubmitTaskForReviewStaleRoundDoesNotAdmitJob(t *testing.T) {
	den := &submissionDen{
		watchGate:   GateEvidence{Status: "passed", Handle: "43", TerminalReason: "checks_passed"},
		contextNext: "review_finalized",
	}
	service, store, runtime := submissionFixture(t, den)
	defer store.Close()
	receipt, _, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || receipt.OK || receipt.Phase != SubmissionStale || receipt.ErrorCode != "stale_review_round" {
		t.Fatalf("stale receipt=%+v err=%v", receipt, err)
	}
	if runtime.acquired != 0 {
		t.Fatalf("stale submission acquired runtime=%d", runtime.acquired)
	}
	replayedReceipt, replayed, err := service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || !replayed || replayedReceipt.ErrorCode != "stale_review_round" || replayedReceipt.Retryable || replayedReceipt.Summary != receipt.Summary {
		t.Fatalf("stale replay receipt=%+v replayed=%v err=%v", replayedReceipt, replayed, err)
	}
}

func TestAdmitRoundConcurrentDistinctKeysShareOneRoundJob(t *testing.T) {
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "review.db"), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 7, CorrelationID: "submission"}
	base := Admission{Key: key, Reviewer: "@reviewer", Workspace: "/repo", Branch: "main", Gate: GateEvidence{CommitSHA: submissionTestSHA, Status: "passed", Handle: "41"}}
	start := make(chan struct{})
	type result struct {
		job      Job
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		admission := base
		admission.IdempotencyKey = "distinct-submission-" + string(rune('a'+i))
		go func() {
			<-start
			job, replayed, callErr := store.AdmitRound(context.Background(), admission)
			results <- result{job: job, replayed: replayed, err: callErr}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent admission errors: first=%v second=%v", first.err, second.err)
	}
	if first.job.ID == "" || first.job.ID != second.job.ID || first.replayed == second.replayed {
		t.Fatalf("concurrent admissions first=%+v second=%+v, want one durable winner and one replay", first, second)
	}
	var jobs int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM crew_review_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("job count=%d, want one job for one review round", jobs)
	}
}

func TestSubmitTaskForReviewRejectsInvalidExactSHA(t *testing.T) {
	den := &submissionDen{}
	service, store, _ := submissionFixture(t, den)
	defer store.Close()
	request := submissionRequestForTest()
	request.CommitSHA = "latest"
	if _, _, err := service.SubmitTaskForReview(context.Background(), request); err == nil {
		t.Fatal("short commit SHA was accepted")
	}
	if den.requestCalls != 0 {
		t.Fatalf("invalid request reached Den request_review: %d", den.requestCalls)
	}
}
