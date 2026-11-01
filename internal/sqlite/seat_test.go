package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
	"crew-services/internal/store"
)

// seatWorld is two harness adapters on one fabric: claude drives sessions a
// and c, codex drives session b.
type seatWorld struct {
	t             *testing.T
	ctx           context.Context
	clock         *mutableClock
	svc           *service.Service
	store         *sqlite.Store
	claude, codex service.AdapterLease
	a, b, c       service.Binding
	mu            sync.Mutex
	op            int
}

func counter(prefix string) func() (string, error) {
	var mu sync.Mutex
	next := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("%s-%d", prefix, next), nil
	}
}

func newSeatWorld(t *testing.T, options ...service.Option) *seatWorld {
	t.Helper()
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "seats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistence.Close() })
	options = append([]service.Option{service.WithMaxLeaseDuration(time.Hour), service.WithMaxTTLDuration(48 * time.Hour), service.WithTokenGenerator(counter("token")), service.WithIDGenerator(counter("id"))}, options...)
	svc, err := service.New(persistence, clock, options...)
	if err != nil {
		t.Fatal(err)
	}
	w := &seatWorld{t: t, ctx: ctx, clock: clock, svc: svc, store: persistence}
	w.claude = register(t, svc, "crew-claude", "claude-1")
	w.codex = register(t, svc, "crew-codex", "codex-1")
	capabilities := []string{store.CapabilityDeliverWhenIdle, store.CapabilityDeliverNextStep}
	w.a = w.session(w.claude, "claude-session-a", capabilities)
	w.b = w.session(w.codex, "codex-thread-b", capabilities)
	w.c = w.session(w.claude, "claude-session-c", capabilities)
	return w
}

func (w *seatWorld) session(lease service.AdapterLease, key string, capabilities []string) service.Binding {
	w.t.Helper()
	binding, err := w.svc.RegisterSessionAddress(w.ctx, service.RegisterSessionAddressRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, NativeKey: key, Capabilities: capabilities})
	if err != nil {
		w.t.Fatal(err)
	}
	return binding
}

func (w *seatWorld) nextOp() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.op++
	return fmt.Sprintf("op-%d", w.op)
}

func (w *seatWorld) req(lease service.AdapterLease, session service.Binding, seat string) service.SeatRequest {
	return service.SeatRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, OperationID: w.nextOp(), SessionAddress: session.Address, SeatAddress: seat}
}

func (w *seatWorld) take(lease service.AdapterLease, session service.Binding, seat string) (service.SeatOperationResult, error) {
	return w.svc.TakeSeat(w.ctx, w.req(lease, session, seat))
}

func (w *seatWorld) setText(lease service.AdapterLease, session service.Binding, seat, kind, body string, expected *int64) (service.SeatOperationResult, error) {
	return w.svc.SetSeatText(w.ctx, service.SetSeatTextRequest{SeatRequest: w.req(lease, session, seat), Kind: kind, Body: body, ExpectedVersion: expected})
}

func (w *seatWorld) send(lease service.AdapterLease, from service.Binding, to, asSeat, body, replyTo string) (service.SubmitMessageResult, error) {
	return w.svc.SubmitMessage(w.ctx, service.SubmitMessageRequest{ProducerID: lease.AdapterID, LeaseToken: lease.LeaseToken, OperationID: w.nextOp(), SenderAddress: from.Address, RecipientAddress: to, AsSeat: asSeat, Body: body, ReplyToMessageID: replyTo, TTL: 24 * time.Hour})
}

func (w *seatWorld) claim(lease service.AdapterLease, address string, generation int64) (service.ClaimDeliveryResult, error) {
	return w.svc.ClaimDelivery(w.ctx, service.ClaimDeliveryRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, OperationID: w.nextOp(), RecipientAddress: address, RecipientGeneration: generation, Availability: store.AvailabilityIdle, ClaimDuration: time.Minute})
}

func (w *seatWorld) seat(address string) service.Seat {
	w.t.Helper()
	seat, err := w.svc.GetSeat(w.ctx, address)
	if err != nil {
		w.t.Fatalf("GetSeat(%s): %v", address, err)
	}
	return seat
}

func detail(err error) any {
	var typed *service.Error
	if errors.As(err, &typed) {
		return typed.Detail
	}
	return nil
}

func TestSeatTakeIsRefusedWhileHeldAndNamesTheHolder(t *testing.T) {
	w := newSeatWorld(t)
	request := w.req(w.claude, w.a, "lead@rusty-engine")
	taken, err := w.svc.TakeSeat(w.ctx, request)
	if err != nil || taken.Holding == nil || taken.Holding.HolderAddress != w.a.Address || taken.Holding.HoldingID != 1 || taken.Seat.State != store.SeatHeld || taken.Seat.Generation != 1 {
		t.Fatalf("take = %#v, %v", taken, err)
	}
	replayed, err := w.svc.TakeSeat(w.ctx, request)
	if err != nil || !replayed.Replayed || replayed.Holding.HoldingID != 1 {
		t.Fatalf("exact retry = %#v, %v", replayed, err)
	}
	_, err = w.take(w.codex, w.b, "lead@rusty-engine")
	if !hasCode(err, service.CodeSeatHeld) {
		t.Fatalf("second take error = %v, want seat_held", err)
	}
	holder, _ := detail(err).(map[string]any)["holding"].(store.Holding)
	if holder.HolderAddress != w.a.Address || holder.HolderAdapterID != "crew-claude" {
		t.Fatalf("seat_held detail = %#v", detail(err))
	}
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); !hasCode(err, service.CodeSeatHeld) {
		t.Fatalf("holder taking again error = %v, want seat_held", err)
	}
	// One session may hold several seats.
	if _, err := w.take(w.claude, w.a, "reviewer@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	mine, err := w.svc.ListSeats(w.ctx, store.SeatFilter{Holder: w.a.Address})
	if err != nil || len(mine) != 2 || mine[0].Address != "lead@rusty-engine" || mine[1].Address != "reviewer@rusty-engine" {
		t.Fatalf("seats held by a = %#v, %v", mine, err)
	}
	if _, err := w.take(w.claude, w.a, "Lead@rusty"); !hasCode(err, service.CodeInvalidSeatAddress) {
		t.Fatalf("malformed seat error = %v", err)
	}
	if _, err := w.svc.PutBinding(w.ctx, service.PutBindingRequest{Address: "lead@rusty-engine", ActorAdapterID: w.claude.AdapterID, LeaseToken: w.claude.LeaseToken, AdapterID: w.claude.AdapterID, TargetRef: "x"}); !hasCode(err, service.CodeInvalid) {
		t.Fatalf("binding a seat address error = %v", err)
	}
}

func TestRelieveRecordsTakeoverNotifiesOldHolderAndFencesSendingAsSeat(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	sent, err := w.send(w.claude, w.a, w.b.Address, "lead@rusty-engine", "from the lead", "")
	if err != nil || sent.Message.SenderAddress != "lead@rusty-engine" || sent.Message.SenderSession != w.a.Address || sent.Message.SenderHoldingID != 1 {
		t.Fatalf("send as seat = %#v, %v", sent.Message, err)
	}
	if _, err := w.send(w.codex, w.b, w.a.Address, "lead@rusty-engine", "not mine", ""); !hasCode(err, service.CodeNotHolder) {
		t.Fatalf("non-holder sending as seat error = %v, want not_holder", err)
	}
	if _, err := w.svc.RelieveSeat(w.ctx, w.req(w.codex, w.b, "reviewer@rusty-engine")); !hasCode(err, service.CodeSeatNotFound) {
		t.Fatalf("relieve missing seat error = %v", err)
	}
	request := w.req(w.codex, w.b, "lead@rusty-engine")
	request.Note = "taking over for the evening"
	relieved, err := w.svc.RelieveSeat(w.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if relieved.Holding.HolderAddress != w.b.Address || relieved.Holding.HoldingID != 2 || relieved.Holding.StartNote != "taking over for the evening" {
		t.Fatalf("new holding = %#v", relieved.Holding)
	}
	old := relieved.PreviousHolding
	if old == nil || old.EndReason != store.HoldingRelieved || old.RelievedBy != 2 || old.EndedAt == nil || old.HolderAddress != w.a.Address {
		t.Fatalf("relieved holding = %#v", old)
	}
	notice, err := w.svc.GetMessage(w.ctx, relieved.NoticeMessageID)
	if err != nil || notice.SenderAddress != store.SeatNoticeSender || notice.RecipientAddress != w.a.Address || !contains(notice.Body, "You no longer hold the seat lead@rusty-engine") || !contains(notice.Body, "taking over for the evening") {
		t.Fatalf("relief notice = %#v, %v", notice, err)
	}
	if _, err := w.send(w.claude, w.a, w.b.Address, "lead@rusty-engine", "still the lead?", ""); !hasCode(err, service.CodeNotHolder) {
		t.Fatalf("relieved session sending as seat error = %v, want not_holder", err)
	}
	if _, err := w.send(w.codex, w.b, w.a.Address, "lead@rusty-engine", "the new lead", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RelieveSeat(w.ctx, w.req(w.codex, w.b, "lead@rusty-engine")); !hasCode(err, service.CodeSeatHeld) {
		t.Fatalf("relieving yourself error = %v", err)
	}
	if _, err := w.svc.LeaveSeat(w.ctx, w.req(w.codex, w.b, "lead@rusty-engine")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RelieveSeat(w.ctx, w.req(w.claude, w.a, "lead@rusty-engine")); !hasCode(err, service.CodeSeatVacant) {
		t.Fatalf("relieve vacant seat error = %v, want seat_vacant", err)
	}
	holdings, err := w.svc.ListSeatHoldings(w.ctx, "lead@rusty-engine")
	if err != nil || len(holdings) != 2 || holdings[0].EndReason != store.HoldingLeftWithoutHandoff || holdings[1].EndReason != store.HoldingRelieved {
		t.Fatalf("holdings = %#v, %v", holdings, err)
	}
}

func TestSeatMailWaitsForTheNextHolderAndCarriesOverAHolderChange(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.send(w.claude, w.c, "lead@rusty-engine", "", "too early", ""); !hasCode(err, service.CodeSeatNotFound) {
		t.Fatalf("mail to a seat that was never created error = %v, want seat_not_found", err)
	}
	if _, err := w.setText(w.claude, w.c, "lead@rusty-engine", store.SeatTextCharter, "Answer for the engine roadmap.", nil); err != nil {
		t.Fatal(err)
	}
	first, err := w.send(w.claude, w.c, "lead@rusty-engine", "", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.send(w.claude, w.c, "lead@rusty-engine", "", "second", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Delivery.RecipientAddress != "lead@rusty-engine" || first.Delivery.RecipientGeneration != 1 {
		t.Fatalf("seat mail delivery = %#v", first.Delivery)
	}
	vacant, err := w.claim(w.claude, "lead@rusty-engine", 1)
	if err != nil || vacant.Claimed || vacant.Reason != "seat_vacant" {
		t.Fatalf("claim on vacant seat = %#v, %v", vacant, err)
	}
	if settled, err := w.svc.SettlePending(w.ctx); err != nil || len(settled) != 0 {
		t.Fatalf("settling vacant seat mail = %#v, %v", settled, err)
	}
	taken, err := w.take(w.claude, w.a, "lead@rusty-engine")
	if err != nil || taken.Seat.QueuedMail != 2 || taken.Seat.Charter == nil || taken.Seat.Charter.Body != "Answer for the engine roadmap." {
		t.Fatalf("take = %#v, %v", taken.Seat, err)
	}
	if _, err := w.claim(w.codex, "lead@rusty-engine", 1); !hasCode(err, service.CodeAdapterMismatch) {
		t.Fatalf("claim by an adapter that does not drive the holder error = %v", err)
	}
	claim, err := w.claim(w.claude, "lead@rusty-engine", 1)
	if err != nil || !claim.Claimed || claim.Delivery.DeliveryID != first.Delivery.DeliveryID || claim.HolderAddress != w.a.Address || claim.Delivery.ClaimHoldingID != 1 {
		t.Fatalf("holder claim = %#v, %v", claim, err)
	}
	// The seat changes hands before dispatch: the claim goes back to the queue
	// and the old holder's adapter can no longer dispatch it.
	if _, err := w.svc.RelieveSeat(w.ctx, w.req(w.codex, w.b, "lead@rusty-engine")); err != nil {
		t.Fatal(err)
	}
	requeued, err := w.svc.GetDelivery(w.ctx, first.Delivery.DeliveryID)
	if err != nil || requeued.State != "queued" {
		t.Fatalf("claim after relief = %#v, %v", requeued, err)
	}
	if _, err := w.svc.BeginDispatch(w.ctx, service.BeginDispatchRequest{AdapterID: w.claude.AdapterID, LeaseToken: w.claude.LeaseToken, OperationID: w.nextOp(), DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "claude-1"}); !hasCode(err, service.CodeClaimFenced) {
		t.Fatalf("old holder dispatch error = %v, want claim_fenced", err)
	}
	for i, want := range []service.SubmitMessageResult{first, second} {
		next, err := w.claim(w.codex, "lead@rusty-engine", 1)
		if err != nil || !next.Claimed || next.Delivery.DeliveryID != want.Delivery.DeliveryID || next.HolderAddress != w.b.Address || next.Delivery.ClaimHoldingID != 2 {
			t.Fatalf("new holder claim %d = %#v, %v", i, next, err)
		}
		attempt := fmt.Sprintf("codex-%d", i)
		if _, err := w.svc.BeginDispatch(w.ctx, service.BeginDispatchRequest{AdapterID: w.codex.AdapterID, LeaseToken: w.codex.LeaseToken, OperationID: w.nextOp(), DeliveryID: next.Delivery.DeliveryID, ClaimToken: next.ClaimToken, NativeAttemptRef: attempt}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.svc.Acknowledge(w.ctx, service.ReconcileDeliveryRequest{AdapterID: w.codex.AdapterID, LeaseToken: w.codex.LeaseToken, OperationID: w.nextOp(), DeliveryID: next.Delivery.DeliveryID, NativeAttemptRef: attempt}); err != nil {
			t.Fatal(err)
		}
	}
	if seat := w.seat("lead@rusty-engine"); seat.QueuedMail != 0 {
		t.Fatalf("queued mail after delivery = %d", seat.QueuedMail)
	}
}

func TestRetiringASeatFailsItsQueuedMailAndRecreatesWithANewGeneration(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.take(w.claude, w.a, "skeptic@rt-12"); err != nil {
		t.Fatal(err)
	}
	queued, err := w.send(w.codex, w.b, "skeptic@rt-12", "", "doubt this", "")
	if err != nil {
		t.Fatal(err)
	}
	retired, err := w.svc.RetireSeat(w.ctx, w.req(w.codex, w.b, "skeptic@rt-12"))
	if err != nil || retired.Seat.State != store.SeatRetired || retired.PreviousHolding == nil || retired.PreviousHolding.EndReason != store.HoldingRetired {
		t.Fatalf("retire = %#v, %v", retired, err)
	}
	failed, err := w.svc.GetDelivery(w.ctx, queued.Delivery.DeliveryID)
	if err != nil || failed.State != "failed" || failed.TerminalReason != "seat_retired" {
		t.Fatalf("queued mail after retire = %#v, %v", failed, err)
	}
	if _, err := w.send(w.codex, w.b, "skeptic@rt-12", "", "anyone?", ""); !hasCode(err, service.CodeSeatRetired) {
		t.Fatalf("mail to retired seat error = %v, want seat_retired", err)
	}
	again, err := w.take(w.codex, w.b, "skeptic@rt-12")
	if err != nil || again.Seat.Generation != 2 || again.Holding.HoldingID != 2 || again.Seat.CharterVersion != 0 {
		t.Fatalf("take after retire = %#v, %v", again, err)
	}
}

func TestAStaleHoldingStaysHeldKeepsItsMailAndIsRelievedNormally(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	mail, err := w.send(w.codex, w.b, "lead@rusty-engine", "", "are you there?", "")
	if err != nil {
		t.Fatal(err)
	}
	// The claude driver stops renewing; codex keeps its lease alive.
	w.clock.Advance(6 * time.Minute)
	if _, err := w.svc.RenewAdapter(w.ctx, service.RenewAdapterRequest{AdapterID: w.codex.AdapterID, LeaseToken: w.codex.LeaseToken, LeaseDuration: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(5 * time.Minute)
	stale, err := w.svc.ListSeats(w.ctx, store.SeatFilter{State: store.SeatStale})
	if err != nil || len(stale) != 1 || stale[0].Holding.StaleReason != "adapter_lease_expired" || stale[0].Holding.StaleSince == nil {
		t.Fatalf("stale seats = %#v, %v", stale, err)
	}
	if _, err := w.svc.SettlePending(w.ctx); err != nil {
		t.Fatal(err)
	}
	if delivery, err := w.svc.GetDelivery(w.ctx, mail.Delivery.DeliveryID); err != nil || delivery.State != "queued" {
		t.Fatalf("mail to a stale seat = %#v, %v", delivery, err)
	}
	if _, err := w.take(w.codex, w.b, "lead@rusty-engine"); !hasCode(err, service.CodeSeatHeld) {
		t.Fatalf("take of a stale seat error = %v, want seat_held", err)
	}
	relieved, err := w.svc.RelieveSeat(w.ctx, w.req(w.codex, w.b, "lead@rusty-engine"))
	if err != nil || relieved.Seat.State != store.SeatHeld || relieved.Seat.QueuedMail != 1 {
		t.Fatalf("relieve stale = %#v, %v", relieved.Seat, err)
	}
}

func TestSeatTextsAreSetNotAppendedWithProvenancePruningAndNotices(t *testing.T) {
	w := newSeatWorld(t, service.WithSeatTextVersions(3))
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := w.setText(w.claude, w.a, "lead@rusty-engine", store.SeatTextCharter, fmt.Sprintf("charter v%d\nsecond line", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	current, err := w.svc.GetSeatText(w.ctx, "lead@rusty-engine", store.SeatTextCharter, 0)
	if err != nil || current.Version != 5 || current.SetByAddress != w.a.Address || !reflect.DeepEqual(current.SetBySeats, []string{"lead@rusty-engine"}) || current.HoldingID != 1 {
		t.Fatalf("current charter = %#v, %v", current, err)
	}
	if _, err := w.svc.GetSeatText(w.ctx, "lead@rusty-engine", store.SeatTextCharter, 2); !hasCode(err, service.CodeVersionPruned) {
		t.Fatalf("pruned version error = %v, want version_pruned", err)
	}
	if _, err := w.svc.GetSeatText(w.ctx, "lead@rusty-engine", store.SeatTextCharter, 9); !hasCode(err, service.CodeVersionNotFound) {
		t.Fatalf("unissued version error = %v, want version_not_found", err)
	}
	versions, err := w.svc.ListSeatTexts(w.ctx, "lead@rusty-engine", store.SeatTextCharter)
	if err != nil || len(versions) != 3 || versions[0].Version != 5 || versions[2].Version != 3 || versions[0].Body != "" || versions[0].Length == 0 {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	if seats, err := w.svc.ListSeats(w.ctx, store.SeatFilter{Scope: "rusty-engine"}); err != nil || len(seats) != 1 || seats[0].CharterSummary != "charter v5" || seats[0].Charter != nil {
		t.Fatalf("listing = %#v, %v", seats, err)
	}
	stale := int64(4)
	_, err = w.setText(w.claude, w.a, "lead@rusty-engine", store.SeatTextCharter, "lost update", &stale)
	if !hasCode(err, service.CodeVersionConflict) || detail(err).(map[string]any)["current_version"] != int64(5) {
		t.Fatalf("stale expected_version error = %v %#v", err, detail(err))
	}
	// Anyone may rewrite a charter; the holder is told.
	rewritten, err := w.setText(w.codex, w.b, "lead@rusty-engine", store.SeatTextCharter, "dictator supreme", nil)
	if err != nil || rewritten.NoticeMessageID == "" || rewritten.Text.Version != 6 || rewritten.Text.SetByAdapterID != "crew-codex" {
		t.Fatalf("rewrite by another session = %#v, %v", rewritten, err)
	}
	notice, err := w.svc.GetMessage(w.ctx, rewritten.NoticeMessageID)
	if err != nil || notice.RecipientAddress != w.a.Address || !contains(notice.Body, "changed by "+w.b.Address) {
		t.Fatalf("charter notice = %#v, %v", notice, err)
	}
	// Leaving records whether a handoff was written during the holding.
	if _, err := w.svc.LeaveSeat(w.ctx, w.req(w.claude, w.c, "lead@rusty-engine")); !hasCode(err, service.CodeNotHolder) {
		t.Fatalf("leave by non-holder error = %v", err)
	}
	if _, err := w.setText(w.claude, w.a, "lead@rusty-engine", store.SeatTextHandoff, "Open: the release branch.", nil); err != nil {
		t.Fatal(err)
	}
	left, err := w.svc.LeaveSeat(w.ctx, w.req(w.claude, w.a, "lead@rusty-engine"))
	if err != nil || left.PreviousHolding.EndReason != store.HoldingLeft || left.Seat.State != store.SeatVacant {
		t.Fatalf("leave with handoff = %#v, %v", left, err)
	}
	next, err := w.take(w.codex, w.b, "lead@rusty-engine")
	if err != nil || next.Seat.Handoff == nil || next.Seat.Handoff.Body != "Open: the release branch." || next.PreviousHolding.HolderAddress != w.a.Address {
		t.Fatalf("take after handoff = %#v, %v", next, err)
	}
	carried := next.Seat.Handoff.Body + "\nAlso: CI is red."
	version := next.Seat.HandoffVersion
	if _, err := w.setText(w.codex, w.b, "lead@rusty-engine", store.SeatTextHandoff, carried, &version); err != nil {
		t.Fatal(err)
	}
	if text, err := w.svc.GetSeatText(w.ctx, "lead@rusty-engine", store.SeatTextHandoff, 0); err != nil || text.Body != carried || text.Version != 2 {
		t.Fatalf("carried-forward handoff = %#v, %v", text, err)
	}
}

func TestRepliesToASeatGoToTheSeat(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	question, err := w.send(w.claude, w.a, w.b.Address, "lead@rusty-engine", "status?", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.send(w.codex, w.b, w.a.Address, "", "to the session", question.Message.MessageID); !hasCode(err, service.CodeReplyRecipientMismatch) {
		t.Fatalf("reply to the sending session error = %v, want reply_recipient_mismatch", err)
	}
	answer, err := w.send(w.codex, w.b, "lead@rusty-engine", "", "green", question.Message.MessageID)
	if err != nil || answer.Message.RecipientAddress != "lead@rusty-engine" {
		t.Fatalf("reply to the seat = %#v, %v", answer, err)
	}
	// The next holder answers mail addressed to the seat as the seat.
	if _, err := w.svc.RelieveSeat(w.ctx, w.req(w.claude, w.c, "lead@rusty-engine")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.send(w.claude, w.c, w.b.Address, "lead@rusty-engine", "thanks", answer.Message.MessageID); err != nil {
		t.Fatalf("new holder replying as the seat: %v", err)
	}
}

func TestSeatReadsNeverClaimWakeOrDeliver(t *testing.T) {
	w := newSeatWorld(t)
	if _, err := w.take(w.claude, w.a, "lead@rusty-engine"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.setText(w.claude, w.a, "lead@rusty-engine", store.SeatTextCharter, "charter", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.send(w.codex, w.b, "lead@rusty-engine", "", "queued", ""); err != nil {
		t.Fatal(err)
	}
	before, err := w.svc.ListDeliveries(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.svc.ListSeats(w.ctx, store.SeatFilter{}); err != nil {
			t.Fatal(err)
		}
		w.seat("lead@rusty-engine")
		if _, err := w.svc.GetSeatText(w.ctx, "lead@rusty-engine", store.SeatTextCharter, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := w.svc.ListSeatTexts(w.ctx, "lead@rusty-engine", store.SeatTextCharter); err != nil {
			t.Fatal(err)
		}
		if _, err := w.svc.ListSeatHoldings(w.ctx, "lead@rusty-engine"); err != nil {
			t.Fatal(err)
		}
	}
	after, err := w.svc.ListDeliveries(w.ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("deliveries changed by reads:\nbefore %#v\nafter  %#v (%v)", before, after, err)
	}
}

func TestSeatsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "restart.db")
	open := func() (*sqlite.Store, *service.Service) {
		persistence, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := service.New(persistence, clock, service.WithMaxLeaseDuration(time.Hour), service.WithTokenGenerator(counter("token")), service.WithIDGenerator(counter("id")))
		if err != nil {
			t.Fatal(err)
		}
		return persistence, svc
	}
	first, svc := open()
	lease := register(t, svc, "crew-claude", "claude-1")
	session, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, NativeKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TakeSeat(ctx, service.SeatRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, OperationID: "take", SessionAddress: session.Address, SeatAddress: "lead@x"}); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	second, svc := open()
	defer second.Close()
	seat, err := svc.GetSeat(ctx, "lead@x")
	if err != nil || seat.Holding == nil || seat.Holding.HolderAddress != session.Address {
		t.Fatalf("seat after restart = %#v, %v", seat, err)
	}
}
