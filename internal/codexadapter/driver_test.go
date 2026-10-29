package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// fakeNative is a scripted Codex daemon: thread statuses, the active turn,
// and what each native call answers.
type fakeNative struct {
	mu       sync.Mutex
	loaded   []string
	threads  map[string]NativeThread
	active   map[string]string
	steerErr error
	queueErr error
	// queueKeeps records an item even when queueErr is returned, as a lost
	// response would.
	queueKeeps bool
	steerKeeps bool
	queued     map[string][]QueuedSubmission
	turns      map[string][]NativeTurn
	calls      []string
}

func newFakeNative() *fakeNative {
	return &fakeNative{threads: map[string]NativeThread{}, active: map[string]string{}, queued: map[string][]QueuedSubmission{}, turns: map[string][]NativeTurn{}}
}

func (f *fakeNative) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeNative) LoadedThreads(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.loaded...), nil
}

func (f *fakeNative) ReadThread(_ context.Context, id string) (NativeThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	thread, ok := f.threads[id]
	if !ok {
		return NativeThread{}, &rpcError{Code: -32600, Message: "thread not found"}
	}
	return thread, nil
}

func (f *fakeNative) ActiveTurn(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("active " + id)
	return f.active[id], nil
}

func (f *fakeNative) RecentTurns(_ context.Context, id string, _ int, _ bool) ([]NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("turns " + id)
	return f.turns[id], nil
}

func (f *fakeNative) Steer(_ context.Context, id, turn, _, clientID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("steer " + id + " " + turn + " " + clientID)
	if f.steerErr == nil || f.steerKeeps {
		f.turns[id] = append(f.turns[id], NativeTurn{ID: turn, Status: "inProgress", ClientIDs: []string{clientID}})
	}
	return f.steerErr
}

func (f *fakeNative) QueueAdd(_ context.Context, id, _, clientID string) (QueuedSubmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("queue " + id + " " + clientID)
	if f.queueErr == nil || f.queueKeeps {
		f.queued[id] = append(f.queued[id], QueuedSubmission{ID: "q-" + clientID, ClientUserMessageID: clientID})
	}
	if f.queueErr != nil {
		return QueuedSubmission{}, f.queueErr
	}
	return QueuedSubmission{ID: "q-" + clientID, ClientUserMessageID: clientID}, nil
}

func (f *fakeNative) QueueList(_ context.Context, id string) ([]QueuedSubmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("queue-list " + id)
	return f.queued[id], nil
}

func (f *fakeNative) Resume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("resume " + id)
	return nil
}

func (f *fakeNative) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

// fakeFabric hands out scripted claims and records settlements.
type fakeFabric struct {
	mu        sync.Mutex
	bindings  map[string]fabricclient.Binding
	claims    map[string][]fabricclient.Claim // address -> next claims
	claimReqs []fabricclient.ClaimRequest
	binds     []fabricclient.BindRequest
	begun     []string
	acked     []string
	unknown   []string
	released  []string
	submitted []fabricclient.MessageRequest
}

func newFakeFabric() *fakeFabric {
	return &fakeFabric{bindings: map[string]fabricclient.Binding{}, claims: map[string][]fabricclient.Claim{}}
}

func (f *fakeFabric) Register(_ context.Context, adapterID, instanceID string, _ time.Duration) (fabricclient.Lease, error) {
	return fabricclient.Lease{AdapterID: adapterID, InstanceID: instanceID, LeaseToken: "lease"}, nil
}
func (f *fakeFabric) Renew(_ context.Context, adapterID, token string, _ time.Duration) (fabricclient.Lease, error) {
	return fabricclient.Lease{AdapterID: adapterID, LeaseToken: token}, nil
}
func (f *fakeFabric) RegisterSessionAddress(_ context.Context, r fabricclient.SessionAddressRequest) (fabricclient.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	address := "s/" + r.NativeKey
	binding := fabricclient.Binding{Address: address, Bound: true, AdapterID: r.AdapterID, TargetRef: r.NativeKey, Generation: 1, Revision: 1, Capabilities: r.Capabilities}
	f.bindings[address] = binding
	return binding, nil
}
func (f *fakeFabric) Resolve(_ context.Context, address string) (fabricclient.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	binding, ok := f.bindings[address]
	if !ok {
		return fabricclient.Binding{}, &fabricclient.HTTPError{Status: 404, Body: `{"code":"not_found"}`}
	}
	return binding, nil
}
func (f *fakeFabric) Bind(_ context.Context, address string, r fabricclient.BindRequest) (fabricclient.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binds = append(f.binds, r)
	old := f.bindings[address]
	generation := old.Generation
	if !old.Bound || old.TargetRef != r.TargetRef {
		generation++
	}
	binding := fabricclient.Binding{Address: address, Bound: true, AdapterID: r.AdapterID, TargetRef: r.TargetRef, Revision: old.Revision + 1, Generation: generation, Capabilities: r.Capabilities}
	f.bindings[address] = binding
	return binding, nil
}
func (f *fakeFabric) Claim(_ context.Context, r fabricclient.ClaimRequest) (fabricclient.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimReqs = append(f.claimReqs, r)
	next := f.claims[r.RecipientAddress]
	if len(next) == 0 {
		return fabricclient.Claim{Reason: "no_work"}, nil
	}
	f.claims[r.RecipientAddress] = next[1:]
	return next[0], nil
}
func (f *fakeFabric) Begin(_ context.Context, id string, r fabricclient.DispatchRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begun = append(f.begun, id+" "+r.NativeAttemptRef)
	return fabricclient.Delivery{DeliveryID: id, State: "dispatching"}, nil
}
func (f *fakeFabric) Release(_ context.Context, id string, _ fabricclient.ClaimTokenRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	return fabricclient.Delivery{DeliveryID: id, State: "queued"}, nil
}
func (f *fakeFabric) Acknowledge(_ context.Context, id string, _ fabricclient.ReconcileRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, id)
	return fabricclient.Delivery{DeliveryID: id, State: "delivered"}, nil
}
func (f *fakeFabric) Unknown(_ context.Context, id string, _ fabricclient.ReconcileRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unknown = append(f.unknown, id)
	return fabricclient.Delivery{DeliveryID: id, State: "outcome_unknown"}, nil
}

func (f *fakeFabric) Submit(_ context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, request)
	return fabricclient.SubmittedMessage{Message: fabricclient.Message{MessageID: "m-" + request.OperationID, SenderAddress: request.SenderAddress, RecipientAddress: request.RecipientAddress}}, nil
}

func claimFor(address, deliveryID, action string) fabricclient.Claim {
	return fabricclient.Claim{
		Claimed:        true,
		ClaimToken:     "token-" + deliveryID,
		DispatchAction: action,
		Message:        &fabricclient.Message{MessageID: "m-" + deliveryID, Body: "hello", SenderAddress: "system/test", RecipientAddress: address},
		Delivery:       &fabricclient.Delivery{DeliveryID: deliveryID, RecipientAddress: address, State: "claimed", DispatchAction: action},
	}
}

func testDriver(t *testing.T, fabric *fakeFabric, native *fakeNative, mappings ...Mapping) *Driver {
	t.Helper()
	cfg := Defaults()
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	cfg.Mappings = mappings
	d, err := NewDriver(fabric, time.Now, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Attach(native)
	return d
}

// loadedThread makes id a loaded thread with status and registers it.
func loadedThread(t *testing.T, d *Driver, native *fakeNative, id, status string) string {
	t.Helper()
	native.loaded = append(native.loaded, id)
	native.threads[id] = NativeThread{ID: id, Status: status, AcceptsInput: true}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	address, ok := d.Address(id)
	if !ok {
		t.Fatalf("thread %s was not registered", id)
	}
	return address
}

func TestNextStepSteersTheActiveTurn(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "active")
	native.active["thread-1"] = "turn-7"
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_next_step")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := native.callsWith("steer"); !slices.Equal(got, []string{"steer thread-1 turn-7 crew-delivery:d1"}) {
		t.Fatalf("steer calls = %v", got)
	}
	if got := native.callsWith("queue "); len(got) != 0 {
		t.Fatalf("a steered message was also queued: %v", got)
	}
	if !slices.Equal(fabric.begun, []string{"d1 codex:d1"}) || !slices.Equal(fabric.acked, []string{"d1"}) {
		t.Fatalf("begun %v acked %v", fabric.begun, fabric.acked)
	}
	if last := fabric.claimReqs[len(fabric.claimReqs)-1]; last.Availability != "busy" {
		t.Fatalf("claimed an active thread as %q", last.Availability)
	}
}

func TestNextStepQueuesWhenTheTurnEndedBeforeTheSteer(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "active")
	native.active["thread-1"] = "turn-7"
	native.steerErr = &rpcError{Code: -32600, Message: "no active turn to steer"}
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_next_step")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := native.callsWith("queue "); !slices.Equal(got, []string{"queue thread-1 crew-delivery:d1"}) {
		t.Fatalf("queue calls = %v", got)
	}
	if !slices.Equal(fabric.acked, []string{"d1"}) || len(fabric.unknown) != 0 {
		t.Fatalf("acked %v unknown %v", fabric.acked, fabric.unknown)
	}
}

func TestNextStepQueuesWhenNoTurnIsActiveAnyMore(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "active")
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_next_step")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(native.callsWith("steer")) != 0 || len(native.callsWith("queue ")) != 1 {
		t.Fatalf("calls = %v", native.calls)
	}
}

func TestIdleThreadGetsItsMessageQueued(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "idle")
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_at_idle")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := native.callsWith("queue "); !slices.Equal(got, []string{"queue thread-1 crew-delivery:d1"}) {
		t.Fatalf("queue calls = %v", got)
	}
	if len(native.callsWith("steer")) != 0 || len(native.callsWith("resume")) != 0 || len(native.callsWith("active")) != 0 {
		t.Fatalf("idle delivery touched the turn: %v", native.calls)
	}
	if last := fabric.claimReqs[len(fabric.claimReqs)-1]; last.Availability != "idle" {
		t.Fatalf("claimed an idle thread as %q", last.Availability)
	}
	if !slices.Equal(fabric.acked, []string{"d1"}) {
		t.Fatalf("acked %v", fabric.acked)
	}
}

func TestNextTurnOnABusyThreadIsQueuedNotSteered(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "active")
	native.active["thread-1"] = "turn-7"
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "register_next_turn")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(native.callsWith("steer")) != 0 || len(native.callsWith("queue ")) != 1 {
		t.Fatalf("calls = %v", native.calls)
	}
}

func TestNotLoadedThreadIsQueuedThenResumed(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	if _, err := d.RegisterThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	address, _ := d.Address("thread-1")
	native.threads["thread-1"] = NativeThread{ID: "thread-1", Status: "notLoaded", AcceptsInput: true}
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "wake_then_deliver")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, call := range native.calls {
		if strings.HasPrefix(call, "queue ") || strings.HasPrefix(call, "resume") {
			order = append(order, call)
		}
	}
	if !slices.Equal(order, []string{"queue thread-1 crew-delivery:d1", "resume thread-1"}) {
		t.Fatalf("native calls = %v", order)
	}
	if fabric.claimReqs[0].Availability != "inactive" {
		t.Fatalf("claimed a not-loaded thread as %q", fabric.claimReqs[0].Availability)
	}
	if !slices.Equal(fabric.acked, []string{"d1"}) {
		t.Fatalf("acked %v", fabric.acked)
	}
}

func TestAmbiguousQueueAddIsReconciledFromTheNativeQueue(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "idle")
	native.queueErr, native.queueKeeps = io.ErrUnexpectedEOF, true
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_at_idle")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := native.callsWith("queue "); len(got) != 1 {
		t.Fatalf("an ambiguous queue/add was retried: %v", got)
	}
	if !slices.Equal(fabric.acked, []string{"d1"}) || len(fabric.unknown) != 0 {
		t.Fatalf("acked %v unknown %v", fabric.acked, fabric.unknown)
	}
}

func TestAmbiguousSteerIsReconciledFromRecentTurnsWithoutQueueing(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "active")
	native.active["thread-1"] = "turn-7"
	native.steerErr, native.steerKeeps = context.DeadlineExceeded, true
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_next_step")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(native.callsWith("queue ")) != 0 {
		t.Fatalf("an ambiguous steer fell back to the queue: %v", native.calls)
	}
	if !slices.Equal(fabric.acked, []string{"d1"}) {
		t.Fatalf("acked %v unknown %v", fabric.acked, fabric.unknown)
	}
}

func TestUnprovableDispatchBecomesUnknownAndIsNotResent(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "idle")
	native.queueErr = errors.New("connection reset")
	fabric.claims[address] = []fabricclient.Claim{claimFor(address, "d1", "deliver_at_idle")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(native.callsWith("queue ")) != 1 || len(fabric.acked) != 0 || !slices.Equal(fabric.unknown, []string{"d1"}) {
		t.Fatalf("queue %v acked %v unknown %v", native.callsWith("queue "), fabric.acked, fabric.unknown)
	}
}

func TestInFlightDispatchFromACrashIsSettledFromCodex(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-1", "idle")
	native.turns["thread-1"] = []NativeTurn{{ID: "turn-1", Status: "completed", ClientIDs: []string{ClientMessageID("d0")}}}
	fabric.claims[address] = []fabricclient.Claim{{Reason: "head_dispatching", Head: &fabricclient.Delivery{DeliveryID: "d0", State: "dispatching", NativeAttemptRef: "codex:d0", ClaimOwnerAdapterID: "crew-codex"}}}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fabric.acked, []string{"d0"}) || len(native.callsWith("queue ")) != 0 {
		t.Fatalf("acked %v calls %v", fabric.acked, native.calls)
	}
}

func TestDiscoverySkipsThreadsThatCannotTakeInput(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	native.loaded = []string{"eph", "sub", "user"}
	native.threads["eph"] = NativeThread{ID: "eph", Status: "idle", Ephemeral: true, AcceptsInput: true}
	native.threads["sub"] = NativeThread{ID: "sub", Status: "idle"}
	native.threads["user"] = NativeThread{ID: "user", Status: "idle", AcceptsInput: true}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Address("eph"); ok {
		t.Fatal("registered an ephemeral thread")
	}
	if _, ok := d.Address("sub"); ok {
		t.Fatal("registered a thread that refuses direct input")
	}
	if _, ok := d.Address("user"); !ok {
		t.Fatal("did not register a loaded user thread")
	}
}

func TestThreadStartedNotificationRegistersTheThread(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	native.threads["new"] = NativeThread{ID: "new", Status: "idle", AcceptsInput: true}
	d.Notify("thread/started", json.RawMessage(`{"thread":{"id":"new"}}`))
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Address("new"); !ok {
		t.Fatal("thread/started did not register the thread")
	}
}

func TestPinnedAddressKeepsItsTargetAndGainsCapabilities(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	fabric.bindings["codex/dsh-crew"] = fabricclient.Binding{Address: "codex/dsh-crew", Bound: true, AdapterID: "crew-codex", TargetRef: "old-session", Revision: 9, Generation: 1, Capabilities: []string{"deliver_when_idle"}}
	d := testDriver(t, fabric, native, Mapping{Address: "codex/dsh-crew", ThreadID: "thread-pinned"}, Mapping{Address: "codex/new", ThreadID: "thread-new"})
	d.ensurePinned(context.Background())
	pinned := fabric.bindings["codex/dsh-crew"]
	if pinned.TargetRef != "old-session" || pinned.Generation != 1 || !sameCapabilities(pinned.Capabilities, Capabilities) {
		t.Fatalf("pinned binding = %+v", pinned)
	}
	if fresh := fabric.bindings["codex/new"]; fresh.TargetRef != "thread-new" || !fresh.Bound {
		t.Fatalf("new pinned binding = %+v", fresh)
	}
	native.loaded = []string{"thread-pinned"}
	native.threads["thread-pinned"] = NativeThread{ID: "thread-pinned", Status: "idle", AcceptsInput: true}
	fabric.claims["codex/dsh-crew"] = []fabricclient.Claim{claimFor("codex/dsh-crew", "d1", "deliver_at_idle")}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := native.callsWith("queue "); !slices.Equal(got, []string{"queue thread-pinned crew-delivery:d1"}) {
		t.Fatalf("queue calls = %v", got)
	}
}

func TestRegisteredThreadsSurviveARestart(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	if _, err := d.RegisterThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	again, err := NewDriver(fabric, time.Now, d.cfg, d.log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Address("thread-1"); !ok {
		t.Fatal("restarted driver forgot the thread")
	}
}

func TestSessionStartHookReturnsTheAddress(t *testing.T) {
	fabric, native := newFakeFabric(), newFakeNative()
	d := testDriver(t, fabric, native)
	server := httptestServer(t, Handler(d))
	out := RunHook(context.Background(), server, "session-start", HookInput{SessionID: "thread-1"})
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	if parsed.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(parsed.HookSpecificOutput.AdditionalContext, "s/thread-1") {
		t.Fatalf("hook output = %s", out)
	}
	fill := RunHook(context.Background(), server, "fill-notify", HookInput{SessionID: "thread-1", ToolInput: json.RawMessage(`{"task_id":1}`)})
	if !strings.Contains(string(fill), `"notify_address":"s/thread-1"`) || !strings.Contains(string(fill), `"permissionDecision":"allow"`) {
		t.Fatalf("fill-notify output = %s", fill)
	}
	if out := RunHook(context.Background(), "http://127.0.0.1:1", "session-start", HookInput{SessionID: "thread-1"}); out != nil {
		t.Fatalf("hook printed %q with the driver down", out)
	}
}

func TestSendGoesOutFromTheThreadsOwnAddressAndWhoListsIt(t *testing.T) {
	fabric := newFakeFabric()
	native := newFakeNative()
	d := testDriver(t, fabric, native)
	address := loadedThread(t, d, native, "thread-send", "idle")
	native.threads["thread-send"] = NativeThread{ID: "thread-send", CWD: "/work", Status: "idle", AcceptsInput: true}
	if err := d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := d.Send(context.Background(), crewcli.SendRequest{From: "thread-send", To: "s/other", Body: "hello", Policy: "next_step", OperationID: "op-1"})
	if err != nil || result.From != address || result.To != "s/other" {
		t.Fatalf("send=%#v %v", result, err)
	}
	if got := fabric.submitted; len(got) != 1 || got[0].SenderAddress != address || got[0].ProducerID != d.cfg.AdapterID || got[0].ActivationPolicy != "next_step" || got[0].OperationID != "op-1" {
		t.Fatalf("submitted=%#v", got)
	}
	var found bool
	for _, s := range d.Sessions() {
		if s.NativeID == "thread-send" {
			found = s.Address == address && s.Cwd == "/work" && s.State == "idle" && s.Runtime == "codex"
		}
	}
	if !found {
		t.Fatalf("who=%#v", d.Sessions())
	}
	if _, err := d.Send(context.Background(), crewcli.SendRequest{From: "thread-send", To: "s/other", OperationID: "op-2"}); err == nil {
		t.Fatal("empty body accepted")
	}
}
