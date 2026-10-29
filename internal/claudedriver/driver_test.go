package claudedriver_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/claudedriver"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

type posted struct{ socket, msgID, text, priority string }

// fakeInbox records posts. A socket listed in gone refuses connections, and
// one in failPost accepts the connection but fails the write.
type fakeInbox struct {
	mu       sync.Mutex
	posts    []posted
	gone     map[string]bool
	failPost map[string]bool
}

func (f *fakeInbox) Dial(_ context.Context, socket, _ string) (claudedriver.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[socket] {
		return nil, claudedriver.ErrInboxGone
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
	return nil
}
func (c *fakeConn) Close() error { return nil }

type harness struct {
	ctx      context.Context
	fabric   *fabricclient.Client
	inbox    *fakeInbox
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
	h := &harness{ctx: ctx, fabric: fabric, inbox: &fakeInbox{gone: map[string]bool{}, failPost: map[string]bool{}}, dir: dir}
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
	d, err := claudedriver.New(h.fabric, h.inbox, time.Now, claudedriver.Config{AdapterID: "claude-code", InstanceID: "test", StatePath: filepath.Join(h.dir, "state.json"), LeaseDuration: time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func (h *harness) send(t *testing.T, op, to, body, policy string) {
	t.Helper()
	sender := service.SessionAddress("producer", "sender")
	if _, err := h.fabric.Submit(h.ctx, fabricclient.MessageRequest{ProducerID: "producer", LeaseToken: h.producer.LeaseToken, OperationID: op, SenderAddress: sender, RecipientAddress: to, Body: body, ActivationPolicy: policy, TTL: "1h"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryFollowsPolicyAndTurnState(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "one")
	address, err := h.driver.RegisterSession(h.ctx, "session-1", sock, "tok", "/work")
	if err != nil || !strings.HasPrefix(address, "s/") {
		t.Fatalf("register=%q %v", address, err)
	}
	again, err := h.driver.RegisterSession(h.ctx, "session-1", sock, "tok", "/work")
	if err != nil || again != address {
		t.Fatalf("re-register=%q %v, want %q", again, err, address)
	}

	// Busy: next_step reaches the active turn; next_turn waits for it to end.
	h.driver.SetBusy("session-1", true)
	h.send(t, "step", address, "gate failed", "next_step")
	h.send(t, "turn", address, "later please", "next_turn")
	h.driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 2 || sent[0].priority != claudedriver.PriorityNext || sent[1].priority != claudedriver.PriorityLater {
		t.Fatalf("busy posts=%#v", sent)
	}
	if !strings.Contains(sent[0].text, "gate failed") || !strings.Contains(sent[0].text, "Crew message from s/") {
		t.Fatalf("rendered text=%q", sent[0].text)
	}
	if len(sent[0].msgID) != 36 || sent[0].msgID == sent[1].msgID {
		t.Fatalf("native ids=%q %q", sent[0].msgID, sent[1].msgID)
	}

	// no_wake waits while idle and goes out with the next busy turn.
	h.driver.SetBusy("session-1", false)
	h.send(t, "quiet", address, "fyi", "no_wake")
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 2 {
		t.Fatalf("no_wake delivered while idle (%d posts)", n)
	}
	h.driver.SetBusy("session-1", true)
	h.driver.Pump(h.ctx)
	if sent := h.inbox.sent(); len(sent) != 3 || sent[2].priority != claudedriver.PriorityNext {
		t.Fatalf("no_wake while busy=%#v", sent)
	}
}

func TestExitedSessionKeepsWorkUntilItResumes(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "two")
	address, err := h.driver.RegisterSession(h.ctx, "session-2", sock, "", "")
	if err != nil {
		t.Fatal(err)
	}
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
	if got, err := driver.RegisterSession(h.ctx, "session-2", resumed, "", ""); err != nil || got != address {
		t.Fatalf("resume register=%q %v, want %q", got, err, address)
	}
	driver.Pump(h.ctx)
	sent := h.inbox.sent()
	if len(sent) != 1 || sent[0].socket != resumed || !strings.Contains(sent[0].text, "while you were away") {
		t.Fatalf("after resume=%#v", sent)
	}
}

func TestFailedWriteIsNotRedelivered(t *testing.T) {
	h := newHarness(t)
	sock := h.socket(t, "three")
	address, err := h.driver.RegisterSession(h.ctx, "session-3", sock, "", "")
	if err != nil {
		t.Fatal(err)
	}
	h.inbox.failPost[sock] = true
	h.send(t, "lost", address, "maybe seen", "next_step")
	h.driver.Pump(h.ctx)
	h.inbox.failPost[sock] = false
	h.driver.Pump(h.ctx)
	if n := len(h.inbox.sent()); n != 0 {
		t.Fatalf("an ambiguous post was retried blindly (%d posts)", n)
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
