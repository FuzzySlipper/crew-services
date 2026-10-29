package claudedriver_test

import (
	"context"
	"encoding/json"
	"errors"
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

	"crew-services/internal/claudedriver"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

type posted struct{ socket, msgID, text, priority string }

// Session behaviours after a frame is written, as the transcript shows them.
const (
	// takes: the message is queued and enters the conversation at once.
	takes = ""
	// queues: queued in memory but not yet in the conversation.
	queues = "queues"
	// ignores: the write succeeds but the session holds, refuses or drops it.
	ignores = "ignores"
)

// fakeInbox records posts and writes what Claude Code would write to the
// session transcript. A socket listed in gone refuses connections, and one in
// failPost accepts the connection but fails the write.
type fakeInbox struct {
	mu          sync.Mutex
	posts       []posted
	gone        map[string]bool
	failPost    map[string]bool
	behaviour   map[string]string
	transcripts map[string]string
	// onDial runs after a connection opens, standing in for a hook that
	// arrives between the claim and the write.
	onDial func(socket string)
}

func (f *fakeInbox) Dial(_ context.Context, socket, _ string) (claudedriver.Conn, error) {
	f.mu.Lock()
	gone, onDial := f.gone[socket], f.onDial
	f.mu.Unlock()
	if gone {
		return nil, claudedriver.ErrInboxGone
	}
	if onDial != nil {
		onDial(socket)
	}
	return &fakeConn{inbox: f, socket: socket}, nil
}

func (f *fakeInbox) sent() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.posts...)
}

type fakeConn struct {
	inbox  *fakeInbox
	socket string
}

func (c *fakeConn) Post(msgID, text, priority string) error {
	c.inbox.mu.Lock()
	defer c.inbox.mu.Unlock()
	if c.inbox.failPost[c.socket] {
		return errors.New("broken pipe")
	}
	c.inbox.posts = append(c.inbox.posts, posted{c.socket, msgID, text, priority})
	path := c.inbox.transcripts[c.socket]
	switch c.inbox.behaviour[c.socket] {
	case takes:
		appendTranscript(path, enqueueLine(text), insertedLine(msgID, text))
	case queues:
		appendTranscript(path, enqueueLine(text))
	}
	return nil
}
func (c *fakeConn) Close() error { return nil }

func enqueueLine(text string) string {
	line, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": text})
	return string(line)
}

func insertedLine(msgID, text string) string {
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}, "origin": map[string]string{"kind": "peer", "from": "unknown", "msg_id": msgID}})
	return string(line)
}

func appendTranscript(path string, lines ...string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	for _, line := range lines {
		_, _ = f.WriteString(line + "\n")
	}
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
	ctx      context.Context
	svc      *service.Service
	fabric   *fabricclient.Client
	inbox    *fakeInbox
	clock    *clock
	driver   *claudedriver.Driver
	producer fabricclient.Lease
	dir      string
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
	h := &harness{ctx: ctx, svc: svc, fabric: fabric, clock: &clock{now: time.Now()}, inbox: &fakeInbox{gone: map[string]bool{}, failPost: map[string]bool{}, behaviour: map[string]string{}, transcripts: map[string]string{}}, dir: dir}
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

func (h *harness) newDriver(t *testing.T) *claudedriver.Driver {
	t.Helper()
	return h.newDriverWith(t, h.fabric)
}

func (h *harness) newDriverWith(t *testing.T, fabric claudedriver.Fabric) *claudedriver.Driver {
	t.Helper()
	d, err := claudedriver.New(fabric, h.inbox, h.clock.Now, claudedriver.Config{AdapterID: "claude-code", InstanceID: "test", StatePath: filepath.Join(h.dir, "state.json"), LeaseDuration: time.Minute, AcceptTimeout: time.Minute, QueuedTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// socket makes a file standing in for a live inbox socket; the driver treats a
// missing path as a session that is not running.
func (h *harness) socket(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(h.dir, name+".sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// register registers a session whose transcript is <session>.jsonl, written
// by the fake inbox for posts to sock.
func (h *harness) register(t *testing.T, d *claudedriver.Driver, sessionID, sock string) string {
	t.Helper()
	transcript := h.transcript(sessionID)
	h.inbox.mu.Lock()
	h.inbox.transcripts[sock] = transcript
	h.inbox.mu.Unlock()
	address, err := d.RegisterSession(h.ctx, claudedriver.Registration{SessionID: sessionID, Socket: sock, Token: "tok", Cwd: "/work/" + sessionID, Transcript: transcript})
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func (h *harness) transcript(sessionID string) string {
	return filepath.Join(h.dir, sessionID+".jsonl")
}

func (h *harness) send(t *testing.T, op, to, body, policy string) {
	t.Helper()
	sender := service.SessionAddress("producer", "sender")
	if _, err := h.fabric.Submit(h.ctx, fabricclient.MessageRequest{ProducerID: "producer", LeaseToken: h.producer.LeaseToken, OperationID: op, SenderAddress: sender, RecipientAddress: to, Body: body, ActivationPolicy: policy, TTL: "1h"}); err != nil {
		t.Fatal(err)
	}
}

// states lists each delivery to address as "state" or "state/reason", in
// acceptance order.
func (h *harness) states(t *testing.T, address string) []string {
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
	return out
}

func TestCapabilitiesLeaveBusyNextTurnInTheFabric(t *testing.T) {
	for _, capability := range claudedriver.Capabilities {
		if capability == "durable_next_turn" {
			t.Fatal("crew-claude claims durable_next_turn, but Claude Code's follow-up queue is in memory")
		}
	}
}

func TestDeliveryFollowsPolicyAndTurnState(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "one")
	address := h.register(t, h.driver, "session-1", sock)
	if !strings.HasPrefix(address, "s/") {
		t.Fatalf("address=%q", address)
	}
	if again := h.register(t, h.driver, "session-1", sock); again != address {
		t.Fatalf("re-register=%q, want %q", again, address)
	}

	// Busy: next_step reaches the active turn; next_turn waits in the fabric.
	h.driver.SetBusy("session-1", true)
	h.send(t, "step", address, "gate failed", "next_step")
	h.send(t, "turn", address, "later please", "next_turn")
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].priority != claudedriver.PriorityNext {
		t.Fatalf("busy posts=%#v", sent)
	}
	if !strings.Contains(sent[0].text, "gate failed") || !strings.Contains(sent[0].text, "Crew message from s/") {
		t.Fatalf("rendered text=%q", sent[0].text)
	}
	if got := h.states(t, address); len(got) != 2 || got[0] != "delivered" || got[1] != "queued" {
		t.Fatalf("states while busy=%v", got)
	}

	// Idle: the next_turn message goes out as a new turn.
	h.driver.SetBusy("session-1", false)
	h.driver.Pump(h.ctx)
	sent = h.inbox.sent()
	if len(sent) != 2 || sent[1].priority != claudedriver.PriorityLater || !strings.Contains(sent[1].text, "later please") {
		t.Fatalf("idle posts=%#v", sent)
	}
	if len(sent[0].msgID) != 36 || sent[0].msgID == sent[1].msgID {
		t.Fatalf("native ids=%q %q", sent[0].msgID, sent[1].msgID)
	}

	// no_wake never goes to the socket, idle or busy: it reaches the active
	// turn through the next in-turn hook.
	h.send(t, "quiet", address, "fyi", "no_wake")
	h.driver.Pump(h.ctx)
	h.driver.SetBusy("session-1", true)
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 2 {
		t.Fatalf("no_wake posted to the inbox (%d posts)", n)
	}
	if event, text := h.hook(t, h.driver, "session-1", "step", true); event != "PostToolUse" || !strings.Contains(text, "fyi") {
		t.Fatalf("step hook=%q %q", event, text)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); strings.Join(got, ",") != "delivered,delivered,delivered" {
		t.Fatalf("final states=%v", got)
	}
}

// hook runs an in-turn hook against the driver's API the way Claude Code
// would and, when it returns additionalContext and lands is set, writes the
// hook_additional_context attachment Claude Code records for it. It returns
// the output's hookEventName and context.
func (h *harness) hook(t *testing.T, d *claudedriver.Driver, sessionID, event string, lands bool) (string, string) {
	t.Helper()
	api := httptest.NewServer(claudedriver.Handler(d))
	defer api.Close()
	out := claudedriver.RunHook(h.ctx, api.URL, event, claudedriver.HookInput{SessionID: sessionID}, claudedriver.HookEnv{})
	if len(out) == 0 {
		return "", ""
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("hook output %s: %v", out, err)
	}
	o := parsed.HookSpecificOutput
	if lands && o.AdditionalContext != "" {
		appendTranscript(h.transcript(sessionID), hookContextLine(o.HookEventName, o.AdditionalContext))
	}
	return o.HookEventName, o.AdditionalContext
}

// hookContextLine is what Claude Code 2.1.312 writes for a hook's
// additionalContext (checked against a real PostToolUse hook).
func hookContextLine(event, text string) string {
	line, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "hook_additional_context", "content": []string{text}, "hookName": event + ":Bash", "toolUseID": "toolu_1", "hookEvent": event}})
	return string(line)
}

// Busy next_turn work is never handed to the session's in-memory queue: it
// stays queued in the fabric through an exit and a resume, and goes out once
// the resumed session reports idle.
func TestBusyNextTurnWaitsForIdleAcrossExit(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "five")
	address := h.register(t, h.driver, "session-5", sock)
	h.driver.SetBusy("session-5", true)
	h.send(t, "turn", address, "after your turn", "next_turn")
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("next_turn posted to a busy session (%d posts)", n)
	}
	// The session exits mid-turn: no Stop hook, the socket disappears.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); len(got) != 1 || got[0] != "queued" {
		t.Fatalf("after exit=%v", got)
	}

	// It resumes under a new process; a restarted driver takes over. The
	// resumed session starts a turn before the driver gets a pass in.
	resumed := h.socket(t, "five-resumed")
	driver := h.newDriver(t)
	if err := driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.register(t, driver, "session-5", resumed); got != address {
		t.Fatalf("resume address=%q, want %q", got, address)
	}
	driver.SetBusy("session-5", true)
	driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("next_turn posted to the resumed busy session (%d posts)", n)
	}
	driver.SetBusy("session-5", false)
	driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].socket != resumed || sent[0].priority != claudedriver.PriorityLater {
		t.Fatalf("after idle=%#v", sent)
	}
	if got := h.states(t, address); len(got) != 1 || got[0] != "delivered" {
		t.Fatalf("final=%v", got)
	}
}

// Every inbox frame can start a turn ("next" landing during a final,
// tool-free response starts one after Stop), so no_wake never goes to the
// socket, whatever the turn state or timing. It goes out only as
// additionalContext of a hook that runs inside an active turn, one message
// per hook call, in the address's order.
func TestNoWakeGoesOnlyThroughInTurnHooks(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "six")
	address := h.register(t, h.driver, "session-6", sock)
	h.driver.SetBusy("session-6", true)
	h.send(t, "quiet-1", address, "fyi one", "no_wake")
	h.send(t, "quiet-2", address, "fyi two", "no_wake")
	h.send(t, "step", address, "gate failed", "next_step")
	for i := 0; i < 4; i++ {
		h.driver.Pump(h.ctx)
		h.clock.Advance(31 * time.Second)
	}
	h.driver.SetBusy("session-6", false)
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("no_wake posted to the inbox (%d posts)", n)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "queued,queued,queued" {
		t.Fatalf("before any hook=%v", got)
	}

	// A turn starts: UserPromptSubmit carries the first message.
	event, text := h.hook(t, h.driver, "session-6", "busy", true)
	if event != "UserPromptSubmit" || !strings.Contains(text, "fyi one") || strings.Contains(text, "fyi two") {
		t.Fatalf("busy hook=%q %q", event, text)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "dispatching,queued,queued" {
		t.Fatalf("after the busy hook=%v", got)
	}
	// Each tool call settles the previous one from the transcript and carries
	// the next; the next_step message behind them rides the same way.
	if _, text := h.hook(t, h.driver, "session-6", "step", true); !strings.Contains(text, "fyi two") {
		t.Fatalf("second step=%q", text)
	}
	if _, text := h.hook(t, h.driver, "session-6", "step", true); !strings.Contains(text, "gate failed") {
		t.Fatalf("third step=%q", text)
	}
	if event, text := h.hook(t, h.driver, "session-6", "step", true); event != "" || text != "" {
		t.Fatalf("nothing pending, yet the hook returned %q %q", event, text)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "delivered,delivered,delivered" {
		t.Fatalf("final=%v", got)
	}
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("inbox posts=%d, want 0", n)
	}
}

// A hook whose context never reaches the transcript (the hook timed out, the
// process died) leaves the delivery outcome_unknown, never handed out again.
func TestLostHookContextBecomesUnknown(t *testing.T) {
	h := newHarness(t)
	address := h.register(t, h.driver, "session-11", h.socket(t, "eleven"))
	h.send(t, "quiet", address, "fyi", "no_wake")
	h.send(t, "after", address, "next one", "no_wake")
	_, first := h.hook(t, h.driver, "session-11", "step", false)
	if !strings.Contains(first, "fyi") {
		t.Fatalf("step=%q", first)
	}
	if _, text := h.hook(t, h.driver, "session-11", "step", true); text != "" {
		t.Fatalf("handed out behind an unsettled head: %q", text)
	}
	// A quoted copy of the text in a tool result does not count.
	quote, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "tool_result", "content": first}}}})
	appendTranscript(h.transcript("session-11"), string(quote))
	h.clock.Advance(61 * time.Second)
	if _, text := h.hook(t, h.driver, "session-11", "step", true); !strings.Contains(text, "next one") {
		t.Fatalf("after the bound=%q", text)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "outcome_unknown/native_outcome_unknown,dispatching" {
		t.Fatalf("states=%v", got)
	}
}

// In-turn hooks return nothing, quickly, when no message is pending for an
// active turn: no work, an unknown session, a subagent's tool call, or a
// next_turn head (which waits for idle and holds back what follows it).
func TestTurnContextIsEmptyWhenNothingIsPending(t *testing.T) {
	h := newHarness(t)
	address := h.register(t, h.driver, "session-12", h.socket(t, "twelve"))
	if event, text := h.hook(t, h.driver, "session-12", "step", true); event != "" || text != "" {
		t.Fatalf("no work: %q %q", event, text)
	}
	if event, text := h.hook(t, h.driver, "nobody", "step", true); event != "" || text != "" {
		t.Fatalf("unknown session: %q %q", event, text)
	}
	h.send(t, "quiet", address, "fyi", "no_wake")
	api := httptest.NewServer(claudedriver.Handler(h.driver))
	defer api.Close()
	if out := claudedriver.RunHook(h.ctx, api.URL, "step", claudedriver.HookInput{SessionID: "session-12", AgentID: "agent-1"}, claudedriver.HookEnv{}); len(out) != 0 {
		t.Fatalf("subagent tool call got the session's message: %s", out)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "queued" {
		t.Fatalf("states=%v", got)
	}
	if out := claudedriver.RunHook(h.ctx, "http://127.0.0.1:1", "step", claudedriver.HookInput{SessionID: "session-12"}, claudedriver.HookEnv{}); len(out) != 0 {
		t.Fatalf("unreachable driver produced output: %s", out)
	}
}

// The fabric delivers an address in order. A no_wake behind a next_turn head
// waits while that head waits for idle; a no_wake head on an idle session
// holds back a next_turn behind it until a turn starts.
func TestNoWakeKeepsAddressOrder(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "thirteen")
	address := h.register(t, h.driver, "session-13", sock)
	h.driver.SetBusy("session-13", true)
	h.send(t, "turn", address, "after your turn", "next_turn")
	h.send(t, "quiet", address, "fyi", "no_wake")
	if _, text := h.hook(t, h.driver, "session-13", "step", true); text != "" {
		t.Fatalf("no_wake passed a waiting next_turn head: %q", text)
	}
	h.driver.SetBusy("session-13", false)
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].priority != claudedriver.PriorityLater || !strings.Contains(sent[0].text, "after your turn") {
		t.Fatalf("idle posts=%#v", sent)
	}
	// That post starts a turn; its UserPromptSubmit carries the no_wake.
	if _, text := h.hook(t, h.driver, "session-13", "busy", true); !strings.Contains(text, "fyi") {
		t.Fatalf("busy hook=%q", text)
	}

	// Idle again, with a no_wake at the head and a next_turn behind it.
	h.hook(t, h.driver, "session-13", "step", true)
	h.driver.SetBusy("session-13", false)
	h.send(t, "quiet-2", address, "fyi again", "no_wake")
	h.send(t, "turn-2", address, "wake me", "next_turn")
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 1 {
		t.Fatalf("posted past a no_wake head (%d posts)", n)
	}
	if _, text := h.hook(t, h.driver, "session-13", "busy", true); !strings.Contains(text, "fyi again") {
		t.Fatalf("busy hook=%q", text)
	}
	h.hook(t, h.driver, "session-13", "step", true)
	h.driver.SetBusy("session-13", false)
	h.driver.Pump(h.ctx)
	if sent := h.inbox.sent(); len(sent) != 2 || !strings.Contains(sent[1].text, "wake me") {
		t.Fatalf("after the turn=%#v", sent)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "delivered,delivered,delivered,delivered" {
		t.Fatalf("final=%v", got)
	}
}

func TestExitedSessionKeepsWorkUntilItResumes(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "two")
	address := h.register(t, h.driver, "session-2", sock)
	// Socket file gone: the session is not running, so nothing is claimed.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	h.send(t, "while-away", address, "gate failed while you were away", "next_step")
	h.driver.Pump(h.ctx)
	// Socket present but refusing: the claim goes back to the queue.
	h.socket(t, "two")
	h.inbox.gone[sock] = true
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("delivered to an exited session (%d posts)", n)
	}

	// The session resumes under a new process and socket; a restarted driver
	// picks up its state file and delivers the queued message.
	resumed := h.socket(t, "two-resumed")
	driver := h.newDriver(t)
	if err := driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.register(t, driver, "session-2", resumed); got != address {
		t.Fatalf("resume register=%q, want %q", got, address)
	}
	driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].socket != resumed || !strings.Contains(sent[0].text, "while you were away") {
		t.Fatalf("after resume=%#v", sent)
	}
	if got := h.states(t, address); len(got) != 1 || got[0] != "delivered" {
		t.Fatalf("final=%v", got)
	}
}

// A successful socket write is not acceptance. The session may hold, refuse
// or drop the frame; only the transcript shows it took the message.
func TestAcceptanceComesFromTheTranscript(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "seven")
	address := h.register(t, h.driver, "session-7", sock)
	h.driver.SetBusy("session-7", true)

	// Rejected after a successful write: nothing in the transcript.
	h.inbox.behaviour[sock] = ignores
	h.send(t, "refused", address, "refused one", "next_step")
	h.send(t, "behind", address, "behind it", "next_step")
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); strings.Join(got, ",") != "dispatching,queued" {
		t.Fatalf("after write=%v", got)
	}
	h.clock.Advance(59 * time.Second)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got[0] != "dispatching" {
		t.Fatalf("settled before the bound=%v", got)
	}
	// Past the bound it becomes outcome_unknown without a second write, and
	// the next message goes out.
	h.inbox.behaviour[sock] = takes
	h.clock.Advance(2 * time.Second)
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 2 || !strings.Contains(sent[1].text, "behind it") {
		t.Fatalf("posts=%#v", sent)
	}
	if got := h.states(t, address); strings.Join(got, ",") != "outcome_unknown/native_outcome_unknown,delivered" {
		t.Fatalf("after bound=%v", got)
	}

	// Queued but lost when the process exits before it enters the
	// conversation.
	h.inbox.behaviour[sock] = queues
	h.send(t, "lost", address, "lost on exit", "next_step")
	h.driver.Pump(h.ctx)
	h.clock.Advance(10 * time.Minute)
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got[2] != "dispatching" {
		t.Fatalf("a queued message timed out like an unqueued one: %v", got)
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	h.driver.Pump(h.ctx)
	if got := h.states(t, address); got[2] != "outcome_unknown/native_outcome_unknown" {
		t.Fatalf("after exit=%v", got)
	}
	if n := len(h.inbox.sent()); n != 3 {
		t.Fatalf("posts=%d, want 3", n)
	}
}

// A message that enters the conversation late (behind a long tool call) is
// acknowledged then, including by a restarted driver; the msg_id quoted in a
// tool result does not count.
func TestLateTranscriptEntryIsAcknowledgedAfterRestart(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "eight")
	address := h.register(t, h.driver, "session-8", sock)
	h.driver.SetBusy("session-8", true)
	h.inbox.behaviour[sock] = queues
	h.send(t, "slow", address, "behind a long tool call", "next_step")
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 {
		t.Fatalf("posts=%#v", sent)
	}
	msgID := sent[0].msgID
	quote, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "tool_result", "content": "msg_id " + msgID}}}})
	appendTranscript(h.transcript("session-8"), string(quote))

	driver := h.newDriver(t)
	if err := driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	driver.Pump(h.ctx)
	if got := h.states(t, address); got[0] != "dispatching" {
		t.Fatalf("a quoted msg_id settled the delivery: %v", got)
	}
	appendTranscript(h.transcript("session-8"), insertedLine(msgID, sent[0].text))
	driver.Pump(h.ctx)
	if got := h.states(t, address); got[0] != "delivered" {
		t.Fatalf("after the transcript entry=%v", got)
	}
	if n := len(h.inbox.sent()); n != 1 {
		t.Fatalf("reposted (%d posts)", n)
	}
}

// A session recorded without a transcript path (by an older build, or a hook
// that did not pass one) is confirmed through Claude Code's default
// transcript location.
func TestSessionWithoutTranscriptPathUsesTheDefaultLocation(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "nine")
	projects := filepath.Join(h.dir, "projects")
	transcript := filepath.Join(projects, "-work-crew-services", "session-9.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	h.inbox.transcripts[sock] = transcript
	address, err := h.driver.RegisterSession(h.ctx, claudedriver.Registration{SessionID: "session-9", Socket: sock, Cwd: "/work/crew-services"})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := claudedriver.New(h.fabric, h.inbox, h.clock.Now, claudedriver.Config{AdapterID: "claude-code", InstanceID: "test", StatePath: filepath.Join(h.dir, "state.json"), LeaseDuration: time.Minute, ProjectsDir: projects}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.send(t, "plain", address, "hello", "next_turn")
	driver.Pump(h.ctx)
	if got := h.states(t, address); len(got) != 1 || got[0] != "delivered" {
		t.Fatalf("states=%v", got)
	}
}

func TestFailedWriteIsNotRedelivered(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "three")
	address := h.register(t, h.driver, "session-3", sock)
	h.driver.SetBusy("session-3", true)
	h.inbox.failPost[sock] = true
	h.send(t, "lost", address, "maybe seen", "next_step")
	h.driver.Pump(h.ctx)
	h.inbox.failPost[sock] = false
	h.driver.Pump(h.ctx)
	h.clock.Advance(2 * time.Minute)
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("an ambiguous post was retried blindly (%d posts)", n)
	}
	if got := h.states(t, address); len(got) != 1 || got[0] != "outcome_unknown/native_outcome_unknown" {
		t.Fatalf("states=%v", got)
	}
}

func TestHooksRegisterAndFillNotifyAddress(t *testing.T) {
	h := newHarness(t)
	api := httptest.NewServer(claudedriver.Handler(h.driver))
	defer api.Close()
	sock := h.socket(t, "four")
	out := claudedriver.RunHook(h.ctx, api.URL, "session-start", claudedriver.HookInput{SessionID: "session-4", Cwd: "/w"}, claudedriver.HookEnv{Socket: sock, Token: "t"})
	address := service.SessionAddress("claude-code", "session-4")
	if !strings.Contains(string(out), `"hookEventName":"SessionStart"`) || !strings.Contains(string(out), address) {
		t.Fatalf("session-start output=%s", out)
	}
	fill := claudedriver.RunHook(h.ctx, api.URL, "fill-notify", claudedriver.HookInput{SessionID: "session-4", ToolInput: []byte(`{"task_id":7,"ref":"main"}`)}, claudedriver.HookEnv{})
	if !strings.Contains(string(fill), `"updatedInput"`) || !strings.Contains(string(fill), `"notify_address":"`+address+`"`) || !strings.Contains(string(fill), `"task_id":7`) {
		t.Fatalf("fill-notify output=%s", fill)
	}
	kept := claudedriver.RunHook(h.ctx, api.URL, "fill-notify", claudedriver.HookInput{SessionID: "session-4", ToolInput: []byte(`{"notify_address":"s/other"}`)}, claudedriver.HookEnv{})
	if len(kept) != 0 {
		t.Fatalf("overwrote an explicit notify_address: %s", kept)
	}
	if out := claudedriver.RunHook(h.ctx, "http://127.0.0.1:1", "session-start", claudedriver.HookInput{SessionID: "x"}, claudedriver.HookEnv{Socket: sock}); len(out) != 0 {
		t.Fatalf("unreachable driver produced output: %s", out)
	}
}

func TestSendFromASessionReachesAnotherSession(t *testing.T) {
	h := newHarness(t)
	alice := h.register(t, h.driver, "alice", h.socket(t, "alice"))
	bobSock := h.socket(t, "bob")
	bob := h.register(t, h.driver, "bob", bobSock)
	api := httptest.NewServer(claudedriver.Handler(h.driver))
	defer api.Close()
	sessions, skipped := crewcli.Who(h.ctx, map[string]string{"claude-code": api.URL})
	if len(skipped) != 0 || len(sessions) != 2 {
		t.Fatalf("who=%#v skipped=%v", sessions, skipped)
	}
	to, err := crewcli.Resolve("bo", sessions)
	if err != nil || to != bob {
		t.Fatalf("resolve=%q %v, want %q", to, err, bob)
	}
	if _, err := crewcli.Resolve("s/", sessions); err == nil {
		t.Fatal("an ambiguous prefix resolved")
	}
	result, err := crewcli.Send(h.ctx, api.URL, crewcli.SendRequest{From: "alice", To: to, Body: "your turn", Policy: "next_step", OperationID: "a-1"})
	if err != nil || result.From != alice {
		t.Fatalf("send=%#v %v", result, err)
	}
	again, err := crewcli.Send(h.ctx, api.URL, crewcli.SendRequest{From: "alice", To: to, Body: "your turn", Policy: "next_step", OperationID: "a-1"})
	if err != nil || !again.Replayed || again.MessageID != result.MessageID {
		t.Fatalf("retry=%#v %v", again, err)
	}
	h.driver.SetBusy("bob", true)
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].socket != bobSock || sent[0].priority != claudedriver.PriorityNext || !strings.Contains(sent[0].text, "Crew message from "+alice) {
		t.Fatalf("delivered=%#v", sent)
	}
	if _, err := crewcli.Send(h.ctx, api.URL, crewcli.SendRequest{From: "stranger", To: to, Body: "x", OperationID: "s-1"}); err == nil {
		t.Fatal("an unregistered session sent a message")
	}
}

// flakyAck fails acknowledgements: "fail" before the fabric sees them, "lose"
// after the fabric applied them (the response is lost).
type flakyAck struct {
	*fabricclient.Client
	mu   sync.Mutex
	mode string
	ops  []string
}

func (f *flakyAck) Acknowledge(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error) {
	f.mu.Lock()
	mode := f.mode
	f.ops = append(f.ops, request.OperationID)
	f.mu.Unlock()
	switch mode {
	case "fail":
		return fabricclient.Delivery{}, errors.New("connection refused")
	case "lose":
		_, _ = f.Client.Acknowledge(ctx, deliveryID, request)
		return fabricclient.Delivery{}, errors.New("connection reset")
	}
	return f.Client.Acknowledge(ctx, deliveryID, request)
}

func (f *flakyAck) set(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

// The transcript line that proves an insertion is read once; the offset moves
// past it. A failed acknowledgement must keep that fact, retry the same
// operation without posting again, and survive a driver restart.
func TestFailedAcknowledgementIsRetriedFromSavedInsertion(t *testing.T) {
	h := newHarness(t)
	flaky := &flakyAck{Client: h.fabric, mode: "fail"}
	driver := h.newDriverWith(t, flaky)
	if err := driver.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	sock := h.socket(t, "ten")
	address := h.register(t, driver, "session-10", sock)
	driver.SetBusy("session-10", true)
	h.send(t, "first", address, "acknowledge me", "next_step")
	h.send(t, "second", address, "behind it", "next_step")
	driver.Pump(h.ctx)
	if got := h.states(t, address); strings.Join(got, ",") != "dispatching,queued" {
		t.Fatalf("after a failed ack=%v", got)
	}
	state, err := os.ReadFile(filepath.Join(h.dir, "state.json"))
	if err != nil || !strings.Contains(string(state), `"inserted": true`) {
		t.Fatalf("insertion not saved before the ack: %s %v", state, err)
	}
	// The fabric applies the ack but the response is lost: still pending here.
	flaky.set("lose")
	driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 1 {
		t.Fatalf("reposted after a failed ack (%d posts)", n)
	}

	// A restarted driver reads the saved insertion and acknowledges with the
	// same operation ID; the fabric replays it and the next message goes out.
	restarted := h.newDriver(t)
	if err := restarted.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	restarted.Pump(h.ctx)
	if got := h.states(t, address); strings.Join(got, ",") != "delivered,delivered" {
		t.Fatalf("after restart=%v", got)
	}
	sent := h.inbox.sent()
	if len(sent) != 2 || !strings.Contains(sent[1].text, "behind it") {
		t.Fatalf("posts=%#v", sent)
	}
	flaky.mu.Lock()
	defer flaky.mu.Unlock()
	if len(flaky.ops) != 2 || flaky.ops[0] != flaky.ops[1] {
		t.Fatalf("ack operation ids=%v, want one id retried", flaky.ops)
	}
}
