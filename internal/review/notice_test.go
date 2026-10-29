package review

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/fabricclient"
)

// fakeFabric mimics the parts of crew-messaging the notifier relies on:
// leases, a sender address that must be bound to the producer, and
// per-producer operation idempotency checked before authority.
type fakeFabric struct {
	mu sync.Mutex

	down bool
	// loseResponse accepts the next submit but reports a transport error, as
	// if crew-review crashed before it saw the reply.
	loseResponse bool
	// recipients that are bound; nil means every recipient is bound.
	recipients map[string]bool

	leases     map[string]fabricclient.Lease
	bindings   map[string]fabricclient.Binding
	accepted   map[string]fabricclient.MessageRequest
	order      []string
	registers  int
	submits    int
	tokenCount int
}

func newFakeFabric() *fakeFabric {
	return &fakeFabric{leases: map[string]fabricclient.Lease{}, bindings: map[string]fabricclient.Binding{}, accepted: map[string]fabricclient.MessageRequest{}}
}

var errFabricDown = errors.New("dial tcp 127.0.0.1:8787: connect: connection refused")

func fabricError(status int, code string) error {
	return &fabricclient.HTTPError{Status: status, Body: `{"code":"` + code + `"}`}
}

func (f *fakeFabric) Register(_ context.Context, adapterID, instanceID string, duration time.Duration) (fabricclient.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fabricclient.Lease{}, errFabricDown
	}
	f.registers++
	f.tokenCount++
	lease := fabricclient.Lease{AdapterID: adapterID, InstanceID: instanceID, LeaseToken: "token-" + string(rune('a'+f.tokenCount)), ExpiresAt: time.Now().Add(duration)}
	f.leases[adapterID] = lease
	return lease, nil
}

func (f *fakeFabric) Renew(_ context.Context, adapterID, token string, duration time.Duration) (fabricclient.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fabricclient.Lease{}, errFabricDown
	}
	lease, ok := f.leases[adapterID]
	if !ok || lease.LeaseToken != token {
		return fabricclient.Lease{}, fabricError(409, "lease_fenced")
	}
	lease.ExpiresAt = time.Now().Add(duration)
	f.leases[adapterID] = lease
	return lease, nil
}

func (f *fakeFabric) Resolve(_ context.Context, address string) (fabricclient.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fabricclient.Binding{}, errFabricDown
	}
	binding, ok := f.bindings[address]
	if !ok {
		return fabricclient.Binding{}, fabricError(404, "not_found")
	}
	return binding, nil
}

func (f *fakeFabric) Bind(_ context.Context, address string, request fabricclient.BindRequest) (fabricclient.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fabricclient.Binding{}, errFabricDown
	}
	if lease := f.leases[request.ActorAdapterID]; lease.LeaseToken != request.LeaseToken {
		return fabricclient.Binding{}, fabricError(409, "lease_fenced")
	}
	binding := fabricclient.Binding{Address: address, Bound: true, AdapterID: request.AdapterID, TargetRef: request.TargetRef, Revision: f.bindings[address].Revision + 1, Generation: 1}
	f.bindings[address] = binding
	return binding, nil
}

func (f *fakeFabric) Submit(_ context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return fabricclient.SubmittedMessage{}, errFabricDown
	}
	f.submits++
	key := request.ProducerID + "\x00" + request.OperationID
	if existing, ok := f.accepted[key]; ok {
		if existing.Body != request.Body || existing.RecipientAddress != request.RecipientAddress {
			return fabricclient.SubmittedMessage{}, fabricError(409, "operation_conflict")
		}
		return fabricclient.SubmittedMessage{Message: fabricclient.Message{MessageID: "msg-" + request.OperationID}, Replayed: true}, nil
	}
	if lease := f.leases[request.ProducerID]; lease.LeaseToken != request.LeaseToken {
		return fabricclient.SubmittedMessage{}, fabricError(409, "lease_fenced")
	}
	sender, ok := f.bindings[request.SenderAddress]
	if !ok || !sender.Bound || sender.AdapterID != request.ProducerID {
		return fabricclient.SubmittedMessage{}, fabricError(409, "adapter_mismatch")
	}
	if f.recipients != nil && !f.recipients[request.RecipientAddress] {
		return fabricclient.SubmittedMessage{}, fabricError(404, "not_found")
	}
	f.accepted[key] = request
	f.order = append(f.order, request.OperationID)
	if f.loseResponse {
		f.loseResponse = false
		return fabricclient.SubmittedMessage{}, errors.New("read tcp: connection reset by peer")
	}
	return fabricclient.SubmittedMessage{Message: fabricclient.Message{MessageID: "msg-" + request.OperationID}}, nil
}

func (f *fakeFabric) acceptedOperations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *fakeFabric) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

type noticeFixture struct {
	t      *testing.T
	path   string
	clock  *testClock
	den    *submissionDen
	fabric *fakeFabric

	store   *SQLiteStore
	service *Service
}

func newNoticeFixture(t *testing.T, den *submissionDen) *noticeFixture {
	t.Helper()
	f := &noticeFixture{t: t, path: filepath.Join(t.TempDir(), "review.db"), clock: &testClock{now: time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)}, den: den, fabric: newFakeFabric()}
	f.restart()
	t.Cleanup(func() { _ = f.store.Close() })
	return f
}

// restart closes the store and builds a fresh store, service and notifier on
// the same database, as a crew-review process restart would.
func (f *noticeFixture) restart() {
	f.t.Helper()
	if f.store != nil {
		_ = f.store.Close()
	}
	store, err := OpenSQLite(context.Background(), f.path, f.clock, 2)
	if err != nil {
		f.t.Fatal(err)
	}
	notifier, err := NewFabricNotifier(f.fabric, f.clock, FabricNotifierConfig{InstanceID: "crew-review-test"})
	if err != nil {
		f.t.Fatal(err)
	}
	service, err := New(store, f.den, &fakeRuntime{}, "review profile", WithClock(f.clock), WithNotifier(notifier))
	if err != nil {
		f.t.Fatal(err)
	}
	f.store, f.service = store, service
}

func (f *noticeFixture) send() error {
	_, err := f.service.SendNotices(context.Background())
	return err
}

func (f *noticeFixture) notices(submissionID string) []Notice {
	f.t.Helper()
	notices, err := f.store.ListSubmissionNotices(context.Background(), submissionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return notices
}

func failedGate(handle string, attempt int64) GateEvidence {
	return GateEvidence{
		Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "failed", Handle: handle, Attempt: attempt,
		TerminalReason: "checks_failed", FailureSummary: "Failed checks: lint (failure)",
		FailedChecks: []GateCheck{{Name: "lint", Conclusion: "failure", URL: "https://github.com/owner/repo/actions/runs/1/job/2"}},
	}
}

func notifyingRequest() SubmissionRequest {
	request := submissionRequestForTest()
	request.NotifyAddress = "claude/session-1"
	return request
}

func titledDen(den *submissionDen) *submissionDen {
	den.taskContext = &TaskContext{ProjectID: "dsh-crew", TaskID: 7416, Status: "review", CurrentReviewRoundID: 7, Title: "Managed review handoff"}
	den.roundID = 7
	return den
}

func TestGateFailureNoticeSurvivesCrashBetweenOutboxWriteAndFabricAccept(t *testing.T) {
	f := newNoticeFixture(t, titledDen(&submissionDen{watchGate: failedGate("42", 1)}))
	receipt, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil || receipt.Phase != SubmissionGateFailed {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	pending := f.notices(receipt.SubmissionID)
	if len(pending) != 1 || pending[0].State != NoticePending || pending[0].OperationID != "submission/"+receipt.SubmissionID+"/gate_failed/42" {
		t.Fatalf("outbox after gate failure=%+v", pending)
	}
	body := pending[0].Body
	for _, want := range []string{"#7416", "Managed review handoff", submissionTestSHA, "main", "lint (failure): https://github.com/owner/repo/actions/runs/1/job/2", "submit_task_for_review again with the same arguments"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice body lacks %q:\n%s", want, body)
		}
	}

	// Crash before any send: the outbox row survives the restart.
	f.restart()
	// The fabric accepts, but crew-review never sees the reply.
	f.fabric.loseResponse = true
	if err := f.send(); err == nil {
		t.Fatal("lost fabric reply was reported as sent")
	}
	if notices := f.notices(receipt.SubmissionID); notices[0].State != NoticePending || notices[0].Attempts != 1 {
		t.Fatalf("notice after lost reply=%+v", notices)
	}
	// Crash again, then retry: the fabric replays the same operation.
	f.restart()
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	notices := f.notices(receipt.SubmissionID)
	if len(notices) != 1 || notices[0].State != NoticeSent || notices[0].MessageID == "" {
		t.Fatalf("notice after retry=%+v", notices)
	}
	if accepted := f.fabric.acceptedOperations(); len(accepted) != 1 || accepted[0] != notices[0].OperationID {
		t.Fatalf("fabric accepted %v, want exactly one notice", accepted)
	}
	request := f.fabric.accepted[FabricProducerID+"\x00"+notices[0].OperationID]
	if request.SenderAddress != FabricOriginAddress || request.RecipientAddress != "claude/session-1" || request.ActivationPolicy != "next_step" || request.TTL != "24h0m0s" {
		t.Fatalf("fabric message=%+v", request)
	}
	if origin := f.fabric.bindings[FabricOriginAddress]; !origin.Bound || origin.AdapterID != FabricProducerID {
		t.Fatalf("origin binding=%+v", origin)
	}
	// Further submits of the same request replay the failed gate without
	// another notice.
	if _, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest()); err != nil {
		t.Fatal(err)
	}
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	if accepted := f.fabric.acceptedOperations(); len(accepted) != 1 || len(f.notices(receipt.SubmissionID)) != 1 {
		t.Fatalf("resubmission of the same failed gate added a notice: %v", accepted)
	}
}

func TestGateFailureNoticeWaitsForUnavailableFabricWithoutBlockingAdmission(t *testing.T) {
	den := titledDen(&submissionDen{
		watchGate: GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "pending", Handle: "42", Attempt: 1},
		readGates: []GateEvidence{failedGate("42", 1)},
	})
	f := newNoticeFixture(t, den)
	f.fabric.setDown(true)
	receipt, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil || receipt.Phase != SubmissionGatePending {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	// The background loop records the failure while the fabric is down.
	if _, err := f.service.AdvanceSubmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := f.send(); err == nil {
			t.Fatal("send to an unavailable fabric reported success")
		}
	}
	notices := f.notices(receipt.SubmissionID)
	if len(notices) != 1 || notices[0].State != NoticePending || notices[0].Attempts != 3 || !strings.Contains(notices[0].LastError, "connection refused") {
		t.Fatalf("notice while fabric down=%+v", notices)
	}

	// Admission of another submission is unaffected by the fabric outage.
	den.mu.Lock()
	den.watchGate = GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "passed", Handle: "43", TerminalReason: "checks_passed"}
	den.mu.Unlock()
	other := notifyingRequest()
	other.RequiredChecks = []string{"build"}
	admitted, _, err := f.service.SubmitTaskForReview(context.Background(), other)
	if err != nil || admitted.Phase != SubmissionJobAdmitted {
		t.Fatalf("admission while fabric down=%+v err=%v", admitted, err)
	}

	f.fabric.setDown(false)
	for range 2 {
		if err := f.send(); err != nil {
			t.Fatal(err)
		}
	}
	if accepted := f.fabric.acceptedOperations(); len(accepted) != 1 || accepted[0] != "submission/"+receipt.SubmissionID+"/gate_failed/42" {
		t.Fatalf("fabric accepted %v, want exactly the one failed-gate notice", accepted)
	}
	if notices := f.notices(receipt.SubmissionID); notices[0].State != NoticeSent {
		t.Fatalf("notice after recovery=%+v", notices)
	}
}

func TestGateFailureNoticePerFailedGateAcrossRerunsAndResubmits(t *testing.T) {
	den := titledDen(&submissionDen{watchGate: failedGate("42", 1)})
	f := newNoticeFixture(t, den)
	first, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil || first.Phase != SubmissionGateFailed {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := f.send(); err != nil {
		t.Fatal(err)
	}

	// The check is re-run: Den reopens gate 42 as attempt 2, which fails too.
	den.mu.Lock()
	den.watchGate = GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "pending", Handle: "42", Attempt: 2}
	den.readGates = []GateEvidence{failedGate("42", 2)}
	den.mu.Unlock()
	rerun, replayed, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil || !replayed || rerun.SubmissionID != first.SubmissionID || rerun.Phase != SubmissionGatePending {
		t.Fatalf("rerun=%+v replayed=%v err=%v", rerun, replayed, err)
	}
	if _, err := f.service.AdvanceSubmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Crash between the second outbox write and its send.
	f.restart()
	if err := f.send(); err != nil {
		t.Fatal(err)
	}

	// A resubmit that registers a new gate (handle 43) that fails gets its
	// own notice as well.
	den.mu.Lock()
	den.watchGate = failedGate("43", 1)
	den.mu.Unlock()
	if _, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.send(); err != nil {
			t.Fatal(err)
		}
	}

	prefix := "submission/" + first.SubmissionID + "/gate_failed/"
	want := []string{prefix + "42", prefix + "42.2", prefix + "43"}
	accepted := f.fabric.acceptedOperations()
	if strings.Join(accepted, ",") != strings.Join(want, ",") {
		t.Fatalf("fabric accepted %v, want %v", accepted, want)
	}
	notices := f.notices(first.SubmissionID)
	if len(notices) != 3 {
		t.Fatalf("outbox=%+v", notices)
	}
	for _, notice := range notices {
		if notice.State != NoticeSent {
			t.Fatalf("notice %s state=%s", notice.OperationID, notice.State)
		}
	}
	if f.fabric.registers != 2 {
		t.Fatalf("fabric registrations=%d, want one per process", f.fabric.registers)
	}
}

func TestNoGateFailureNoticeWithoutNotifyAddress(t *testing.T) {
	f := newNoticeFixture(t, titledDen(&submissionDen{watchGate: failedGate("42", 1)}))
	receipt, _, err := f.service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil || receipt.Phase != SubmissionGateFailed {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	if notices := f.notices(receipt.SubmissionID); len(notices) != 0 {
		t.Fatalf("outbox without notify address=%+v", notices)
	}
	if f.fabric.submits != 0 || f.fabric.registers != 0 {
		t.Fatalf("fabric was contacted: submits=%d registers=%d", f.fabric.submits, f.fabric.registers)
	}
}

func TestNotifyAddressOnResendDoesNotForkSubmission(t *testing.T) {
	den := titledDen(&submissionDen{watchGate: GateEvidence{Repository: "owner/repo", Ref: "main", CommitSHA: submissionTestSHA, Status: "pending", Handle: "42", Attempt: 1}})
	f := newNoticeFixture(t, den)
	first, _, err := f.service.SubmitTaskForReview(context.Background(), submissionRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	resend, replayed, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil || !replayed || resend.SubmissionID != first.SubmissionID {
		t.Fatalf("resend=%+v replayed=%v err=%v", resend, replayed, err)
	}
	record, err := f.store.GetSubmission(context.Background(), first.SubmissionID)
	if err != nil || record.Request.NotifyAddress != "claude/session-1" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	// A resend without the address keeps the stored one.
	if _, _, err := f.service.SubmitTaskForReview(context.Background(), submissionRequestForTest()); err != nil {
		t.Fatal(err)
	}
	den.mu.Lock()
	den.readGates = []GateEvidence{failedGate("42", 1)}
	den.mu.Unlock()
	if _, err := f.service.AdvanceSubmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	if accepted := f.fabric.acceptedOperations(); len(accepted) != 1 {
		t.Fatalf("fabric accepted %v", accepted)
	}
	bad := notifyingRequest()
	bad.NotifyAddress = "claude/two words"
	if _, _, err := f.service.SubmitTaskForReview(context.Background(), bad); err == nil {
		t.Fatal("notify address with whitespace was accepted")
	}
}

func TestUndeliverableNoticeIsAbandonedAfterItsTTL(t *testing.T) {
	f := newNoticeFixture(t, titledDen(&submissionDen{watchGate: failedGate("42", 1)}))
	f.fabric.recipients = map[string]bool{}
	receipt, _, err := f.service.SubmitTaskForReview(context.Background(), notifyingRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.send(); err == nil {
		t.Fatal("unbound recipient reported as sent")
	}
	f.clock.Advance(NoticeTTL)
	if err := f.send(); err != nil {
		t.Fatal(err)
	}
	notices := f.notices(receipt.SubmissionID)
	if len(notices) != 1 || notices[0].State != NoticeAbandoned {
		t.Fatalf("notice after TTL=%+v", notices)
	}
	if accepted := f.fabric.acceptedOperations(); len(accepted) != 0 {
		t.Fatalf("fabric accepted %v", accepted)
	}
}
