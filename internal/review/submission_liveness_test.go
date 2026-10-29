package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// perTaskRoundDen gives every task its own Den review round, so several
// submissions in one service admit separate jobs.
type perTaskRoundDen struct{ *submissionDen }

func (d perTaskRoundDen) RequestReview(ctx context.Context, request SubmissionRequest) (ReviewRoundRef, error) {
	if _, err := d.submissionDen.RequestReview(ctx, request); err != nil {
		return ReviewRoundRef{}, err
	}
	return ReviewRoundRef{ID: request.TaskID, ProjectID: request.ProjectID, TaskID: request.TaskID}, nil
}

func (d perTaskRoundDen) GetTaskContext(_ context.Context, key TaskKey) (TaskContext, error) {
	return TaskContext{ProjectID: key.ProjectID, TaskID: key.TaskID, Status: "review", CurrentReviewRoundID: key.TaskID}, nil
}

// hangingSourceChecker hangs, until its context ends, on the commits listed
// in hang, as a stuck git command would; other commits are present once
// ready is set.
type hangingSourceChecker struct {
	mu    sync.Mutex
	ready bool
	hang  map[string]bool
	hung  int
}

func (c *hangingSourceChecker) HasCommit(ctx context.Context, _ string, commitSHA string) (bool, error) {
	c.mu.Lock()
	ready, hang := c.ready, c.hang[commitSHA]
	if hang {
		c.hung++
	}
	c.mu.Unlock()
	if hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return ready, nil
}

func TestHungSourceCheckDoesNotBlockOtherSubmissionsAndHeartbeatIsVisible(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)}
	store, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "review.db"), clock, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	den := perTaskRoundDen{&submissionDen{}}
	source := &hangingSourceChecker{}
	service, err := New(store, den, &fakeRuntime{}, "review profile", WithClock(clock),
		WithSourceChecker(source, time.Hour), WithSubmissionAdvanceTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if loop := service.SubmissionLoop(); loop.Passes != 0 || !loop.LastPassStartedAt.IsZero() {
		t.Fatalf("heartbeat before any pass=%+v", loop)
	}

	hung := submissionRequestForTest()
	hung.RequiredChecks = []string{}
	other := hung
	other.TaskID = hung.TaskID + 1
	other.CommitSHA = "1123456789abcdef0123456789abcdef01234567"
	var ids []string
	for _, request := range []SubmissionRequest{hung, other} {
		receipt, _, err := service.SubmitTaskForReview(ctx, request)
		if err != nil || receipt.Phase != SubmissionSourcePending {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		ids = append(ids, receipt.SubmissionID)
	}

	// The first submission's checkout check now hangs; the second's commit
	// has arrived.
	source.mu.Lock()
	source.ready = true
	source.hang = map[string]bool{hung.CommitSHA: true}
	source.mu.Unlock()
	clock.Advance(time.Minute)
	started := time.Now()
	if _, err := service.AdvanceSubmissions(ctx); err == nil {
		t.Fatal("pass did not report the timed-out advance")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("pass took %s behind a hung check", elapsed)
	}
	if record, err := store.GetSubmission(ctx, ids[1]); err != nil || record.Phase != SubmissionJobAdmitted || record.JobID == "" {
		t.Fatalf("other submission behind the hung one=%+v err=%v", record, err)
	}
	// The hung submission still waits, and its checkout grace period still
	// runs from when the wait began: a no-checks submission does not bounce
	// through gate_passed on each pass.
	if record, err := store.GetSubmission(ctx, ids[0]); err != nil || record.Phase != SubmissionSourcePending || !record.UpdatedAt.Equal(clock.now.Add(-time.Minute)) {
		t.Fatalf("hung submission=%+v err=%v", record, err)
	}

	loop := service.SubmissionLoop()
	if loop.Running || loop.Passes != 1 || !loop.LastPassStartedAt.Equal(clock.now) || !loop.LastPassFinishedAt.Equal(clock.now) ||
		loop.LastPassExamined != 2 || loop.LastPassError == "" {
		t.Fatalf("heartbeat=%+v", loop)
	}

	handler := NewHandler(service)
	for _, path := range []string{"/healthz", "/v1/review-pool"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		var body struct {
			SubmissionLoop *SubmissionLoopHeartbeat `json:"submission_loop"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d body=%s err=%v", path, response.Code, response.Body, err)
		}
		if body.SubmissionLoop == nil || body.SubmissionLoop.Passes != 1 || !body.SubmissionLoop.LastPassFinishedAt.Equal(clock.now) {
			t.Fatalf("%s heartbeat=%+v", path, body.SubmissionLoop)
		}
	}

	// Once the check stops hanging but the commit never arrives, the grace
	// period ends the wait.
	source.mu.Lock()
	source.ready, source.hang = false, nil
	source.mu.Unlock()
	clock.Advance(time.Hour)
	if _, err := service.AdvanceSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	if record, err := store.GetSubmission(ctx, ids[0]); err != nil || record.Phase != SubmissionSourceMissing {
		t.Fatalf("submission after grace=%+v err=%v", record, err)
	}
	if loop := service.SubmissionLoop(); loop.Passes != 2 || loop.LastPassError != "" || loop.LastPassExamined != 1 {
		t.Fatalf("heartbeat after a clean pass=%+v", loop)
	}
}
