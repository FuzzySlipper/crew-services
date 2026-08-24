package codexadapter

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestProjectorCanonicalReplayDoesNotDuplicateEntries(t *testing.T) {
	fabric := newFakeFabric()
	native := &fakeAppServer{threads: []NativeThread{thread("thread-1", "Scout", "idle", []NativeTurn{{ID: "turn-1", Status: "completed", Items: []NativeItem{
		{ID: "user-1", Type: "userMessage", Content: []NativeContent{{Type: "text", Text: "survey this"}}},
		{ID: "agent-1", Type: "agentMessage", Text: "done"},
		{ID: "tool-1", Type: "commandExecution", Text: "ignored"},
	}}})}}
	projector := newProjector(fabric, native)
	if err := projector.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := projector.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fabric.events); got != 2 {
		t.Fatalf("events=%d, want 2", got)
	}
	if got := fabric.sessions["thread-1"].SessionID; got != "session-1" {
		t.Fatalf("session ID=%q", got)
	}
	if got := fabric.bindings["crew/scout"].TargetRef; got != "session-1" {
		t.Fatalf("public target=%q", got)
	}
	if native.closed {
		t.Fatal("reconciliation must not close a healthy native reader")
	}

	// A new adapter process has no local checkpoint. Stable operation IDs make
	// the full canonical history replay a no-op in the fabric.
	restarted := newProjector(fabric, native)
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fabric.events); got != 2 {
		t.Fatalf("restart events=%d, want 2", got)
	}
	for _, event := range fabric.events {
		if event.PayloadContains("thread-1") {
			t.Fatalf("native thread ID leaked into entry payload: %s", event.OperationID)
		}
	}
}

func TestProjectorUpdatesPresentationWithoutChangingIdentity(t *testing.T) {
	fabric := newFakeFabric()
	native := &fakeAppServer{threads: []NativeThread{thread("thread-1", "Before", "idle", nil)}}
	projector := newProjector(fabric, native)
	if err := projector.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := fabric.sessions["thread-1"]
	native.threads[0] = thread("thread-1", "After", "active", nil)
	native.threads[0].CWD = "/new/workspace"
	if err := projector.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fabric.sessions["thread-1"]
	if got.SessionID != first.SessionID {
		t.Fatalf("session identity changed: %q -> %q", first.SessionID, got.SessionID)
	}
	if got.Label != "After" || got.Location != "/new/workspace" || got.Status != "active" || got.Revision <= first.Revision {
		t.Fatalf("presentation update=%+v", got)
	}
}

func TestProjectorRejectsForeignAddressAndDuplicateMapping(t *testing.T) {
	cfg := Defaults()
	cfg.Mappings = []Mapping{{Address: "crew/a", ThreadID: "thread-1"}, {Address: "crew/b", ThreadID: "thread-1"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate native mapping was accepted")
	}
	fabric := newFakeFabric()
	fabric.bindings["crew/scout"] = Binding{Address: "crew/scout", Bound: true, AdapterID: "other-adapter", TargetRef: "other-session", Revision: 1}
	projector := newProjector(fabric, &fakeAppServer{threads: []NativeThread{thread("thread-1", "Scout", "idle", nil)}})
	if err := projector.Reconcile(context.Background()); err == nil {
		t.Fatal("foreign binding was overwritten")
	}
}

func TestNormalizedEventsOmitActiveAgentMessageAndUnsupportedItems(t *testing.T) {
	events := normalizedEvents(thread("thread", "", "active", []NativeTurn{{ID: "turn", Status: "inProgress", Items: []NativeItem{
		{ID: "user", Type: "userMessage", Content: []NativeContent{{Type: "text", Text: "hello"}}},
		{ID: "agent", Type: "agentMessage", Text: "partial"},
		{ID: "reason", Type: "reasoning"},
	}}}))
	want := []Entry{{EntryID: "user", TurnID: "turn", Author: "user", Category: "message", Text: "hello", Completed: true, Final: true}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%+v, want %+v", events, want)
	}
}

func TestRunStopsNativeWhileWaitingForNextPoll(t *testing.T) {
	fabric := newFakeFabric()
	native := &fakeAppServer{threads: []NativeThread{thread("thread-1", "Scout", "idle", nil)}, read: make(chan struct{})}
	cfg := Defaults()
	cfg.Mappings = []Mapping{{Address: "crew/scout", ThreadID: "thread-1"}}
	cfg.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, fabric, func() (AppServer, error) { return native, nil }, nil) }()
	select {
	case <-native.read:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop promptly")
	}
	if !native.closed {
		t.Fatal("Run did not close native App Server")
	}
}

func TestProjectorReadsExactMappingOutsideThreadListPage(t *testing.T) {
	fabric := newFakeFabric()
	native := &fakeAppServer{
		threads:       []NativeThread{thread("thread-1", "Scout", "idle", nil)},
		listedThreads: []NativeThread{},
	}
	if err := newProjector(fabric, native).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fabric.sessions) != 1 {
		t.Fatalf("sessions=%d, want configured thread projection", len(fabric.sessions))
	}
}

func TestRunMaintainsLeaseAcrossNativeOutageAndRecovers(t *testing.T) {
	fabric := &outageFabric{fakeFabric: newFakeFabric(), appended: make(chan struct{})}
	native := &outageNative{thread: thread("thread-1", "Scout", "idle", []NativeTurn{{ID: "turn", Status: "completed", Items: []NativeItem{{ID: "entry", Type: "agentMessage", Text: "recovered"}}}}), failuresRemaining: 8}
	cfg := Defaults()
	cfg.Mappings = []Mapping{{Address: "crew/scout", ThreadID: "thread-1"}}
	cfg.LeaseDuration = 20 * time.Millisecond
	cfg.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, fabric, func() (AppServer, error) { return native, nil }, nil) }()
	select {
	case <-fabric.appended:
	case <-time.After(time.Second):
		t.Fatal("projection did not recover after native outage")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if fabric.renewals == 0 || fabric.registrations < 2 {
		t.Fatalf("lease was not renewed/recovered: renewals=%d registrations=%d", fabric.renewals, fabric.registrations)
	}
	if native.reads > 20 {
		t.Fatalf("native outage spun too quickly: reads=%d", native.reads)
	}
}

type fakeAppServer struct {
	threads       []NativeThread
	listedThreads []NativeThread
	closed        bool
	init          bool
	read          chan struct{}
}

func (f *fakeAppServer) Initialize(context.Context) error { f.init = true; return nil }
func (f *fakeAppServer) ListThreads(context.Context) ([]NativeThread, error) {
	if f.listedThreads != nil {
		return append([]NativeThread(nil), f.listedThreads...), nil
	}
	return append([]NativeThread(nil), f.threads...), nil
}
func (f *fakeAppServer) ReadThread(_ context.Context, id string) (NativeThread, error) {
	if f.read != nil {
		select {
		case <-f.read:
		default:
			close(f.read)
		}
	}
	for _, value := range f.threads {
		if value.ID == id {
			return value, nil
		}
	}
	return NativeThread{}, errors.New("missing thread")
}

type outageNative struct {
	thread                   NativeThread
	failuresRemaining, reads int
	closed                   bool
}

func (n *outageNative) Initialize(context.Context) error                    { return nil }
func (n *outageNative) ListThreads(context.Context) ([]NativeThread, error) { return nil, nil }
func (n *outageNative) ReadThread(context.Context, string) (NativeThread, error) {
	n.reads++
	if n.failuresRemaining > 0 {
		n.failuresRemaining--
		return NativeThread{}, errors.New("native unavailable")
	}
	return n.thread, nil
}
func (n *outageNative) Close() error  { n.closed = true; return nil }
func (f *fakeAppServer) Close() error { f.closed = true; return nil }

type fakeFabric struct {
	sessions   map[string]Session
	bindings   map[string]Binding
	events     []fakeEvent
	operations map[string]struct{}
}

type outageFabric struct {
	*fakeFabric
	registrations, renewals int
	appended                chan struct{}
}

func (f *outageFabric) Register(ctx context.Context, adapter, instance string, duration time.Duration) (Lease, error) {
	f.registrations++
	lease, err := f.fakeFabric.Register(ctx, adapter, instance, duration)
	if f.registrations == 1 {
		lease.ExpiresAt = time.Now().Add(30 * time.Millisecond)
	} else {
		lease.ExpiresAt = time.Now().Add(time.Hour)
	}
	return lease, err
}
func (f *outageFabric) Renew(context.Context, string, string, time.Duration) (Lease, error) {
	f.renewals++
	return Lease{}, errors.New("lease expired")
}
func (f *outageFabric) Append(ctx context.Context, sessionID string, request AppendRequest) error {
	err := f.fakeFabric.Append(ctx, sessionID, request)
	if err == nil {
		select {
		case <-f.appended:
		default:
			close(f.appended)
		}
	}
	return err
}

type fakeEvent struct {
	SessionID, OperationID string
	Payload                []byte
}

func (e fakeEvent) PayloadContains(value string) bool {
	return string(e.Payload) == value || len(value) > 0 && contains(string(e.Payload), value)
}
func contains(text, value string) bool {
	for index := 0; index+len(value) <= len(text); index++ {
		if text[index:index+len(value)] == value {
			return true
		}
	}
	return false
}
func newFakeFabric() *fakeFabric {
	return &fakeFabric{sessions: map[string]Session{}, bindings: map[string]Binding{}, operations: map[string]struct{}{}}
}
func (f *fakeFabric) Register(_ context.Context, adapter, instance string, _ time.Duration) (Lease, error) {
	return Lease{AdapterID: adapter, InstanceID: instance, LeaseToken: "lease"}, nil
}
func (f *fakeFabric) Renew(_ context.Context, adapter, token string, _ time.Duration) (Lease, error) {
	return Lease{AdapterID: adapter, LeaseToken: token}, nil
}
func (f *fakeFabric) Adopt(_ context.Context, request AdoptRequest) (Session, error) {
	if old, found := f.sessions[request.AdapterKey]; found {
		return old, nil
	}
	session := Session{SessionID: "session-" + string(rune(len(f.sessions)+49)), AdapterID: request.AdapterID, Label: request.Label, Location: request.Location, Status: request.Status, Capabilities: request.Capabilities, Revision: 1}
	f.sessions[request.AdapterKey] = session
	return session, nil
}
func (f *fakeFabric) Update(_ context.Context, request UpdateRequest) (Session, error) {
	for key, old := range f.sessions {
		if old.SessionID == request.SessionID {
			if old.Revision != request.ExpectedRevision {
				return Session{}, errors.New("stale")
			}
			old.Label, old.Location, old.Status, old.Capabilities, old.Revision = request.Label, request.Location, request.Status, request.Capabilities, old.Revision+1
			f.sessions[key] = old
			return old, nil
		}
	}
	return Session{}, errors.New("not found")
}
func (f *fakeFabric) Resolve(_ context.Context, address string) (Binding, error) {
	if b, found := f.bindings[address]; found {
		return b, nil
	}
	return Binding{}, &HTTPError{Status: 404}
}
func (f *fakeFabric) Bind(_ context.Context, address string, request BindRequest) (Binding, error) {
	if old, found := f.bindings[address]; found && old.Bound && (old.AdapterID != request.AdapterID || old.TargetRef != request.TargetRef) {
		return Binding{}, errors.New("foreign")
	}
	b := Binding{Address: address, Bound: true, AdapterID: request.AdapterID, TargetRef: request.TargetRef, Revision: 1, Capabilities: request.Capabilities}
	f.bindings[address] = b
	return b, nil
}
func (f *fakeFabric) Append(_ context.Context, sessionID string, request AppendRequest) error {
	if _, found := f.operations[request.OperationID]; found {
		return nil
	}
	f.operations[request.OperationID] = struct{}{}
	f.events = append(f.events, fakeEvent{SessionID: sessionID, OperationID: request.OperationID, Payload: append([]byte(nil), request.Payload...)})
	return nil
}

func newProjector(fabric Fabric, native AppServer) *Projector {
	return &Projector{Fabric: fabric, Native: native, AdapterID: "crew-codex", Lease: Lease{LeaseToken: "lease"}, Mappings: []Mapping{{Address: "crew/scout", ThreadID: "thread-1"}}, Capabilities: []string{"session-events", "read-only-native-history"}}
}
func thread(id, name, status string, turns []NativeTurn) NativeThread {
	return NativeThread{ID: id, Name: name, CWD: "/workspace", Status: status, Turns: turns}
}
