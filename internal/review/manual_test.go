package review

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type manualDen struct {
	task             TaskContext
	roundID          int64
	manualCalls      int
	requestCalls     int
	lastRequest      ManualReviewRequest
	contextCalls     int
	contextNext      string
	contextMaterial  []byte
	requestReviewErr error
}

func (d *manualDen) GetTaskContext(context.Context, TaskKey) (TaskContext, error) {
	return d.task, nil
}

func (d *manualDen) RequestManualReview(_ context.Context, request ManualReviewRequest) (ReviewRoundRef, error) {
	d.manualCalls++
	d.lastRequest = request
	if d.roundID == 0 {
		d.roundID = 27
	}
	return ReviewRoundRef{ProjectID: request.ProjectID, TaskID: request.TaskID, ID: d.roundID}, nil
}

func (d *manualDen) RequestReview(_ context.Context, request SubmissionRequest) (ReviewRoundRef, error) {
	d.requestCalls++
	if d.requestReviewErr != nil {
		return ReviewRoundRef{}, d.requestReviewErr
	}
	return ReviewRoundRef{ProjectID: request.ProjectID, TaskID: request.TaskID, ID: d.roundID}, nil
}

func (*manualDen) WatchGitHubChecks(context.Context, GateRequest) (GateEvidence, error) {
	return GateEvidence{}, errors.New("watch_github_checks must not be called for this test")
}

func (*manualDen) GetGitHubCheckGate(context.Context, GateRequest) (GateEvidence, error) {
	return GateEvidence{}, errors.New("get_github_check_gate must not be called for this test")
}

func (d *manualDen) GetReviewContext(_ context.Context, key Key) (Context, error) {
	d.contextCalls++
	next := d.contextNext
	if next == "" {
		next = "source_review_ready"
	}
	return Context{Key: key, NextState: next, Workspace: "/repo", Material: d.contextMaterial}, nil
}

func (*manualDen) FinalizeReview(_ context.Context, finalization Finalization) (Receipt, error) {
	return Receipt{Verdict: finalization.Completion.Verdict}, nil
}

func manualFixture(t *testing.T, den *manualDen) (*Service, *SQLiteStore, *fakeRuntime) {
	t.Helper()
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "manual-review.db"), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{completion: Completion{Verdict: "looks_good"}}
	service, err := New(store, den, runtime, "review profile")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return service, store, runtime
}

func manualTask(status string) TaskContext {
	return TaskContext{ProjectID: "dsh-crew", TaskID: 7416, Status: status, CurrentReviewRoundID: 27}
}

func TestManualReviewCapabilityUsesLiveTaskStatusAndExactLedger(t *testing.T) {
	den := &manualDen{task: manualTask("review")}
	service, store, _ := manualFixture(t, den)
	defer store.Close()

	best, err := service.GetManualReviewCapability(context.Background(), "dsh-crew", 7416)
	if err != nil {
		t.Fatal(err)
	}
	if !best.Eligible || best.Mode != ManualReviewBestEffort || best.Label != "Request best-effort review" || best.Source != nil {
		t.Fatalf("best-effort capability=%+v", best)
	}

	request := submissionRequestForTest()
	hash, err := submissionMaterialHash(request)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.AdmitSubmission(context.Background(), request, "exact-idem", hash)
	if err != nil {
		t.Fatal(err)
	}
	var roundID int64 = 27
	if _, err = store.TransitionSubmission(context.Background(), record.ID, SubmissionAccepted, SubmissionTransition{Phase: SubmissionRoundRecorded, ReviewRoundID: &roundID}); err != nil {
		t.Fatal(err)
	}
	exact, err := service.GetManualReviewCapability(context.Background(), "dsh-crew", 7416)
	if err != nil {
		t.Fatal(err)
	}
	if !exact.Eligible || exact.Mode != ManualReviewExact || exact.Label != "Send to review queue" || exact.Source == nil || exact.Source.Repository != request.Repository || exact.Source.CommitSHA != request.CommitSHA || exact.Source.Ref != request.Ref {
		t.Fatalf("exact capability=%+v", exact)
	}

	den.task = manualTask("in_progress")
	ineligible, err := service.GetManualReviewCapability(context.Background(), "dsh-crew", 7416)
	if err != nil {
		t.Fatal(err)
	}
	if ineligible.Eligible || ineligible.Mode != ManualReviewBestEffort || ineligible.Source != nil {
		t.Fatalf("ineligible capability=%+v", ineligible)
	}
}

func TestManualReviewDoesNotReuseExactSubmissionFromStaleRound(t *testing.T) {
	den := &manualDen{task: manualTask("review"), roundID: 28}
	den.task.CurrentReviewRoundID = 28
	service, store, _ := manualFixture(t, den)
	defer store.Close()

	request := submissionRequestForTest()
	request.RequiredChecks = nil
	request, err := normalizeSubmissionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := submissionMaterialHash(request)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.AdmitSubmission(context.Background(), request, "stale-exact-idem", hash)
	if err != nil {
		t.Fatal(err)
	}
	staleRoundID := int64(27)
	if _, err = store.TransitionSubmission(context.Background(), record.ID, SubmissionAccepted, SubmissionTransition{Phase: SubmissionRoundRecorded, ReviewRoundID: &staleRoundID}); err != nil {
		t.Fatal(err)
	}

	capability, err := service.GetManualReviewCapability(context.Background(), "dsh-crew", 7416)
	if err != nil {
		t.Fatal(err)
	}
	if !capability.Eligible || capability.Mode != ManualReviewBestEffort || capability.Source != nil {
		t.Fatalf("stale exact capability=%+v", capability)
	}
	receipt, replayed, err := service.SubmitManualReview(context.Background(), ManualReviewSubmissionRequest{
		ProjectID: "dsh-crew", TaskID: 7416, IdempotencyKey: "stale-manual-idem",
	})
	if err != nil || replayed || receipt.Mode != ManualReviewBestEffort || receipt.Status != "queued" || receipt.RoundID != 28 {
		t.Fatalf("stale exact submission receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
	if den.manualCalls != 1 {
		t.Fatalf("manual calls=%d, want best-effort request", den.manualCalls)
	}
}

func TestManualReviewExactReplayPreservesLedgerIdempotencyKey(t *testing.T) {
	den := &manualDen{task: manualTask("review"), roundID: 27}
	service, store, _ := manualFixture(t, den)
	defer store.Close()

	request := submissionRequestForTest()
	request.RequiredChecks = nil
	request, err := normalizeSubmissionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := submissionMaterialHash(request)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.AdmitSubmission(context.Background(), request, "ledger-exact-idem", hash)
	if err != nil {
		t.Fatal(err)
	}
	currentRoundID := int64(27)
	if _, err = store.TransitionSubmission(context.Background(), record.ID, SubmissionAccepted, SubmissionTransition{Phase: SubmissionRoundRecorded, ReviewRoundID: &currentRoundID}); err != nil {
		t.Fatal(err)
	}

	receipt, replayed, err := service.SubmitManualReview(context.Background(), ManualReviewSubmissionRequest{
		ProjectID: "dsh-crew", TaskID: 7416, IdempotencyKey: "different-button-idem",
	})
	if err != nil || !replayed || receipt.Mode != ManualReviewExact || receipt.Status != "queued" || receipt.JobID == "" {
		t.Fatalf("exact replay receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
	var submissions int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM crew_review_submissions`).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if submissions != 1 {
		t.Fatalf("submission rows=%d, want original ledger row only", submissions)
	}
	if den.manualCalls != 0 {
		t.Fatalf("manual calls=%d, want exact resume", den.manualCalls)
	}
}

func TestSubmitManualReviewQueuesCommitlessBestEffortJobWithPreamble(t *testing.T) {
	den := &manualDen{
		task:            manualTask("review"),
		contextMaterial: []byte(`{"task":{"description":"canonical task description"},"recent_messages":[{"content":"implementation is ready"}]}`),
	}
	service, store, runtime := manualFixture(t, den)
	defer store.Close()

	receipt, replayed, err := service.SubmitManualReview(context.Background(), ManualReviewSubmissionRequest{
		ProjectID: "dsh-crew", TaskID: 7416, IdempotencyKey: "manual-idem",
	})
	if err != nil || replayed || receipt.Mode != ManualReviewBestEffort || receipt.Status != "queued" || receipt.JobID == "" || receipt.RoundID != 27 {
		t.Fatalf("receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
	if den.manualCalls != 1 || !strings.Contains(den.lastRequest.Preamble, "current code on main") {
		t.Fatalf("manual Den request=%+v calls=%d", den.lastRequest, den.manualCalls)
	}
	job, err := service.Get(context.Background(), receipt.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Admission.Gate.CommitSHA != "" || job.Admission.ReviewPreamble == "" {
		t.Fatalf("commitless admission=%+v", job.Admission)
	}
	if _, err := service.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := runtime.Prompt()
	if !strings.Contains(prompt, "Server-owned review instructions") || !strings.Contains(prompt, "current code on main") || !strings.Contains(prompt, "canonical task description") {
		t.Fatalf("reviewer prompt omitted manual context: %s", prompt)
	}

	replayedReceipt, replayed, err := service.SubmitManualReview(context.Background(), ManualReviewSubmissionRequest{
		ProjectID: "dsh-crew", TaskID: 7416, IdempotencyKey: "manual-idem",
	})
	if err != nil || !replayed || replayedReceipt.JobID != receipt.JobID || den.manualCalls != 2 {
		t.Fatalf("replayed receipt=%+v replayed=%v calls=%d err=%v", replayedReceipt, replayed, den.manualCalls, err)
	}
}

func TestSubmitManualReviewRejectsTaskThatLeavesReview(t *testing.T) {
	den := &manualDen{task: manualTask("done")}
	service, store, _ := manualFixture(t, den)
	defer store.Close()

	_, _, err := service.SubmitManualReview(context.Background(), ManualReviewSubmissionRequest{ProjectID: "dsh-crew", TaskID: 7416})
	if !errors.Is(err, ErrTaskNotReviewable) {
		t.Fatalf("error=%v, want ErrTaskNotReviewable", err)
	}
	if den.manualCalls != 0 {
		t.Fatalf("manual Den calls=%d, want zero", den.manualCalls)
	}
}

func TestManualReviewHTTPContractAndTaskConflict(t *testing.T) {
	den := &manualDen{task: manualTask("review")}
	service, store, _ := manualFixture(t, den)
	defer store.Close()
	handler := NewHandler(service)

	capabilityResponse := httptest.NewRecorder()
	handler.ServeHTTP(capabilityResponse, httptest.NewRequest(http.MethodGet, "/v1/projects/dsh-crew/tasks/7416/manual-review", nil))
	if capabilityResponse.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", capabilityResponse.Code, capabilityResponse.Body.String())
	}
	var capability ManualReviewCapability
	if err := json.NewDecoder(capabilityResponse.Body).Decode(&capability); err != nil {
		t.Fatal(err)
	}
	if !capability.Eligible || capability.Mode != ManualReviewBestEffort || capability.Label != "Request best-effort review" || capability.Detail == "" {
		t.Fatalf("capability=%+v", capability)
	}

	post := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/projects/dsh-crew/tasks/7416/manual-review", nil)
	request.Header.Set("Idempotency-Key", "http-manual-idem")
	handler.ServeHTTP(post, request)
	if post.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", post.Code, post.Body.String())
	}
	var receipt ManualReviewReceipt
	if err := json.NewDecoder(post.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Mode != ManualReviewBestEffort || receipt.Status != "queued" || receipt.JobID == "" || receipt.RoundID != 27 {
		t.Fatalf("receipt=%+v", receipt)
	}

	den.task = manualTask("done")
	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, httptest.NewRequest(http.MethodPost, "/v1/projects/dsh-crew/tasks/7416/manual-review", nil))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(conflict.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "task_not_reviewable" {
		t.Fatalf("conflict body=%v", body)
	}
}
