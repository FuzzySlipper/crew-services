package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is a product host: an output stream naming the binding, control
// claims that move it, an input lane that checks sequences, and debug commands.
type fakeHost struct {
	mu        sync.Mutex
	revision  int
	claimed   bool
	next      int
	events    []map[string]any // every accepted wire event, in order
	batches   int
	inputFail func(batch int) (status int, body string, header string)
	commands  []string
	step      int
	moved     float64
	held      map[string]bool
	stream    chan string
}

func newFakeHost() *fakeHost {
	return &fakeHost{revision: 1, next: 1, held: map[string]bool{}, stream: make(chan string, 64)}
}

func (f *fakeHost) binding() map[string]string {
	return map[string]string{"instanceId": "7", "generation": "1", "controlRevision": fmt.Sprint(f.revision)}
}

func (f *fakeHost) publish(outputs ...map[string]any) {
	data, _ := json.Marshal(outputs)
	f.stream <- "data: " + string(data) + "\n\n"
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case runtimePrefix + "/outputs/fresh":
		w.Header().Set("Content-Type", "text/event-stream")
		baseline, _ := json.Marshal(map[string]any{"accepted": true, "binding": f.binding(), "nextInputSequence": fmt.Sprint(f.next)})
		fmt.Fprintf(w, "event: rusty-output-baseline\ndata: %s\n\n", baseline)
		w.(http.Flusher).Flush()
		f.mu.Unlock()
		defer f.mu.Lock()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-f.stream:
				io.WriteString(w, event)
				w.(http.Flusher).Flush()
			}
		}
	case runtimePrefix + "/control/claim":
		var request struct {
			Runtime map[string]string `json:"runtime"`
			Label   string            `json:"label"`
			LeaseMS string            `json:"leaseMs"`
		}
		if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &request) != nil || request.LeaseMS == "" {
			http.Error(w, "bad claim", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Rusty-Commit-Disposition", "committed")
		if request.Runtime["controlRevision"] != fmt.Sprint(f.revision) {
			w.Header().Set("X-Rusty-Commit-Disposition", "not-applied")
			json.NewEncoder(w).Encode(map[string]any{"accepted": false, "code": "CSHARP_CONTROL_BINDING", "disposition": "rejected-recoverable"})
			return
		}
		f.revision++
		f.claimed, f.next, f.held = true, 1, map[string]bool{}
		json.NewEncoder(w).Encode(map[string]any{"accepted": true, "code": "PRODUCT_HOST_ACCEPTED", "binding": f.binding(), "nextInputSequence": "1"})
		f.publish(map[string]any{"kind": "binding", "runtime": f.binding(), "nextInputSequence": "1", "inputClaim": request.Label})
	case runtimePrefix + "/control/release":
		f.revision++
		f.claimed = false
		w.Header().Set("X-Rusty-Commit-Disposition", "committed")
		json.NewEncoder(w).Encode(map[string]any{"accepted": true, "binding": f.binding(), "nextInputSequence": "1"})
	case runtimePrefix + "/input":
		f.batches++
		if f.inputFail != nil {
			if status, answer, header := f.inputFail(f.batches); status != 0 {
				w.Header().Set("X-Rusty-Commit-Disposition", header)
				w.WriteHeader(status)
				io.WriteString(w, answer)
				return
			}
		}
		var request struct {
			Batch []map[string]any `json:"batch"`
		}
		json.Unmarshal(body, &request)
		for _, event := range request.Batch {
			if event["sequence"] != fmt.Sprint(f.next) || event["context"] != gameplayContext {
				w.Header().Set("X-Rusty-Commit-Disposition", "not-applied")
				json.NewEncoder(w).Encode(map[string]any{"accepted": false, "code": "CSHARP_INPUT_SEQUENCE_OUT_OF_ORDER", "disposition": "rejected-recoverable"})
				return
			}
			f.next++
			f.events = append(f.events, event)
			fact := event["fact"].(map[string]any)
			if fact["kind"] == "key" {
				f.held[fmt.Sprint(fact["code"])] = fact["edge"] == "pressed"
			}
		}
		w.Header().Set("X-Rusty-Commit-Disposition", "committed")
		json.NewEncoder(w).Encode(map[string]any{"accepted": true, "code": "PRODUCT_HOST_INPUT_QUEUED", "count": len(request.Batch)})
	case runtimePrefix + "/debug/execute":
		command := string(body)
		f.commands = append(f.commands, command)
		// Queued input reaches the runtime with each debug command.
		if f.next > 1 {
			f.publish(map[string]any{"kind": "runtime-input-result", "result": map[string]any{"accepted": true, "binding": f.binding(), "acceptedThrough": fmt.Sprint(f.next - 1), "nextInputSequence": fmt.Sprint(f.next)}})
		}
		var answer any
		switch {
		case command == "engine.time":
			answer = map[string]any{"mode": "manual", "fixedStepHz": 60, "simulationStep": fmt.Sprint(f.step), "advancedMs": 0}
		case strings.HasPrefix(command, "engine.time.advance "):
			var ms float64
			fmt.Sscan(strings.TrimPrefix(command, "engine.time.advance "), &ms)
			f.step += int(ms * 60 / 1000)
			if f.held["key-w"] {
				f.moved += ms / 100
			}
			answer = map[string]any{"mode": "manual", "fixedStepHz": 60, "simulationStep": fmt.Sprint(f.step), "advancedMs": ms}
		case command == "playtest.action forward":
			answer = map[string]any{"available": true, "key": "KeyW", "durationMs": 200, "hold": true}
		case command == "playtest.observe":
			answer = map[string]any{"player": map[string]any{"position": map[string]any{"x": 0.0, "y": 0.0, "z": -f.moved}, "health": 100.0}}
		case command == "refuse":
			http.Error(w, "usage: refuse", http.StatusUnprocessableEntity)
			return
		default:
			answer = map[string]any{}
		}
		json.NewEncoder(w).Encode(answer)
	default:
		http.NotFound(w, r)
	}
}

func startFake(t *testing.T) (*fakeHost, *Host) {
	t.Helper()
	fake := newFakeHost()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	host, err := NewHost(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return fake, host
}

func TestClaimSendsIncreasingSequencesUnderTheClaimedBinding(t *testing.T) {
	fake, host := startFake(t)
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	for _, code := range []string{"key-w", "key-a"} {
		receipt, err := c.send(context.Background(), []fact{{"kind": "key", "code": code, "edge": "pressed"}, {"kind": "key", "code": code, "edge": "released"}})
		if err != nil || receipt.Delivery != "queued" {
			t.Fatalf("send: %+v %v", receipt, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.events) != 4 || fake.batches != 2 {
		t.Fatalf("events %d batches %d", len(fake.events), fake.batches)
	}
	for i, event := range fake.events {
		runtime := event["runtime"].(map[string]any)
		if event["sequence"] != fmt.Sprint(i+1) || runtime["controlRevision"] != "2" {
			t.Fatalf("event %d: %v", i, event)
		}
	}
}

func TestUncertainInputIsNeverReplayedAndTheNextSendReclaims(t *testing.T) {
	fake, host := startFake(t)
	fake.inputFail = func(batch int) (int, string, string) {
		if batch == 1 {
			return http.StatusInternalServerError, `{"accepted":false,"code":"PRODUCT_HOST_RUNTIME"}`, "unknown"
		}
		return 0, "", ""
	}
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	press := []fact{{"kind": "key", "code": "key-w", "edge": "pressed"}}
	receipt, err := c.send(context.Background(), press)
	if err == nil || !strings.HasPrefix(receipt.Delivery, "uncertain") || c.uncertain == "" {
		t.Fatalf("unknown outcome not reported: %+v %v", receipt, err)
	}
	// Releasing held input after an uncertain batch must not send anything.
	if err := c.releaseHeld(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.batches != 1 {
		t.Fatalf("uncertain input was followed by %d batches", fake.batches-1)
	}
	receipt, err = c.send(context.Background(), []fact{{"kind": "key", "code": "key-d", "edge": "pressed"}})
	if err != nil || receipt.Delivery != "queued" {
		t.Fatalf("send after reclaim: %+v %v", receipt, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revision != 3 || len(fake.events) != 1 || fake.events[0]["fact"].(map[string]any)["code"] != "key-d" {
		t.Fatalf("expected one fresh claim and only the new event: revision %d events %v", fake.revision, fake.events)
	}
}

func TestInputRefusedBeforeDispatchKeepsItsSequence(t *testing.T) {
	fake, host := startFake(t)
	fake.inputFail = func(batch int) (int, string, string) {
		if batch == 1 {
			return http.StatusBadRequest, `{"accepted":false,"error":{"code":"PRODUCT_HOST_JSON"}}`, ""
		}
		return 0, "", ""
	}
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	if receipt, err := c.send(context.Background(), []fact{{"kind": "key", "code": "key-w", "edge": "pressed"}}); err == nil || receipt.Delivery != "not-applied" {
		t.Fatalf("refusal: %+v %v", receipt, err)
	}
	if _, err := c.send(context.Background(), []fact{{"kind": "key", "code": "key-w", "edge": "pressed"}}); err != nil {
		t.Fatal(err)
	}
	if fake.revision != 2 || fake.events[0]["sequence"] != "1" {
		t.Fatalf("a refused batch consumed a sequence or forced a claim: revision %d %v", fake.revision, fake.events)
	}
}

func TestClaimFollowsABindingThatMovedAway(t *testing.T) {
	fake, host := startFake(t)
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	if _, err := c.send(context.Background(), []fact{{"kind": "key", "code": "key-w", "edge": "pressed"}}); err != nil {
		t.Fatal(err)
	}
	// The lease lapses: the runtime moves the binding and the page has input.
	fake.mu.Lock()
	fake.revision++
	fake.next = 1
	fake.publish(map[string]any{"kind": "binding", "runtime": fake.binding(), "nextInputSequence": "1"})
	fake.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for c.stream.snapshot().claimLabel != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.send(context.Background(), []fact{{"kind": "key", "code": "key-w", "edge": "released"}}); err != nil {
		t.Fatal(err)
	}
	if fake.revision != 4 {
		t.Fatalf("expected a fresh claim on the moved binding, revision %d", fake.revision)
	}
}

func TestActHoldsThroughHeldTimeAndReleases(t *testing.T) {
	fake, host := startFake(t)
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	r := &runner{host: host, directory: t.TempDir(), input: c}
	result, err := r.run(context.Background(), Request{Op: "act", ID: "forward", MS: ptr(300.0)})
	if err != nil {
		t.Fatal(err)
	}
	if result["accepted"] != true || result["advancedMs"] != 300.0 || result["admission"] != "admitted by the runtime" {
		t.Fatalf("act: %v", result)
	}
	if moved := result["delta"].(map[string]any)["distanceMoved"].(float64); math.Abs(moved-3) > 1e-9 {
		t.Fatalf("held key did not span the advances: moved %v (%v)", moved, fake.commands)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.held["key-w"] {
		t.Fatal("forward is still held after act")
	}
}

func TestDebugRefusalsAndNonJSONAnswers(t *testing.T) {
	_, host := startFake(t)
	var refused *RefusedError
	if _, err := host.Debug(context.Background(), "refuse"); err == nil || !asRefused(err, &refused) {
		t.Fatalf("422 not reported as a refusal: %v", err)
	}
}

func asRefused(err error, target **RefusedError) bool {
	r, ok := err.(*RefusedError)
	*target = r
	return ok
}

func TestKeyMappings(t *testing.T) {
	for dom, wire := range map[string]string{"KeyW": "key-w", "Digit1": "digit-1", "ShiftLeft": "shift-left", "ArrowUp": "arrow-up", "Space": "space", "ControlLeft": "control-left"} {
		if got, err := keyCode(dom); err != nil || got != wire {
			t.Fatalf("%s: %q %v", dom, got, err)
		}
	}
	if _, err := keyCode("F5"); err == nil {
		t.Fatal("unmapped key accepted")
	}
	for vk, wire := range map[int]string{69: "key-e", 49: "digit-1", 0x20: "space", 0xA2: "control-left", 0x26: "arrow-up"} {
		if got, err := virtualKey(vk); err != nil || got != wire {
			t.Fatalf("vk %d: %q %v", vk, got, err)
		}
	}
	if f, err := controlFact("Primary"); err != nil || f["button"] != "primary" {
		t.Fatalf("pointer control: %v %v", f, err)
	}
}

func TestParseFrame(t *testing.T) {
	frame := make([]byte, 48+3)
	copy(frame, "RSF1")
	le := binary.LittleEndian
	le.PutUint32(frame[4:], 48) // header extended by 8 unknown bytes
	le.PutUint64(frame[8:], 5)
	le.PutUint64(frame[16:], 42)
	le.PutUint32(frame[24:], 1280)
	le.PutUint32(frame[28:], 720)
	frame[32], frame[33] = 3, 1
	le.PutUint32(frame[36:], 3)
	copy(frame[48:], []byte{7, 8, 9})
	parsed, err := ParseFrame(frame)
	if err != nil || parsed.Step != 42 || parsed.Format != "png" || !parsed.Held || string(parsed.Payload) != "\x07\x08\x09" {
		t.Fatalf("%+v %v", parsed, err)
	}
	if _, err := ParseFrame(frame[:50]); err == nil {
		t.Fatal("truncated frame accepted")
	}
}

func ptr[T any](v T) *T { return &v }

func TestRawClicksUseTheBatchButtonNumbers(t *testing.T) {
	fake, host := startFake(t)
	c := &claim{host: host, label: "crew-test"}
	defer c.release(context.Background())
	if _, err := c.steps(context.Background(), []map[string]any{{"kind": "click", "button": 3, "ms": 0}}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if button := fake.events[0]["fact"].(map[string]any)["button"]; button != "secondary" {
		t.Fatalf("button 3 sent as %v", button)
	}
}
