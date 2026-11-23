package agydriver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/agydriver"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

// Conversation behaviours after a post, as its files show them.
const (
	// takes: the message file is written and the message enters the
	// conversation at once.
	takes = ""
	// holds: the message file is written, but it has not entered the
	// conversation yet (a tool call is still running).
	holds = "holds"
	// drops: the server answers {} but nothing is written.
	drops = "drops"
)

type post struct{ addr, conversation, content string }

// fakeNative stands in for agy language servers, by address. It writes the
// message file and transcript step agy would write.
type fakeNative struct {
	mu        sync.Mutex
	token     string
	posts     []post
	runState  map[string]string // conversation -> run state
	parent    map[string]string
	gone      map[string]bool // address -> nothing listening
	refuse    map[string]bool // conversation -> refuse sends
	behaviour map[string]string
	brain     string
}

func (f *fakeNative) Conversation(_ context.Context, addr, token, conversationID string) (agydriver.ConversationState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[addr] {
		return agydriver.ConversationState{}, agydriver.ErrGone
	}
	if token != f.token {
		return agydriver.ConversationState{}, &agydriver.RefusedError{Code: "unauthenticated", Message: "missing CSRF token"}
	}
	state := f.runState[conversationID]
	if state == "" {
		state = agydriver.RunStateIdle
	}
	return agydriver.ConversationState{RunState: state, Parent: f.parent[conversationID]}, nil
}

func (f *fakeNative) Send(_ context.Context, addr, token, conversationID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[addr] {
		return agydriver.ErrGone
	}
	if token != f.token {
		return &agydriver.RefusedError{Code: "unauthenticated"}
	}
	if f.refuse[conversationID] {
		return &agydriver.RefusedError{Code: "invalid_argument", Message: "no"}
	}
	f.posts = append(f.posts, post{addr, conversationID, content})
	switch f.behaviour[conversationID] {
	case takes:
		writeMessageFile(f.brain, conversationID, len(f.posts), content)
		appendStep(transcriptPath(f.brain, conversationID), "SYSTEM", "SYSTEM_MESSAGE", "<SYSTEM_MESSAGE>\n[Message] sender=system priority=MESSAGE_PRIORITY_HIGH content="+content+"\n</SYSTEM_MESSAGE>")
	case holds:
		writeMessageFile(f.brain, conversationID, len(f.posts), content)
	}
	return nil
}

func (f *fakeNative) sent() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post(nil), f.posts...)
}

func (f *fakeNative) set(fn func(f *fakeNative)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeProcs is a process table: each live process listens on one HTTP port.
type fakeProcs struct {
	mu       sync.Mutex
	alive    map[agydriver.Proc]bool
	ports    map[agydriver.Proc][]int
	resuming map[string]agydriver.Proc
}

func (p *fakeProcs) Alive(proc agydriver.Proc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive[proc]
}

func (p *fakeProcs) ListenPorts(proc agydriver.Proc) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ports[proc], nil
}

func (p *fakeProcs) Resuming() map[string]agydriver.Proc {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]agydriver.Proc{}
	for k, v := range p.resuming {
		out[k] = v
	}
	return out
}

func (p *fakeProcs) start(pid int, port int) agydriver.Proc {
	p.mu.Lock()
	defer p.mu.Unlock()
	proc := agydriver.Proc{PID: pid, Start: int64(pid * 100)}
	p.alive[proc] = true
	p.ports[proc] = []int{port}
	return proc
}

func (p *fakeProcs) exit(proc agydriver.Proc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.alive, proc)
}

func transcriptPath(brain, conversation string) string {
	return filepath.Join(brain, conversation, ".system_generated", "logs", "transcript_full.jsonl")
}

func writeMessageFile(brain, conversation string, n int, content string) {
	dir := filepath.Join(brain, conversation, ".system_generated", "messages")
	_ = os.MkdirAll(dir, 0o755)
	data, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("m-%d", n), "recipient": conversation, "sender": "system", "content": content})
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("m-%d.json", n)), data, 0o644)
}

func appendStep(path, source, kind, content string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{"source": source, "type": kind, "status": "DONE", "content": content})
	_, _ = f.Write(append(line, '\n'))
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type harness struct {
	ctx       context.Context
	svc       *service.Service
	fabric    *fabricclient.Client
	native    *fakeNative
	procs     *fakeProcs
	clock     *clock
	driver    *agydriver.Driver
	producer  fabricclient.Lease
	dir       string
	tokenPath string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "fabric.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := service.New(store, service.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewHandler(svc))
	t.Cleanup(server.Close)
	fabric, err := fabricclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "agy-token")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{ctx: ctx, svc: svc, fabric: fabric, clock: &clock{now: time.Now()}, dir: dir, tokenPath: tokenPath,
		native: &fakeNative{token: "tok", runState: map[string]string{}, parent: map[string]string{}, gone: map[string]bool{}, refuse: map[string]bool{}, behaviour: map[string]string{}, brain: filepath.Join(dir, "brain")},
		procs:  &fakeProcs{alive: map[agydriver.Proc]bool{}, ports: map[agydriver.Proc][]int{}, resuming: map[string]agydriver.Proc{}},
	}
	h.driver = h.newDriver(t)
	if err := h.driver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h.producer, err = fabric.Register(ctx, "producer", "p-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: "producer", LeaseToken: h.producer.LeaseToken, NativeKey: "sender"}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) newDriver(t *testing.T) *agydriver.Driver {
	t.Helper()
	d, err := agydriver.New(h.fabric, h.native, h.procs, h.clock.Now, agydriver.Config{AdapterID: "antigravity", InstanceID: "test", StatePath: filepath.Join(h.dir, "state.json"), TokenPath: h.tokenPath, LeaseDuration: time.Minute, AcceptTimeout: time.Minute, QueuedTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (h *harness) register(t *testing.T, conversation string, proc agydriver.Proc) agydriver.Reachable {
	t.Helper()
	reach, err := h.driver.RegisterSession(h.ctx, agydriver.Registration{ConversationID: conversation, Proc: proc, Cwd: "/work/" + conversation, Transcript: transcriptPath(h.native.brain, conversation)})
	if err != nil {
		t.Fatal(err)
	}
	// Each hook arrives a moment after the last, as in a real session.
	h.clock.Advance(time.Millisecond)
	return reach
}

func (h *harness) send(t *testing.T, op, to, body, policy string) {
	t.Helper()
	sender := service.SessionAddress("producer", "sender")
	if _, err := h.fabric.Submit(h.ctx, fabricclient.MessageRequest{ProducerID: "producer", LeaseToken: h.producer.LeaseToken, OperationID: op, SenderAddress: sender, RecipientAddress: to, Body: body, ActivationPolicy: policy, TTL: "1h"}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) states(t *testing.T, address string) string {
	t.Helper()
	all, err := h.svc.ListDeliveries(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].AcceptedSequence < all[j].AcceptedSequence })
	out := []string{}
	for _, d := range all {
		if d.RecipientAddress != address {
			continue
		}
		state := d.State
		if d.TerminalReason != "" && d.State != "delivered" {
			state += "/" + d.TerminalReason
		}
		out = append(out, state)
	}
	return strings.Join(out, ",")
}

// hook runs a hook against the driver's API the way agy would. For a step
// (PreInvocation) whose answer injects a system message, it records the step
// agy writes to the transcript. It returns the injected text.
func (h *harness) hook(t *testing.T, conversation, event string, proc agydriver.Proc) string {
	t.Helper()
	api := httptest.NewServer(agydriver.Handler(h.driver))
	defer api.Close()
	in := agydriver.HookInput{ConversationID: conversation, Cwd: "/work/" + conversation, TranscriptPath: transcriptPath(h.native.brain, conversation)}
	out := agydriver.RunHook(h.ctx, api.URL, event, in, proc, agydriver.HookOptions{})
	h.clock.Advance(time.Millisecond)
	if len(out) == 0 {
		return ""
	}
	var answer struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out, &answer); err != nil || len(answer.InjectSteps) != 1 {
		t.Fatalf("hook %s answer=%s", event, out)
	}
	text := answer.InjectSteps[0].SystemMessage.SystemMessage
	if event == "step" {
		appendStep(in.TranscriptPath, "SYSTEM_SDK", "SYSTEM_MESSAGE", "The following system message was injected by a PreInvocation hook:\n"+text)
	}
	return text
}

func TestCapabilitiesLeaveBusyNextTurnInTheFabric(t *testing.T) {
	for _, capability := range agydriver.Capabilities {
		if capability == "durable_next_turn" || capability == "wake_inactive" {
			t.Fatalf("crew-agy claims %s", capability)
		}
	}
}

func TestDeliveryFollowsPolicyAndRunState(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	reach := h.register(t, "conv-1", proc)
	if !strings.HasPrefix(reach.Address, "s/") || !reach.Wakeable {
		t.Fatalf("reach=%#v", reach)
	}
	address := reach.Address
	if again := h.register(t, "conv-1", proc); again.Address != address {
		t.Fatalf("re-register=%q, want %q", again.Address, address)
	}

	// Busy: next_step is posted (the running turn reads it at its next step);
	// next_turn waits in the fabric.
	h.driver.SetBusy("conv-1", true)
	h.native.set(func(f *fakeNative) { f.runState["conv-1"] = agydriver.RunStateRunning })
	h.send(t, "step", address, "gate failed", "next_step")
	h.send(t, "turn", address, "later please", "next_turn")
	h.driver.Pump(h.ctx)
	sent := h.native.sent()
	if len(sent) != 1 || sent[0].addr != "127.0.0.1:4100" || sent[0].conversation != "conv-1" || !strings.Contains(sent[0].content, "gate failed") {
		t.Fatalf("busy posts=%#v", sent)
	}
	if got := h.states(t, address); got != "delivered,queued" {
		t.Fatalf("states while busy=%v", got)
	}

	// Idle: the next_turn message is posted and starts a turn.
	h.native.set(func(f *fakeNative) { f.runState["conv-1"] = agydriver.RunStateIdle })
	h.driver.SetBusy("conv-1", false)
	h.driver.Pump(h.ctx)
	sent = h.native.sent()
	if len(sent) != 2 || !strings.Contains(sent[1].content, "later please") {
		t.Fatalf("idle posts=%#v", sent)
	}

	// no_wake is never posted, idle or busy: it reaches a turn through the
	// next PreInvocation hook.
	h.send(t, "quiet", address, "fyi", "no_wake")
	h.driver.Pump(h.ctx)
	h.driver.SetBusy("conv-1", true)
	h.driver.Pump(h.ctx)
	if n := len(h.native.sent()); n != 2 {
		t.Fatalf("no_wake was posted (%d posts)", n)
	}
	if text := h.hook(t, "conv-1", "step", proc); !strings.Contains(text, "fyi") || !strings.Contains(text, "crew-agy send --to") {
		t.Fatalf("step hook injected %q", text)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "delivered,delivered,delivered" {
		t.Fatalf("final states=%v", got)
	}
	// Nothing left: the next step injects nothing.
	if text := h.hook(t, "conv-1", "step", proc); text != "" {
		t.Fatalf("empty step hook injected %q", text)
	}
}

func TestNextTurnWaitsWhileTheServerSaysRunning(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	// The hooks last said idle, but a turn has just started (its
	// PreInvocation hook is still on its way).
	h.native.set(func(f *fakeNative) { f.runState["conv-1"] = agydriver.RunStateRunning })
	h.send(t, "turn", address, "after this turn", "next_turn")
	h.driver.Pump(h.ctx)
	if n := len(h.native.sent()); n != 0 {
		t.Fatalf("next_turn was posted into a running turn (%d posts)", n)
	}
	if got := h.states(t, address); got != "queued" {
		t.Fatalf("states=%v", got)
	}
	h.native.set(func(f *fakeNative) { f.runState["conv-1"] = agydriver.RunStateIdle })
	h.hook(t, "conv-1", "idle", proc)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "delivered" {
		t.Fatalf("states after Stop=%v", got)
	}
}

func TestExitedProcessKeepsMailUntilResumed(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	h.procs.exit(proc)
	h.send(t, "turn", address, "while you were away", "next_turn")
	h.driver.Pump(h.ctx)
	if n := len(h.native.sent()); n != 0 {
		t.Fatalf("posted to an exited process (%d posts)", n)
	}
	if got := h.driver.Sessions(); len(got) != 1 || got[0].State != "exited" || got[0].Runtime != "antigravity" {
		t.Fatalf("sessions=%#v", got)
	}
	// agy --conversation conv-1 resumes it in a new process, which runs no
	// SessionStart hook; the driver finds it from the command line.
	resumed := h.procs.start(200, 4200)
	h.procs.mu.Lock()
	h.procs.resuming["conv-1"] = resumed
	h.procs.mu.Unlock()
	for i := 0; i < 5; i++ {
		h.driver.Pump(h.ctx)
	}
	sent := h.native.sent()
	if len(sent) != 1 || sent[0].addr != "127.0.0.1:4200" {
		t.Fatalf("posts after resume=%#v", sent)
	}
	if got := h.states(t, address); got != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestHookFromResumedConversationReattachesIt(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	h.procs.exit(proc)
	h.send(t, "step", address, "note", "next_step")
	h.driver.Pump(h.ctx)
	// Resumed with --continue (no id on the command line): its first turn's
	// PreInvocation hook re-registers it under the new process, and the
	// waiting message goes into that turn.
	resumed := h.procs.start(200, 4200)
	h.native.set(func(f *fakeNative) { f.runState["conv-1"] = agydriver.RunStateRunning })
	if text := h.hook(t, "conv-1", "step", resumed); !strings.Contains(text, "note") {
		t.Fatalf("step hook injected %q", text)
	}
	if again := h.register(t, "conv-1", resumed); again.Address != address || !again.Wakeable {
		t.Fatalf("reach after resume=%#v", again)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestNewConversationInTheSameProcessSupersedesTheOld(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	first := h.register(t, "conv-1", proc).Address
	// /new in the same terminal: the old conversation is no longer on screen,
	// and a post would run it unseen.
	h.register(t, "conv-2", proc)
	h.send(t, "turn", first, "for the old one", "next_turn")
	h.driver.Pump(h.ctx)
	if n := len(h.native.sent()); n != 0 {
		t.Fatalf("posted to a superseded conversation (%d posts)", n)
	}
	// Switching back to it (its next hook) makes it current again.
	h.hook(t, "conv-1", "idle", proc)
	h.driver.Pump(h.ctx)
	if got := h.states(t, first); got != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestSettlesFromTheConversationsFiles(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address

	// Answered {} but nothing written: outcome_unknown after AcceptTimeout,
	// never posted twice.
	h.native.set(func(f *fakeNative) { f.behaviour["conv-1"] = drops })
	h.send(t, "lost", address, "lost one", "next_step")
	h.driver.Pump(h.ctx)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "dispatching" {
		t.Fatalf("states before timeout=%v", got)
	}
	h.clock.Advance(2 * time.Minute)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "outcome_unknown/native_outcome_unknown" {
		t.Fatalf("states after timeout=%v", got)
	}
	if n := len(h.native.sent()); n != 1 {
		t.Fatalf("posted %d times", n)
	}

	// Taken in (message file written) while a tool runs, then the process
	// exits before the conversation reads it.
	h.native.set(func(f *fakeNative) { f.behaviour["conv-1"] = holds })
	h.send(t, "held", address, "held one", "next_step")
	h.driver.Pump(h.ctx)
	h.clock.Advance(3 * time.Minute)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "outcome_unknown/native_outcome_unknown,dispatching" {
		t.Fatalf("held past AcceptTimeout=%v", got)
	}
	h.procs.exit(proc)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "outcome_unknown/native_outcome_unknown,outcome_unknown/native_outcome_unknown" {
		t.Fatalf("after exit=%v", got)
	}
}

func TestRestartedDriverSettlesItsDispatch(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	h.native.set(func(f *fakeNative) { f.behaviour["conv-1"] = holds })
	h.send(t, "one", address, "survives a restart", "next_step")
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "dispatching" {
		t.Fatalf("states=%v", got)
	}
	// The conversation reads the message while the driver is down.
	sent := h.native.sent()
	appendStep(transcriptPath(h.native.brain, "conv-1"), "SYSTEM", "SYSTEM_MESSAGE", "[Message] content="+sent[0].content)
	h.driver = h.newDriver(t)
	if err := h.driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "delivered" {
		t.Fatalf("states after restart=%v", got)
	}
	if n := len(h.native.sent()); n != 1 {
		t.Fatalf("posted %d times", n)
	}
}

func TestRefusedPostGoesBackToTheQueue(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	h.native.set(func(f *fakeNative) { f.refuse["conv-1"] = true })
	h.send(t, "one", address, "try again", "next_turn")
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "queued" {
		t.Fatalf("states after refusal=%v", got)
	}
	h.native.set(func(f *fakeNative) { f.refuse["conv-1"] = false })
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestWithoutTheTokenMailReachesOnlyRunningTurns(t *testing.T) {
	h := newHarness(t)
	// agy was started without ANTIGRAVITY_SERVER_CSRF_TOKEN.
	h.native.set(func(f *fakeNative) { f.token = "agy-made-its-own" })
	proc := h.procs.start(100, 4100)
	reach := h.register(t, "conv-1", proc)
	if reach.Wakeable {
		t.Fatal("wakeable without the token")
	}
	if text := agydriver.SessionContext(reach.Address, reach.Wakeable, nil, ""); !strings.Contains(text, "never by starting a turn") {
		t.Fatalf("session context=%q", text)
	}
	h.send(t, "step", reach.Address, "step note", "next_step")
	h.driver.Pump(h.ctx)
	if n := len(h.native.sent()); n != 0 {
		t.Fatalf("posted without the token (%d posts)", n)
	}
	// A running turn takes it through PreInvocation.
	h.driver.SetBusy("conv-1", true)
	if text := h.hook(t, "conv-1", "step", proc); !strings.Contains(text, "step note") {
		t.Fatalf("step hook injected %q", text)
	}
	// next_turn waits for an idle conversation the driver cannot post to, so
	// it stays queued until agy runs with the token.
	h.hook(t, "conv-1", "idle", proc)
	h.send(t, "turn", reach.Address, "wake up", "next_turn")
	h.driver.Pump(h.ctx)
	if got := h.states(t, reach.Address); got != "delivered,queued" {
		t.Fatalf("states=%v", got)
	}
	h.native.set(func(f *fakeNative) { f.token = "tok" })
	h.hook(t, "conv-1", "idle", proc)
	h.driver.Pump(h.ctx)
	if got := h.states(t, reach.Address); got != "delivered,delivered" {
		t.Fatalf("states once agy has the token=%v", got)
	}
}

func TestSubagentConversationsDoNotRegister(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	main := h.register(t, "conv-1", proc).Address
	h.native.set(func(f *fakeNative) { f.parent["child-1"] = "conv-1" })
	if _, err := h.driver.RegisterSession(h.ctx, agydriver.Registration{ConversationID: "child-1", Proc: proc}); err != agydriver.ErrSubagent {
		t.Fatalf("subagent registration err=%v", err)
	}
	if text := h.hook(t, "child-1", "session-start", proc); text != "" {
		t.Fatalf("subagent session-start injected %q", text)
	}
	// The main conversation stays current in its process.
	h.send(t, "turn", main, "for main", "next_turn")
	h.driver.Pump(h.ctx)
	if got := h.states(t, main); got != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestSessionStartInjectsTheAddress(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	text := h.hook(t, "conv-1", "session-start", proc)
	address, _ := h.driver.Address("conv-1")
	if !strings.Contains(text, "Your crew messaging address is "+address) || !strings.Contains(text, "notify_address") || !strings.Contains(text, "`crew-agy board`") {
		t.Fatalf("session-start context=%q", text)
	}
	if strings.Contains(text, "never by starting a turn") {
		t.Fatalf("wakeable session told it cannot be woken: %q", text)
	}
}

func TestFillNotifyAddsTheAddressToDenSubmissions(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	address := h.register(t, "conv-1", proc).Address
	api := httptest.NewServer(agydriver.Handler(h.driver))
	defer api.Close()
	var in agydriver.HookInput
	if err := json.Unmarshal([]byte(`{"conversationId":"conv-1","hookEventName":"PreToolUse","toolCall":{"id":"call_1","name":"call_mcp_tool","args":{"ServerName":"den","ToolName":"submit_task_for_review","Arguments":{"task_id":42,"project_id":"p"}}}}`), &in); err != nil {
		t.Fatal(err)
	}
	out := agydriver.RunHook(h.ctx, api.URL, "fill-notify", in, proc, agydriver.HookOptions{})
	var answer struct {
		Decision  string `json:"decision"`
		Overwrite struct {
			Arguments map[string]any `json:"Arguments"`
		} `json:"overwrite"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("answer=%s", out)
	}
	if answer.Decision != "ask" || answer.Overwrite.Arguments["notify_address"] != address || answer.Overwrite.Arguments["task_id"] != float64(42) {
		t.Fatalf("answer=%s", out)
	}
}

func TestModelComesFromTheTranscript(t *testing.T) {
	h := newHarness(t)
	proc := h.procs.start(100, 4100)
	h.register(t, "conv-1", proc)
	path := transcriptPath(h.native.brain, "conv-1")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","model":"gemini-4-argon-high","content":"ok"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(agydriver.Handler(h.driver))
	defer api.Close()
	resp, err := api.Client().Get(api.URL + "/v1/sessions/conv-1/model")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var about struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&about); err != nil || about.Model != "gemini-4-argon-high" {
		t.Fatalf("about=%#v err=%v", about, err)
	}
}
