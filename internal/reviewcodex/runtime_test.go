package reviewcodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/codexadapter"
	"crew-services/internal/review"
)

type fakeServer struct {
	mu            sync.Mutex
	handler       func(context.Context, json.RawMessage) (json.RawMessage, error)
	options       []codexadapter.EphemeralThreadOptions
	next          int
	starts        chan string
	waits         map[string]chan codexadapter.TurnCompletion
	forgotten     []string
	closed        bool
	startErr      error
	interactions  []codexadapter.NativeInteraction
	interrupts    []string
	startBlock    chan struct{}
	turnStartHook func(thread, turn string)
	turnOptions   []codexadapter.EphemeralTurnOptions
}

func (f *fakeServer) Initialize(context.Context) error { return nil }
func (f *fakeServer) SetDynamicToolHandler(h func(context.Context, json.RawMessage) (json.RawMessage, error)) {
	f.handler = h
}
func (f *fakeServer) StartEphemeralThread(_ context.Context, o codexadapter.EphemeralThreadOptions) (codexadapter.NativeThread, error) {
	f.mu.Lock()
	if f.startErr != nil {
		err := f.startErr
		f.mu.Unlock()
		return codexadapter.NativeThread{}, err
	}
	block := f.startBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.options = append(f.options, o)
	return codexadapter.NativeThread{ID: "thread-" + itoa(f.next)}, nil
}
func (f *fakeServer) StartEphemeralTurn(_ context.Context, thread, _ string, options codexadapter.EphemeralTurnOptions) (codexadapter.StartedTurn, error) {
	f.mu.Lock()
	f.turnOptions = append(f.turnOptions, options)
	f.next++
	id := "turn-" + itoa(f.next)
	if f.waits == nil {
		f.waits = map[string]chan codexadapter.TurnCompletion{}
	}
	f.waits[thread+"/"+id] = make(chan codexadapter.TurnCompletion, 1)
	if f.starts != nil {
		f.starts <- thread + "/" + id
	}
	hook := f.turnStartHook
	f.mu.Unlock()
	if hook != nil {
		hook(thread, id)
	}
	return codexadapter.StartedTurn{ID: id}, nil
}
func (f *fakeServer) WaitTurn(ctx context.Context, thread, turn string) (codexadapter.TurnCompletion, error) {
	f.mu.Lock()
	ch := f.waits[thread+"/"+turn]
	f.mu.Unlock()
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		return codexadapter.TurnCompletion{}, ctx.Err()
	}
}
func (f *fakeServer) finish(thread, turn string) {
	f.mu.Lock()
	ch := f.waits[thread+"/"+turn]
	f.mu.Unlock()
	ch <- codexadapter.TurnCompletion{ThreadID: thread, TurnID: turn, Status: "completed"}
}
func (f *fakeServer) ForgetThread(thread string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, thread)
}
func (f *fakeServer) Interactions() []codexadapter.NativeInteraction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codexadapter.NativeInteraction(nil), f.interactions...)
}
func (f *fakeServer) Interrupt(_ context.Context, thread, turn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, thread+"/"+turn)
	return nil
}
func (f *fakeServer) Close() error { f.closed = true; return nil }
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "x"
}
func runtimeFixture(t *testing.T) (*Runtime, *fakeServer) {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "SOUL.md")
	if e := os.WriteFile(profile, []byte("review procedure"), 0600); e != nil {
		t.Fatal(e)
	}
	server := &fakeServer{}
	r, e := NewWithServer(context.Background(), server, 2, profile)
	if e != nil {
		t.Fatal(e)
	}
	return r, server
}
func call(thread, turn, verdict string) json.RawMessage {
	arguments := map[string]any{"verdict": verdict, "notes": "ok"}
	if verdict == "changes_requested" {
		arguments["new_findings"] = []map[string]any{{"category": "blocking_bug", "summary": "current round regression"}}
	}
	b, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "callId": "call", "tool": "complete_review", "arguments": arguments})
	return b
}

func callWithArguments(thread, turn string, arguments map[string]any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "callId": "call", "tool": "complete_review", "arguments": arguments})
	return b
}

func TestEphemeralProfileAndSchema(t *testing.T) {
	r, s := runtimeFixture(t)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "/repo")
	if e != nil {
		t.Fatal(e)
	}
	if len(s.options) != 1 || !s.options[0].ReadOnly || s.options[0].CWD != "/repo" || !strings.Contains(s.options[0].DeveloperInstructions, "Managed review runtime") {
		t.Fatalf("options=%+v", s.options)
	}
	if !strings.Contains(s.options[0].DeveloperInstructions, "blocking_bug, acceptance_gap, test_weakness, or follow_up_candidate") || !strings.Contains(s.options[0].DeveloperInstructions, "verified_fixed, not_fixed, superseded, or split_to_follow_up") || !strings.Contains(s.options[0].DeveloperInstructions, "prior-finding resolution with status not_fixed alone is insufficient") {
		t.Fatalf("instructions do not name allowed values: %q", s.options[0].DeveloperInstructions)
	}
	raw, _ := json.Marshal(completionTool())
	if strings.Contains(string(raw), "project_id") || strings.Contains(string(raw), "review_round_id") || strings.Contains(string(raw), "correlation_id") {
		t.Fatalf("tool leaks controller identity: %s", raw)
	}
	if e = r.Release(context.Background(), w); e != nil {
		t.Fatal(e)
	}
}

func TestConfiguredModelAndEffortReachEveryReviewTurn(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "reviewer.md")
	if e := os.WriteFile(profile, []byte("review procedure"), 0600); e != nil {
		t.Fatal(e)
	}
	s := &fakeServer{starts: make(chan string, 1)}
	r, e := newWithServer(context.Background(), s, 1, profile, "gpt-5.6-sol", "medium")
	if e != nil {
		t.Fatal(e)
	}
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "/repo")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), w, "review", func(review.Completion) error { return nil }) }()
	parts := strings.Split(<-s.starts, "/")
	if _, e = s.handler(context.Background(), call(parts[0], parts[1], "looks_good")); e != nil {
		t.Fatal(e)
	}
	s.finish(parts[0], parts[1])
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.turnOptions) != 1 || s.turnOptions[0].Model != "gpt-5.6-sol" || s.turnOptions[0].Effort != "medium" {
		t.Fatalf("turn options = %+v", s.turnOptions)
	}
}

func TestCompletionToolConstrainsDenFindingValues(t *testing.T) {
	raw, e := json.Marshal(completionTool())
	if e != nil {
		t.Fatal(e)
	}
	var tools []struct {
		Description string `json:"description"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"inputSchema"`
	}
	if e = json.Unmarshal(raw, &tools); e != nil {
		t.Fatal(e)
	}
	if len(tools) != 1 {
		t.Fatalf("tool count=%d", len(tools))
	}
	if !strings.Contains(tools[0].Description, "changes_requested verdict requires at least one valid new finding") || !strings.Contains(tools[0].Description, "not_fixed alone is insufficient") {
		t.Fatalf("tool description does not explain actionable findings: %q", tools[0].Description)
	}
	assertNestedEnum(t, tools[0].InputSchema.Properties["new_findings"], []string{"blocking_bug", "acceptance_gap", "test_weakness", "follow_up_candidate"})
	assertNestedEnum(t, tools[0].InputSchema.Properties["prior_finding_resolutions"], []string{"verified_fixed", "not_fixed", "superseded", "split_to_follow_up"})
}

func assertNestedEnum(t *testing.T, raw json.RawMessage, want []string) {
	t.Helper()
	var schema struct {
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	if e := json.Unmarshal(raw, &schema); e != nil {
		t.Fatal(e)
	}
	field := "category"
	if schema.Items.Properties[field] == nil {
		field = "status"
	}
	var value struct {
		Enum []string `json:"enum"`
	}
	if e := json.Unmarshal(schema.Items.Properties[field], &value); e != nil {
		t.Fatal(e)
	}
	if len(value.Enum) != len(want) {
		t.Fatalf("%s enum=%v, want %v", field, value.Enum, want)
	}
	for i := range want {
		if value.Enum[i] != want[i] {
			t.Fatalf("%s enum=%v, want %v", field, value.Enum, want)
		}
	}
}

func TestToolRejectsInvalidDenFindingValues(t *testing.T) {
	r, s := runtimeFixture(t)
	cases := []struct {
		name      string
		arguments map[string]any
	}{
		{name: "changes requested without finding", arguments: map[string]any{"verdict": "changes_requested"}},
		{name: "prior not fixed without current finding", arguments: map[string]any{"verdict": "changes_requested", "prior_finding_resolutions": []map[string]any{{"finding_id": 1, "status": "not_fixed", "verification_note": "still fails"}}}},
		{name: "category", arguments: map[string]any{"verdict": "changes_requested", "new_findings": []map[string]any{{"category": "arbitrary", "summary": "bad enum"}}}},
		{name: "resolution status", arguments: map[string]any{"verdict": "looks_good", "prior_finding_resolutions": []map[string]any{{"finding_id": 1, "status": "arbitrary", "verification_note": "bad enum"}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			out, e := s.handler(context.Background(), callWithArguments("thread", "turn", testCase.arguments))
			if e != nil || !strings.Contains(string(out), "invalid completion arguments") {
				t.Fatalf("result=%s err=%v", out, e)
			}
		})
	}
	_ = r
}
func TestSequentialTurnsAndCompletionRules(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 2)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "/repo")
	if e != nil {
		t.Fatal(e)
	}
	for _, verdict := range []string{"looks_good", "changes_requested"} {
		done := make(chan error, 1)
		calls := 0
		go func() {
			done <- r.Run(context.Background(), w, "review", func(review.Completion) error { calls++; return nil })
		}()
		pair := <-s.starts
		parts := strings.Split(pair, "/")
		if _, e := s.handler(context.Background(), call(parts[0], parts[1], verdict)); e != nil {
			t.Fatal(e)
		}
		if _, e := s.handler(context.Background(), call(parts[0], parts[1], verdict)); e != nil {
			t.Fatal(e)
		}
		other := "looks_good"
		if verdict == other {
			other = "changes_requested"
		}
		if out, e := s.handler(context.Background(), call(parts[0], parts[1], other)); e != nil || !strings.Contains(string(out), "conflicting") {
			t.Fatalf("conflict=%s %v", out, e)
		}
		s.finish(parts[0], parts[1])
		if e := <-done; e != nil {
			t.Fatal(e)
		}
		if calls != 1 {
			t.Fatalf("calls=%d", calls)
		}
	}
}
func TestMissingCompletionPoolBoundAndRelease(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 1)
	w1, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	w2, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 2}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 3}, "", ""); e == nil {
		t.Fatal("capacity accepted third worker")
	}
	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), w1, "review", func(review.Completion) error { return nil })
	}()
	pair := <-s.starts
	parts := strings.Split(pair, "/")
	s.finish(parts[0], parts[1])
	if e := <-done; e == nil || !strings.Contains(e.Error(), "without complete_review") {
		t.Fatalf("missing completion=%v", e)
	}
	if e = r.Release(context.Background(), w1); e != nil {
		t.Fatal(e)
	}
	if e = r.Release(context.Background(), w2); e != nil {
		t.Fatal(e)
	}
	if e = r.Run(context.Background(), w1, "", func(review.Completion) error { return nil }); e == nil {
		t.Fatal("released worker accepted")
	}
}
func TestMissingCompletionIncludesRejectedToolReason(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 1)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), w, "review", func(review.Completion) error { return nil }) }()
	parts := strings.Split(<-s.starts, "/")
	out, e := s.handler(context.Background(), callWithArguments(parts[0], parts[1], map[string]any{"verdict": "changes_requested", "new_findings": []map[string]any{{"category": "not_an_allowed_category", "summary": "bad enum"}}}))
	if e != nil || !strings.Contains(string(out), "invalid completion arguments") {
		t.Fatalf("tool result=%s err=%v", out, e)
	}
	s.finish(parts[0], parts[1])
	if e := <-done; e == nil || !strings.Contains(e.Error(), "without complete_review: invalid completion arguments") {
		t.Fatalf("run result=%v", e)
	}
}

func TestCallbackRejectionAllowsCorrectedCompletion(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 1)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), w, "review", func(review.Completion) error {
			calls++
			if calls == 1 {
				return errors.New("encoded completion is too large")
			}
			return nil
		})
	}()
	parts := strings.Split(<-s.starts, "/")
	first, e := s.handler(context.Background(), callWithArguments(parts[0], parts[1], map[string]any{"verdict": "looks_good", "notes": "long"}))
	if e != nil || !strings.Contains(string(first), "too large") {
		t.Fatalf("first completion=%s err=%v", first, e)
	}
	second, e := s.handler(context.Background(), callWithArguments(parts[0], parts[1], map[string]any{"verdict": "looks_good", "notes": "short"}))
	if e != nil || !strings.Contains(string(second), "completion accepted") {
		t.Fatalf("corrected completion=%s err=%v", second, e)
	}
	s.finish(parts[0], parts[1])
	if e := <-done; e != nil || calls != 2 {
		t.Fatalf("run result=%v calls=%d", e, calls)
	}
}

func TestCallbackRejectionBeforeTurnStartReturnsAllowsCorrection(t *testing.T) {
	r, s := runtimeFixture(t)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	s.turnStartHook = func(thread, turn string) {
		first, err := s.handler(context.Background(), callWithArguments(thread, turn, map[string]any{"verdict": "looks_good", "notes": "long"}))
		if err != nil || !strings.Contains(string(first), "too large") {
			t.Errorf("first completion=%s err=%v", first, err)
		}
		second, err := s.handler(context.Background(), callWithArguments(thread, turn, map[string]any{"verdict": "looks_good", "notes": "short"}))
		if err != nil || !strings.Contains(string(second), "completion accepted") {
			t.Errorf("corrected completion=%s err=%v", second, err)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- r.Run(context.Background(), w, "review", func(review.Completion) error {
			calls++
			if calls == 1 {
				return errors.New("encoded completion is too large")
			}
			return nil
		})
	}()
	for {
		s.mu.Lock()
		var key string
		for candidate := range s.waits {
			key = candidate
			break
		}
		s.mu.Unlock()
		if key != "" {
			parts := strings.Split(key, "/")
			s.finish(parts[0], parts[1])
			break
		}
		time.Sleep(time.Millisecond)
	}
	if e := <-done; e != nil || calls != 2 {
		t.Fatalf("run result=%v calls=%d", e, calls)
	}
}
func TestSecondTurnAcceptsCompletionBeforeStartReturns(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 2)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	run := func() chan error {
		done := make(chan error, 1)
		go func() { done <- r.Run(context.Background(), w, "review", func(review.Completion) error { return nil }) }()
		return done
	}
	firstDone := run()
	first := strings.Split(<-s.starts, "/")
	if _, e := s.handler(context.Background(), call(first[0], first[1], "looks_good")); e != nil {
		t.Fatal(e)
	}
	s.finish(first[0], first[1])
	if e := <-firstDone; e != nil {
		t.Fatal(e)
	}

	toolResult := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.turnStartHook = func(thread, turn string) {
		out, _ := s.handler(context.Background(), call(thread, turn, "changes_requested"))
		toolResult <- out
	}
	s.mu.Unlock()
	secondDone := run()
	second := strings.Split(<-s.starts, "/")
	if out := <-toolResult; !strings.Contains(string(out), "completion accepted") {
		t.Fatalf("pre-return completion=%s", out)
	}
	s.finish(second[0], second[1])
	if e := <-secondDone; e != nil {
		t.Fatal(e)
	}
}
func TestToolRejectsLateAndForeignThread(t *testing.T) {
	r, s := runtimeFixture(t)
	if _, e := s.handler(context.Background(), call("gone", "turn", "looks_good")); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(must(s.handler(context.Background(), call("gone", "turn", "looks_good")))), "no longer active") {
		t.Fatal("late call was not rejected")
	}
	_ = r
}
func TestAcquireReservesCapacityDuringBlockedStart(t *testing.T) {
	r, s := runtimeFixture(t)
	r.capacity = 1
	s.startBlock = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
		done <- e
	}()
	time.Sleep(10 * time.Millisecond)
	if _, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 2}, "", ""); e == nil {
		t.Fatal("concurrent acquire exceeded reserved capacity")
	}
	close(s.startBlock)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestInteractionInterruptsOnlyExactTurn(t *testing.T) {
	r, s := runtimeFixture(t)
	s.starts = make(chan string, 1)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), w, "review", func(review.Completion) error { return nil }) }()
	pair := <-s.starts
	parts := strings.Split(pair, "/")
	s.mu.Lock()
	s.interactions = []codexadapter.NativeInteraction{{ThreadID: "other", TurnID: parts[1]}}
	s.mu.Unlock()
	time.Sleep(35 * time.Millisecond)
	if len(s.interrupts) != 0 {
		t.Fatal("other interaction interrupted active review")
	}
	s.mu.Lock()
	s.interactions = []codexadapter.NativeInteraction{{ThreadID: parts[0], TurnID: parts[1]}}
	s.mu.Unlock()
	if e := <-done; e == nil || !strings.Contains(e.Error(), "noninteractive") {
		t.Fatalf("interaction result=%v", e)
	}
	if len(s.interrupts) != 1 {
		t.Fatalf("interrupts=%v", s.interrupts)
	}
}
func TestLossRestartInvalidatesOldWorkers(t *testing.T) {
	r, old := runtimeFixture(t)
	w, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 1}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	old.startErr = errors.New("child exited")
	fresh := &fakeServer{}
	r.factory = func(context.Context) (codexadapter.EphemeralServer, error) { return fresh, nil }
	newWorker, e := r.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh", TaskID: 2}, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Run(context.Background(), w, "", func(review.Completion) error { return nil }); e == nil {
		t.Fatal("old worker survived child loss")
	}
	if e = r.Release(context.Background(), newWorker); e != nil {
		t.Fatal(e)
	}
	if !old.closed || len(fresh.options) != 1 {
		t.Fatalf("old.closed=%v fresh=%d", old.closed, len(fresh.options))
	}
}
func must(v json.RawMessage, e error) json.RawMessage {
	if e != nil {
		panic(e)
	}
	return v
}

var _ = errors.New
