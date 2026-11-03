package review

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// crewNotifyDen is a Den whose project lists crew_notify on_done targets.
type crewNotifyDen struct {
	*submissionDen
	targets []CrewNotifyTarget
}

func (d *crewNotifyDen) DoneNotifyTargets(context.Context, string) ([]CrewNotifyTarget, error) {
	return d.targets, nil
}

type verdictFixture struct {
	store   *SQLiteStore
	service *Service
	fabric  *fakeFabric
	clock   *testClock
}

// newVerdictFixture runs one submission through a reviewer that returns
// completion, with den as Den, and the fake fabric as the notifier.
func newVerdictFixture(t *testing.T, den DenReviewClient, completion Completion, notifyAddress string) (*verdictFixture, SubmissionReceipt) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}
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
	service, err := New(store, den, &fakeRuntime{completion: completion}, "review profile", WithClock(clock), WithNotifier(notifier))
	if err != nil {
		t.Fatal(err)
	}
	request := submissionRequestForTest()
	request.NotifyAddress = notifyAddress
	result, _, err := service.SubmitTaskForReview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job, err := service.Get(context.Background(), result.JobID); err != nil || job.State != Succeeded {
		t.Fatalf("job = %+v, %v", job, err)
	}
	return &verdictFixture{store: store, service: service, fabric: fabric, clock: clock}, result
}

func (f *verdictFixture) notices(t *testing.T, kind NoticeKind) []Notice {
	t.Helper()
	all, err := f.store.queryNotices(context.Background(), `SELECT `+noticeColumns+` FROM crew_review_notices WHERE kind=? ORDER BY operation_id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func passedGateDen() *submissionDen {
	return &submissionDen{watchGate: GateEvidence{Status: "passed", Handle: "43", TerminalReason: "checks_passed"}}
}

var changes = Completion{Verdict: "changes_requested", NewFindings: []NewFinding{{Category: "blocking_bug", Summary: "Thread pages hide posts after the first 200"}}}

func TestChangesRequestedWakesTheSubmitter(t *testing.T) {
	f, _ := newVerdictFixture(t, passedGateDen(), changes, "s/5b693970244d98db")
	got := f.notices(t, NoticeChangesRequested)
	if len(got) != 1 {
		t.Fatalf("changes_requested notices = %+v", got)
	}
	n := got[0]
	if n.Recipient != "s/5b693970244d98db" || n.Fallback != "dsh-crew.lead" || n.Policy != "next_turn" {
		t.Fatalf("notice = %+v", n)
	}
	for _, want := range []string{"changes requested on Den dsh-crew task 7416", "1 new finding.", "[blocking_bug] Thread pages hide posts after the first 200", "submit_task_for_review again with the same arguments", "After 12 review rounds"} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, n.Body)
		}
	}
	if _, err := f.service.SendNotices(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := f.fabric.accepted["crew-review\x00"+n.OperationID]
	if sent.RecipientAddress != "s/5b693970244d98db" || sent.ActivationPolicy != "next_turn" {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestARetriedCompletionDoesNotNotifyTwice(t *testing.T) {
	f, submitted := newVerdictFixture(t, passedGateDen(), changes, "s/5b693970244d98db")
	id := submitted.JobID
	job, _ := f.store.Get(context.Background(), id)
	if _, err := f.store.CompleteWithNotices(context.Background(), id, *job.Receipt, verdictNotices(job, *job.Receipt, SubmissionRecord{}, false, nil)); err != nil {
		t.Fatal(err)
	}
	if got := f.notices(t, NoticeChangesRequested); len(got) != 1 {
		t.Fatalf("a second completion added notices: %+v", got)
	}
}

func TestWithoutANotifyAddressTheLeadSeatIsTold(t *testing.T) {
	f, _ := newVerdictFixture(t, passedGateDen(), changes, "")
	got := f.notices(t, NoticeChangesRequested)
	if len(got) != 1 || got[0].Recipient != "dsh-crew.lead" || got[0].Fallback != "" {
		t.Fatalf("notices = %+v", got)
	}
}

func TestAGoneSubmitterFallsBackToTheLeadSeat(t *testing.T) {
	f, _ := newVerdictFixture(t, passedGateDen(), changes, "s/0000000000000000")
	f.fabric.recipients = map[string]bool{"dsh-crew.lead": true}
	if _, err := f.service.SendNotices(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := f.notices(t, NoticeChangesRequested)[0]
	if n.Recipient != "dsh-crew.lead" || n.Fallback != "" || n.State != NoticePending {
		t.Fatalf("after the first pass = %+v", n)
	}
	if _, err := f.service.SendNotices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n = f.notices(t, NoticeChangesRequested)[0]; n.State != NoticeSent {
		t.Fatalf("after the second pass = %+v", n)
	}
	if sent := f.fabric.accepted["crew-review\x00"+n.OperationID]; sent.RecipientAddress != "dsh-crew.lead" || sent.ActivationPolicy != "next_turn" {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestAProjectWithNoLeadSeatAndNoAddressGetsNothingUseless(t *testing.T) {
	if seat := leadSeat("Bad_Project"); seat != "" {
		t.Fatalf("lead seat of an impossible scope = %q", seat)
	}
	j := Job{ID: "j1", Admission: Admission{Key: Key{ProjectID: "Bad_Project", TaskID: 1, ReviewRoundID: 2, CorrelationID: "c"}}}
	if got := verdictNotices(j, Receipt{Verdict: "changes_requested"}, SubmissionRecord{}, false, nil); len(got) != 0 {
		t.Fatalf("notices = %+v", got)
	}
}

func TestPassedTellsOnlyTheProjectsCrewNotifyTargets(t *testing.T) {
	passed := Completion{Verdict: "looks_good"}
	f, _ := newVerdictFixture(t, passedGateDen(), passed, "s/5b693970244d98db")
	if got := append(f.notices(t, NoticePassed), f.notices(t, NoticeChangesRequested)...); len(got) != 0 {
		t.Fatalf("a pass with no crew_notify sent %+v", got)
	}
	den := &crewNotifyDen{submissionDen: passedGateDen(), targets: []CrewNotifyTarget{
		{Address: "dsh-crew.lead"},
		{Address: "dsh-crew.deploy", Policy: "next_turn"},
		{Address: "dsh-crew.lead"},
	}}
	f, _ = newVerdictFixture(t, den, passed, "s/5b693970244d98db")
	got := f.notices(t, NoticePassed)
	if len(got) != 2 {
		t.Fatalf("passed notices = %+v", got)
	}
	policies := map[string]string{}
	for _, n := range got {
		policies[n.Recipient] = n.Policy
		if !strings.Contains(n.Body, "task 7416 passed review") || !strings.Contains(n.Body, "crew_notify on_done") {
			t.Errorf("body = %q", n.Body)
		}
	}
	if policies["dsh-crew.lead"] != "no_wake" || policies["dsh-crew.deploy"] != "next_turn" {
		t.Fatalf("policies = %v", policies)
	}
}
