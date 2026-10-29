package dshdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// fakeSession is one session of the fake web host.
type fakeSession struct {
	summary  SessionSummary
	active   bool
	running  bool
	inbox    []fakeItem
	gap      []fakeItem // taken from the inbox, user/message not yet written
	events   []Event
	turns    int
	notFound bool
}

type fakeItem struct {
	rpcID string
	mode  string
}

// fakeHost is a DSH web host speaking the session API over HTTP: launch-token
// connect, cookie auth, session/list, state, events and prompt. Prompts are
// deduplicated as the real host does, on the inbox and the user messages
// only, so a repeat in the inbox-to-history gap is admitted again.
type fakeHost struct {
	t      *testing.T
	mu     sync.Mutex
	token  string
	cookie string
	gen    int
	server *httptest.Server

	sessions map[string]*fakeSession
	order    []string
	// prompts records every session/prompt call as "session mode rpcId".
	prompts []string
	// onPrompt, when set, runs before a prompt is handled. It returns how to
	// answer: "" handles it normally, "drop-before" closes the connection
	// without admitting, "drop-after" admits and then closes the connection,
	// "drop-after-take" admits, takes the item into the gap and closes the
	// connection, "slow" admits and answers only after the client gave up,
	// or a host error code to refuse with.
	onPrompt func(n int, sessionID string) string
	// onState runs on every session/state call with its 1-based count.
	onState  func(n int, s *fakeSession)
	states   int
	connects int
}

func newFakeHost(t *testing.T) *fakeHost {
	h := &fakeHost{t: t, token: "tok-1", sessions: map[string]*fakeSession{}}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *fakeHost) tokens(context.Context) (Credentials, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Credentials{BaseURL: h.server.URL, Token: h.token}, nil
}

// restart is a `dsh web` restart: a new launch token, old cookies refused.
func (h *fakeHost) restart() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.token = fmt.Sprintf("tok-%d", h.gen+1)
	h.cookie = ""
}

func (h *fakeHost) add(id string, s SessionSummary, active, running bool) *fakeSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.SessionID = id
	if s.Cwd == "" && s.Origin != "subagent" {
		s.Cwd = "/tmp/" + id
	}
	if s.UpdatedAt == 0 {
		s.UpdatedAt = time.Now().UnixMilli()
	}
	fs := &fakeSession{summary: s, active: active, running: running}
	h.sessions[id] = fs
	h.order = append(h.order, id)
	return fs
}

func (h *fakeHost) session(id string) *fakeSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *fakeHost) appendEvent(s *fakeSession, typ string, data any) {
	raw, _ := json.Marshal(data)
	s.events = append(s.events, Event{Type: typ, Seq: int64(len(s.events) + 1), Data: raw})
}

// take is the turn taking the first inbox item: it leaves the inbox before
// its user message is written.
func (h *fakeHost) take(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if len(s.inbox) == 0 {
		return
	}
	s.gap = append(s.gap, s.inbox[0])
	s.inbox = s.inbox[1:]
	s.running = true
	h.appendEvent(s, "agent/inbox/spliced", map[string]any{"removedCount": 1, "inserted": []any{}})
}

// commit writes the user messages of the taken items, each its own turn.
func (h *fakeHost) commit(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	for _, item := range s.gap {
		s.turns++
		h.appendEvent(s, "turn/start", map[string]any{"turn": s.turns})
		h.appendEvent(s, "user/message", map[string]any{"source": map[string]any{"kind": "user", "rpcId": item.rpcID}})
	}
	s.gap = nil
}

func (h *fakeHost) hasRequest(s *fakeSession, rpcID string) bool {
	for _, item := range s.inbox {
		if item.rpcID == rpcID {
			return true
		}
	}
	for _, e := range s.events {
		if e.Type != "user/message" {
			continue
		}
		var data struct {
			Source struct {
				RPCID string `json:"rpcId"`
			} `json:"source"`
		}
		if json.Unmarshal(e.Data, &data) == nil && data.Source.RPCID == rpcID {
			return true
		}
	}
	return false
}

func (h *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/connect" {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Query().Get("token") != h.token || r.URL.Query().Get("interactions") != "none" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.connects++
		h.cookie = fmt.Sprintf("op-%d-%d", h.gen, h.connects)
		http.SetCookie(w, &http.Cookie{Name: "dsh-auth", Value: h.cookie, Path: "/"})
		w.Header().Set("Location", "./api/remote.mux")
		w.WriteHeader(http.StatusFound)
		return
	}
	cookie, err := r.Cookie("dsh-auth")
	h.mu.Lock()
	valid := err == nil && h.cookie != "" && cookie.Value == h.cookie
	h.mu.Unlock()
	if !valid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var envelope struct {
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args map[string]json.RawMessage `json:"args"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil || "/api/"+envelope.Method != r.URL.Path {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	value, code := h.handle(w, envelope.Method, envelope.Payload.Args)
	if code == "hijacked" {
		return
	}
	result := map[string]any{"ok": true, "value": value}
	if code != "" {
		result = map[string]any{"ok": false, "error": map[string]any{"code": code, "message": "refused by the fake host"}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "server-response", "rpcId": envelope.RPCID, "result": result})
}

func hangUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (h *fakeHost) handle(w http.ResponseWriter, method string, args map[string]json.RawMessage) (any, string) {
	var request struct {
		SessionID string `json:"sessionId"`
		RequestID string `json:"requestId"`
		Mode      string `json:"mode"`
		Address   struct {
			SessionID string `json:"sessionId"`
		} `json:"address"`
	}
	_ = json.Unmarshal(args["request"], &request)
	switch method {
	case "session/list":
		h.mu.Lock()
		defer h.mu.Unlock()
		items := []SessionSummary{}
		for _, id := range h.order {
			s := h.sessions[id]
			if s.notFound {
				continue
			}
			summary := s.summary
			summary.AgentAvailable, summary.Running = s.active, s.running
			items = append(items, summary)
		}
		return map[string]any{"items": items}, ""
	case "session/state":
		h.mu.Lock()
		defer h.mu.Unlock()
		s, ok := h.sessions[request.SessionID]
		if !ok || s.notFound {
			return nil, "session/not-found"
		}
		h.states++
		if h.onState != nil {
			h.onState(h.states, s)
		}
		if !s.active {
			return SessionState{Cursor: int64(len(s.events)), PendingInput: []string{}}, ""
		}
		queue := []QueuedItem{}
		for i, item := range s.inbox {
			placement := "queued"
			if item.mode == "steer" {
				placement = "steering"
			}
			queue = append(queue, QueuedItem{ID: fmt.Sprintf("q%d", i), Placement: placement, RPCID: item.rpcID})
		}
		return SessionState{Cursor: int64(len(s.events)), Active: true, Running: s.running, Queue: queue, PendingInput: []string{}}, ""
	case "session/events":
		h.mu.Lock()
		defer h.mu.Unlock()
		s, ok := h.sessions[request.Address.SessionID]
		if !ok || s.notFound {
			return nil, "session/not-found"
		}
		records := []any{}
		for _, e := range s.events {
			records = append(records, map[string]any{"type": "event", "event": e})
		}
		return map[string]any{"records": records, "hasMore": false, "cursor": len(s.events)}, ""
	case "session/prompt":
		h.mu.Lock()
		h.prompts = append(h.prompts, request.SessionID+" "+request.Mode+" "+request.RequestID)
		n := len(h.prompts)
		hook := h.onPrompt
		h.mu.Unlock()
		how := ""
		if hook != nil {
			how = hook(n, request.SessionID)
		}
		if how == "drop-before" {
			hangUp(w)
			return nil, "hijacked"
		}
		if strings.Contains(how, "/") {
			return nil, how
		}
		h.mu.Lock()
		s, ok := h.sessions[request.SessionID]
		if !ok || s.notFound {
			h.mu.Unlock()
			return nil, "session/not-found"
		}
		s.active = true // a cold session is resumed first
		if !h.hasRequest(s, request.RequestID) {
			s.inbox = append(s.inbox, fakeItem{rpcID: request.RequestID, mode: request.Mode})
			h.appendEvent(s, "agent/inbox/spliced", map[string]any{"inserted": []any{map[string]any{"source": map[string]any{"kind": "user", "rpcId": request.RequestID}}}})
		}
		h.mu.Unlock()
		switch how {
		case "drop-after":
			hangUp(w)
			return nil, "hijacked"
		case "drop-after-take":
			h.take(request.SessionID)
			hangUp(w)
			return nil, "hijacked"
		case "slow":
			time.Sleep(300 * time.Millisecond)
		}
		return map[string]any{"accepted": true}, ""
	}
	return nil, "gateway/not-found"
}

// admitted counts the prompts with rpcID the session took in: inbox, gap and
// user messages.
func (h *fakeHost) admitted(id, rpcID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	n := 0
	for _, item := range append(append([]fakeItem{}, s.inbox...), s.gap...) {
		if item.rpcID == rpcID {
			n++
		}
	}
	for _, e := range s.events {
		if e.Type == "user/message" && strings.Contains(string(e.Data), `"`+rpcID+`"`) {
			n++
		}
	}
	return n
}

func (h *fakeHost) promptCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.prompts...)
}

// fakeFabric hands out scripted claims and records settlements.
type fakeFabric struct {
	mu        sync.Mutex
	bindings  map[string]fabricclient.Binding
	claims    map[string][]fabricclient.Claim
	claimReqs []fabricclient.ClaimRequest
	begun     []string
	acked     []string
	unknown   []string
	released  []string
	failed    []string
	requeued  []fabricclient.NotInsertedRequest
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
	binding := fabricclient.Binding{Address: address, Bound: true, AdapterID: r.AdapterID, Generation: 1, Revision: 1, Capabilities: r.Capabilities}
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
	f.begun = append(f.begun, id+" "+r.NativeAttemptRef+" "+r.OperationID)
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
func (f *fakeFabric) Fail(_ context.Context, id string, _ fabricclient.ReconcileRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, id)
	return fabricclient.Delivery{DeliveryID: id, State: "failed"}, nil
}
func (f *fakeFabric) NotInserted(_ context.Context, id string, r fabricclient.NotInsertedRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requeued = append(f.requeued, r)
	return fabricclient.Delivery{DeliveryID: id, State: "queued"}, nil
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

func (f *fakeFabric) queue(address string, claims ...fabricclient.Claim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims[address] = append(f.claims[address], claims...)
}

func claimFor(address, deliveryID, action, policy string) fabricclient.Claim {
	return fabricclient.Claim{
		Claimed:        true,
		ClaimToken:     "token-" + deliveryID,
		DispatchAction: action,
		Message:        &fabricclient.Message{MessageID: "m-" + deliveryID, Body: "hello", SenderAddress: "system/test", RecipientAddress: address, ActivationPolicy: policy},
		Delivery:       &fabricclient.Delivery{DeliveryID: deliveryID, RecipientAddress: address, State: "claimed", DispatchAction: action, AttemptCount: 1},
	}
}

type harness struct {
	host   *fakeHost
	fabric *fakeFabric
	d      *Driver
	// duringWait runs while the driver waits out the reconcile window.
	duringWait func()
	waits      int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{host: newFakeHost(t), fabric: newFakeFabric()}
	web := NewWebHost(h.host.tokens)
	web.promptTimeout = 150 * time.Millisecond
	h.d = NewDriver(h.fabric, web, time.Now, Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.d.sleep = func(ctx context.Context, _ time.Duration) error {
		h.waits++
		if h.duringWait != nil {
			h.duringWait()
		}
		return nil
	}
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) pump(t *testing.T) {
	t.Helper()
	if err := h.d.Pump(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryRegistersRootSessionsOnly(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-root", SessionSummary{}, true, false)
	h.host.add("session-cold", SessionSummary{}, false, false)
	h.host.add("child", SessionSummary{Origin: "subagent", ParentSessionID: "session-root"}, false, false)
	h.host.add("session-blank", SessionSummary{Blank: true}, true, false)
	h.host.add("session-old", SessionSummary{UpdatedAt: time.Now().Add(-30 * 24 * time.Hour).UnixMilli()}, false, false)
	h.pump(t)
	got := map[string]string{}
	for _, s := range h.d.Sessions() {
		got[s.NativeID] = s.State
	}
	want := map[string]string{"session-root": "idle", "session-cold": "not loaded"}
	if len(got) != len(want) || got["session-root"] != "idle" || got["session-cold"] != "not loaded" {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
	// A subagent's commands send as its parent session.
	address, err := h.d.Address(context.Background(), "child")
	if err != nil || address != "s/session-root" {
		t.Fatalf("subagent address = %q, %v", address, err)
	}
}

func TestPolicyMapsToPromptMode(t *testing.T) {
	cases := []struct {
		name, action, policy, mode string
		running, active            bool
	}{
		{"idle next_turn queues", "deliver_at_idle", "next_turn", "queue", false, true},
		{"idle next_step steers", "deliver_at_idle", "next_step", "steer", false, true},
		{"busy next_step steers", "deliver_next_step", "next_step", "steer", true, true},
		{"busy next_turn queues", "register_next_turn", "next_turn", "queue", true, true},
		{"busy no_wake steers", "deliver_next_step", "no_wake", "steer", true, true},
		{"cold next_turn queues and resumes", "wake_then_deliver", "next_turn", "queue", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.host.add("session-a", SessionSummary{}, tc.active, tc.running)
			// The first pass registers the session and claims for it.
			h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", tc.action, tc.policy))
			h.pump(t)
			calls := h.host.promptCalls()
			if len(calls) != 1 || calls[0] != "session-a "+tc.mode+" crew-delivery:d1" {
				t.Fatalf("prompts = %v", calls)
			}
			if len(h.fabric.acked) != 1 || h.waits != 0 {
				t.Fatalf("acked = %v, waits = %d", h.fabric.acked, h.waits)
			}
			if !h.host.session("session-a").active {
				t.Fatal("session was not resumed")
			}
			if len(h.fabric.begun) != 1 || !strings.HasPrefix(h.fabric.begun[0], "d1 dsh:d1 crew-dsh/begin/d1") {
				t.Fatalf("begun = %v", h.fabric.begun)
			}
		})
	}
}

func TestAvailabilityFollowsTheSessionList(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-busy", SessionSummary{}, true, true)
	h.host.add("session-idle", SessionSummary{}, true, false)
	h.host.add("session-cold", SessionSummary{}, false, false)
	h.pump(t) // pass 1 claims cold sessions too
	got := map[string]string{}
	for _, r := range h.fabric.claimReqs {
		got[r.RecipientAddress] = r.Availability
	}
	if got["s/session-busy"] != "busy" || got["s/session-idle"] != "idle" || got["s/session-cold"] != "inactive" {
		t.Fatalf("availability = %v", got)
	}
	h.fabric.claimReqs = nil
	h.pump(t) // pass 2 skips cold sessions
	for _, r := range h.fabric.claimReqs {
		if r.RecipientAddress == "s/session-cold" {
			t.Fatal("cold session claimed on every pass")
		}
	}
}

func TestNoWakeNeverStartsATurn(t *testing.T) {
	t.Run("idle at claim: released, nothing sent", func(t *testing.T) {
		h := newHarness(t)
		h.host.add("session-a", SessionSummary{}, true, true)
		h.pump(t)
		h.host.session("session-a").running = false // the turn ended after the list
		h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_next_step", "no_wake"))
		h.pump(t)
		if len(h.host.promptCalls()) != 0 || len(h.fabric.released) != 1 || len(h.fabric.begun) != 0 {
			t.Fatalf("prompts %v released %v begun %v", h.host.promptCalls(), h.fabric.released, h.fabric.begun)
		}
	})
	t.Run("turn ends after Begin: not inserted", func(t *testing.T) {
		h := newHarness(t)
		h.host.add("session-a", SessionSummary{}, true, true)
		h.pump(t)
		h.host.onState = func(n int, s *fakeSession) {
			if n == 2 { // the re-check right before the steer
				s.running = false
			}
		}
		h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_next_step", "no_wake"))
		h.pump(t)
		if len(h.host.promptCalls()) != 0 || len(h.fabric.begun) != 1 || len(h.fabric.requeued) != 1 || h.fabric.requeued[0].Reason != "turn_ended" {
			t.Fatalf("prompts %v begun %v requeued %+v", h.host.promptCalls(), h.fabric.begun, h.fabric.requeued)
		}
		if len(h.fabric.acked)+len(h.fabric.unknown) != 0 {
			t.Fatal("a not-inserted delivery was also settled")
		}
	})
}

func TestAmbiguousPromptInsideTheWindowIsReconciledNotResent(t *testing.T) {
	// The host admits the prompt, the turn takes it out of the inbox, and the
	// response is lost before the user message is written: an immediate
	// repeat would land in the gap and run a second turn.
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(n int, _ string) string { return "drop-after-take" }
	h.duringWait = func() { h.host.commit("session-a") }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if calls := h.host.promptCalls(); len(calls) != 1 {
		t.Fatalf("prompt sent %d times: %v", len(calls), calls)
	}
	if h.waits != 1 || len(h.fabric.acked) != 1 || len(h.fabric.unknown) != 0 {
		t.Fatalf("waits %d acked %v unknown %v", h.waits, h.fabric.acked, h.fabric.unknown)
	}
	if n := h.host.admitted("session-a", "crew-delivery:d1"); n != 1 || h.host.session("session-a").turns != 1 {
		t.Fatalf("admitted %d times, %d turns", n, h.host.session("session-a").turns)
	}
}

func TestAmbiguousPromptStillInTheGapCountsAsAdmitted(t *testing.T) {
	// Even if the window has not closed when the check runs, the inbox
	// admission is in the session log.
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(int, string) string { return "drop-after-take" }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if len(h.host.promptCalls()) != 1 || len(h.fabric.acked) != 1 {
		t.Fatalf("prompts %v acked %v", h.host.promptCalls(), h.fabric.acked)
	}
}

func TestAmbiguousPromptAbsentAfterTheWindowIsSentOnceMore(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(n int, _ string) string {
		if n == 1 {
			return "drop-before"
		}
		return ""
	}
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	calls := h.host.promptCalls()
	if len(calls) != 2 || calls[0] != calls[1] {
		t.Fatalf("prompts = %v, want the same request twice", calls)
	}
	if h.waits != 1 || len(h.fabric.acked) != 1 || h.host.admitted("session-a", "crew-delivery:d1") != 1 {
		t.Fatalf("waits %d acked %v admitted %d", h.waits, h.fabric.acked, h.host.admitted("session-a", "crew-delivery:d1"))
	}
}

func TestSecondAmbiguousPromptIsOutcomeUnknown(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(int, string) string { return "drop-before" }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if len(h.host.promptCalls()) != 2 || len(h.fabric.unknown) != 1 || len(h.fabric.acked) != 0 {
		t.Fatalf("prompts %v unknown %v acked %v", h.host.promptCalls(), h.fabric.unknown, h.fabric.acked)
	}
}

func TestTimedOutPromptIsNeverResent(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(int, string) string { return "slow" }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	// The slow prompt was admitted, so the check finds it.
	if len(h.host.promptCalls()) != 1 || len(h.fabric.acked) != 1 {
		t.Fatalf("prompts %v acked %v", h.host.promptCalls(), h.fabric.acked)
	}

	h2 := newHarness(t)
	h2.host.add("session-a", SessionSummary{}, true, false)
	h2.pump(t)
	release := make(chan struct{})
	h2.host.onPrompt = func(int, string) string { <-release; return "" }
	h2.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h2.pump(t)
	close(release)
	if len(h2.host.promptCalls()) != 1 || len(h2.fabric.unknown) != 1 {
		t.Fatalf("prompts %v unknown %v", h2.host.promptCalls(), h2.fabric.unknown)
	}
}

func TestRefusedPromptIsNotInsertedAndBacksOff(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(int, string) string { return "session/model-unavailable" }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if len(h.fabric.requeued) != 1 || h.fabric.requeued[0].Reason != "prompt_refused" || len(h.host.promptCalls()) != 1 {
		t.Fatalf("requeued %+v prompts %v", h.fabric.requeued, h.host.promptCalls())
	}
	h.fabric.claimReqs = nil
	h.pump(t)
	if len(h.fabric.claimReqs) != 0 {
		t.Fatal("a refusing session was claimed again at once")
	}
}

func TestDeletedSessionFailsTheDelivery(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.onPrompt = func(int, string) string { return "session/not-found" }
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if len(h.fabric.failed) != 1 || len(h.fabric.requeued) != 0 {
		t.Fatalf("failed %v requeued %v", h.fabric.failed, h.fabric.requeued)
	}
}

func TestInFlightDispatchIsReconciledNotResent(t *testing.T) {
	for _, held := range []bool{true, false} {
		h := newHarness(t)
		s := h.host.add("session-a", SessionSummary{}, true, false)
		h.pump(t)
		if held {
			h.host.mu.Lock()
			s.inbox = append(s.inbox, fakeItem{rpcID: "crew-delivery:d1", mode: "queue"})
			h.host.mu.Unlock()
		}
		head := fabricclient.Delivery{DeliveryID: "d1", State: "dispatching", NativeAttemptRef: "dsh:d1", ClaimOwnerAdapterID: "crew-dsh"}
		h.fabric.queue("s/session-a", fabricclient.Claim{Reason: "head_dispatching", Head: &head})
		h.pump(t)
		if len(h.host.promptCalls()) != 0 || h.waits != 1 {
			t.Fatalf("held=%v: prompts %v waits %d", held, h.host.promptCalls(), h.waits)
		}
		if held && len(h.fabric.acked) != 1 || !held && len(h.fabric.unknown) != 1 {
			t.Fatalf("held=%v: acked %v unknown %v", held, h.fabric.acked, h.fabric.unknown)
		}
	}
}

func TestHostRestartReconnectsWithTheNewToken(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	h.host.restart()
	h.fabric.queue("s/session-a", claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn"))
	h.pump(t)
	if len(h.fabric.acked) != 1 || len(h.host.promptCalls()) != 1 {
		t.Fatalf("acked %v prompts %v", h.fabric.acked, h.host.promptCalls())
	}
}

func TestParseLaunchLine(t *testing.T) {
	log := "dsh web: http://127.0.0.1:3080/?token=old\nnoise\ndsh web: http://127.0.0.1:3080/?token=Ab-_9.x~\n"
	creds, ok := ParseLaunchLine(log)
	if !ok || creds.BaseURL != "http://127.0.0.1:3080" || creds.Token != "Ab-_9.x~" {
		t.Fatalf("got %+v %v", creds, ok)
	}
	if _, ok := ParseLaunchLine("nothing here"); ok {
		t.Fatal("parsed a log without a launch line")
	}
}

func TestDefiniteAndTimeoutClassification(t *testing.T) {
	if !Definite(&RemoteError{Code: "session/agent-busy"}) || !Definite(&notSentError{errors.New("dial")}) {
		t.Fatal("refusals are definite")
	}
	if Definite(errors.New("EOF")) || !Timeout(fmt.Errorf("x: %w", context.DeadlineExceeded)) {
		t.Fatal("classification")
	}
}

func TestHandlerSendAndWho(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	server := httptest.NewServer(Handler(h.d))
	defer server.Close()
	result, err := crewcli.Send(context.Background(), server.URL, crewcli.SendRequest{From: "session-a", To: "s/other", Body: "hi", OperationID: "op1", Policy: "next_step"})
	if err != nil || result.From != "s/session-a" {
		t.Fatalf("send = %+v, %v", result, err)
	}
	if got := h.fabric.submitted[0]; got.ProducerID != "crew-dsh" || got.ActivationPolicy != "next_step" || got.SenderAddress != "s/session-a" {
		t.Fatalf("submitted %+v", got)
	}
	sessions, skipped := crewcli.Who(context.Background(), map[string]string{"dsh": server.URL})
	if len(skipped) != 0 || len(sessions) != 1 || sessions[0].Runtime != "dsh" || sessions[0].Address != "s/session-a" {
		t.Fatalf("who = %+v %v", sessions, skipped)
	}
}

func TestRenderedPromptNamesTheReplyCommand(t *testing.T) {
	h := newHarness(t)
	h.host.add("session-a", SessionSummary{}, true, false)
	h.pump(t)
	var text string
	h.d.native = &capture{Native: h.d.native, text: &text}
	claim := claimFor("s/session-a", "d1", "deliver_at_idle", "next_turn")
	claim.Message.SenderAddress = "s/peer"
	h.fabric.queue("s/session-a", claim)
	h.pump(t)
	if !strings.Contains(text, "crew message m-d1") || !strings.HasSuffix(text, `crew-dsh send --to s/peer "your reply"`) {
		t.Fatalf("text = %q", text)
	}
}

type capture struct {
	Native
	text *string
}

func (c *capture) Prompt(ctx context.Context, p PromptRequest) error {
	*c.text = p.Text
	return c.Native.Prompt(ctx, p)
}
