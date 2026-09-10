package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/playtest/evidence"
)

type fakeBackend struct {
	mu            sync.Mutex
	inputs        int
	held          bool
	cancelled     int
	block         bool
	entered       chan struct{}
	cancelErr     error
	cancelEntered chan struct{}
	cancelBlock   chan struct{}
	releaseResult map[string]any
	releaseErr    error
}

func (f *fakeBackend) Acquire(context.Context, int, int, int, int) (map[string]any, error) {
	return map[string]any{"lease_id": newID()}, nil
}

func (f *fakeBackend) Observe(context.Context, string) (map[string]any, error) {
	return map[string]any{"path": "frame.png"}, nil
}

func (f *fakeBackend) Input(ctx context.Context, _ string, steps []map[string]any) (map[string]any, error) {
	f.mu.Lock()
	f.inputs++
	f.held = true
	block := f.block
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
	if block {
		<-ctx.Done()
	}
	f.mu.Lock()
	f.held = false
	f.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return map[string]any{"completed_steps": len(steps)}, nil
}

func (f *fakeBackend) Cancel(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	f.held = false
	f.cancelled++
	cancelled := f.cancelled
	cancelErr := f.cancelErr
	cancelEntered := f.cancelEntered
	cancelBlock := f.cancelBlock
	f.mu.Unlock()
	if cancelEntered != nil {
		select {
		case cancelEntered <- struct{}{}:
		default:
		}
	}
	if cancelBlock != nil && cancelled == 1 {
		<-cancelBlock
	}
	return map[string]any{"cancel_requested": true}, cancelErr
}

func (f *fakeBackend) Status(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (f *fakeBackend) Release(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseResult != nil || f.releaseErr != nil {
		return f.releaseResult, f.releaseErr
	}
	return map[string]any{"released": true}, nil
}

type fakeLauncher struct{}

func (fakeLauncher) Launch(context.Context, string, Profile) (map[string]any, error) {
	return map[string]any{"game_readiness": "requires observation"}, nil
}

type partialFinalJournal struct{ file *os.File }

func (journal *partialFinalJournal) Append(value any) error {
	record, _ := value.(map[string]any)
	if record["event"] != "program_done" {
		return evidence.AppendJSON(journal.file, value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 1 {
		data = data[:len(data)/2]
	}
	if _, err := journal.file.Write(data); err != nil {
		return err
	}
	if err := journal.file.Sync(); err != nil {
		return err
	}
	return io.ErrShortWrite
}

func (journal *partialFinalJournal) Close() error { return journal.file.Close() }

func testService(t *testing.T) (*Service, *fakeBackend, string) {
	t.Helper()
	f := &fakeBackend{}
	worker, err := filepath.Abs("../scriptworker/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(f, fakeLauncher{}, []Profile{{ID: "game"}}, t.TempDir(), worker)
	if err != nil {
		t.Fatal(err)
	}
	value, err := s.Start(context.Background(), "game", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	return s, f, value.(*Session).ID
}

func awaitScript(t *testing.T, s *Service, id string) Script {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := s.Script(id)
		if err != nil {
			t.Fatal(err)
		}
		st := value.(Script)
		if st.EndedAt != nil {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("script did not terminate")
	return Script{}
}

func TestProgramSequencesNativeInputsAndRetainsCheckpoint(t *testing.T) {
	s, f, id := testService(t)
	value, err := s.Run(id, `await controller.hold({ly:0.5},20); await keyboard.hold(["W","Shift"],20); checkpoint("after",await observe()); return {samples:2}`, 2000)
	if err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, value.(Script).ID)
	if st.Phase != "completed" || st.Checkpoint == nil {
		t.Fatalf("%+v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs != 2 || f.held {
		t.Fatalf("inputs=%d held=%v", f.inputs, f.held)
	}
}

func TestInfiniteWorkerTimesOutWithoutLosingSession(t *testing.T) {
	s, _, id := testService(t)
	value, err := s.Run(id, `while(true){}`, 150)
	if err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, value.(Script).ID)
	if st.Phase != "timed_out" {
		t.Fatalf("%+v", st)
	}
	if _, err = s.ManualInput(context.Background(), id, []map[string]any{{"kind": "wait", "ms": 1}}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelHeldActionWaitsForNeutral(t *testing.T) {
	s, f, id := testService(t)
	f.block = true
	f.entered = make(chan struct{}, 1)
	value, err := s.Run(id, `await controller.hold({rt:1},9000)`, 10000)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no input")
	}
	if _, err = s.ManualInput(context.Background(), id, nil); err == nil {
		t.Fatal("manual input admitted during script")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = s.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, value.(Script).ID)
	if st.Phase != "cancelled" {
		t.Fatalf("%+v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held || f.inputs != 1 || f.cancelled < 1 {
		t.Fatalf("held=%v inputs=%d cancels=%d", f.held, f.inputs, f.cancelled)
	}
}

func TestYieldResumesWithAgentData(t *testing.T) {
	s, _, id := testService(t)
	value, err := s.Run(id, `const next=await yieldToAgent("choose",await observe()); return next;`, 3000)
	if err != nil {
		t.Fatal(err)
	}
	scriptID := value.(Script).ID
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, _ := s.Script(scriptID)
		if st.(Script).Phase == "yielded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no yield")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = s.Resume(id, []byte(`{"direction":"left"}`)); err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, scriptID)
	if st.Phase != "completed" || st.Result.(map[string]any)["direction"] != "left" {
		t.Fatalf("%+v", st)
	}
}

func TestCancelMakesYieldedScriptNonResumable(t *testing.T) {
	s, f, id := testService(t)
	f.cancelEntered = make(chan struct{}, 2)
	f.cancelBlock = make(chan struct{})
	value, err := s.Run(id, `await yieldToAgent("choose", {ready:true})`, 3000)
	if err != nil {
		t.Fatal(err)
	}
	scriptID := value.(Script).ID
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, scriptErr := s.Script(scriptID)
		if scriptErr != nil {
			t.Fatal(scriptErr)
		}
		if st.(Script).Phase == "yielded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no yield")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelled := make(chan error, 1)
	go func() {
		_, cancelErr := s.Cancel(context.Background(), id)
		cancelled <- cancelErr
	}()
	select {
	case <-f.cancelEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not reach backend")
	}
	if _, err := s.Resume(id, []byte(`{"direction":"left"}`)); err == nil {
		t.Fatal("resume was accepted after cancellation began")
	}
	close(f.cancelBlock)
	select {
	case err := <-cancelled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	st := awaitScript(t, s, scriptID)
	if st.Phase != "cancelled" {
		t.Fatalf("%+v", st)
	}
}

func TestTimeoutPreservesCleanupUncertainty(t *testing.T) {
	s, f, id := testService(t)
	f.cancelErr = errors.New("neutralization unavailable")
	value, err := s.Run(id, `while(true){}`, 150)
	if err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, value.(Script).ID)
	if st.Phase != "cleanup_uncertain" {
		t.Fatalf("%+v", st)
	}
	if st.Error == "" || !strings.Contains(st.Error, "neutralization unavailable") {
		t.Fatalf("cleanup error was hidden: %+v", st)
	}
}

func TestStopSeparatesCompletedCleanupFromEvidenceFailure(t *testing.T) {
	s, f, id := testService(t)
	f.mu.Lock()
	f.releaseResult = map[string]any{"released": true, "evidence_error": "persist local release receipt: disk full"}
	f.releaseErr = errors.New("persist local release receipt: disk full")
	f.mu.Unlock()
	if _, err := s.Stop(context.Background(), id); err == nil {
		t.Fatal("evidence persistence failure was hidden")
	}
	if s.sessions[id].Phase != "stopped" || s.current != "" {
		t.Fatalf("completed cleanup was misclassified: %+v current=%q", s.sessions[id], s.current)
	}
	if !strings.Contains(s.sessions[id].LastError, "cleanup completed; evidence incomplete") {
		t.Fatalf("missing evidence diagnostic: %+v", s.sessions[id])
	}
}

func TestLargeJournalRetainsEveryDurableRecord(t *testing.T) {
	s, _, id := testService(t)
	value, err := s.Run(id, `for (let i = 0; i < 500; i++) console.log("sample-" + i + ":" + "x".repeat(512)); return {samples:500}`, 10000)
	if err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, value.(Script).ID)
	if st.Phase != "completed" {
		t.Fatalf("large journal script = %+v", st)
	}
	info, err := os.Stat(st.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 256*1024 {
		t.Fatalf("journal is too small for the exercised history: %d bytes", info.Size())
	}
	file, err := os.Open(st.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	entries := 0
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("journal entry %d is invalid: %v", entries, err)
		}
		entries++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if entries != 1002 { // start + 500 calls + 500 results + terminal record
		t.Fatalf("journal entries = %d, want 1002", entries)
	}
}

func TestPartialTerminalJournalFailsWithoutDiscardingPriorEvidence(t *testing.T) {
	s, f, id := testService(t)
	originalOpen := openScriptJournal
	t.Cleanup(func() { openScriptJournal = originalOpen })
	openScriptJournal = func(path string) (scriptJournal, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return &partialFinalJournal{file: file}, nil
	}
	value, err := s.Run(id, `const choice = await yieldToAgent("continue", {ready:true}); return choice`, 3000)
	if err != nil {
		t.Fatal(err)
	}
	scriptID := value.(Script).ID
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, scriptErr := s.Script(scriptID)
		if scriptErr != nil {
			t.Fatal(scriptErr)
		}
		if current.(Script).Phase == "yielded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("script did not yield")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := s.Resume(id, []byte(`{"choice":"left"}`)); err != nil {
		t.Fatal(err)
	}
	st := awaitScript(t, s, scriptID)
	if st.Phase != "failed" || !strings.Contains(st.Error, "persist final journal record") {
		t.Fatalf("terminal journal failure was hidden: %+v", st)
	}
	data, err := os.ReadFile(st.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || lines[len(lines)-1] == "" {
		t.Fatalf("expected an explicit incomplete terminal tail, got %q", data)
	}
	for index, line := range lines[:len(lines)-1] {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("durable record %d was damaged: %v", index, err)
		}
	}
	if _, err := os.ReadFile(st.SourcePath); err != nil {
		t.Fatalf("original script was discarded: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(filepath.Dir(st.SourcePath), "script.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recovered Script
	if err := json.Unmarshal(saved, &recovered); err != nil {
		t.Fatalf("final script state is not parseable: %v", err)
	}
	if recovered.Phase != "failed" || !strings.Contains(recovered.Error, "persist final journal record") {
		t.Fatalf("final state claimed complete evidence: %+v", recovered)
	}
	f.mu.Lock()
	cancelled := f.cancelled
	f.mu.Unlock()
	if cancelled == 0 {
		t.Fatal("finalization failure skipped owned input cleanup")
	}
}

func TestStopReportsSessionStatePersistenceFailureAfterCleanup(t *testing.T) {
	s, _, id := testService(t)
	path := filepath.Join(s.stateDir, "session-"+id+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	value, err := s.Stop(context.Background(), id)
	if err == nil {
		t.Fatal("session persistence failure was hidden")
	}
	result := value.(map[string]any)
	if result["released"] != true {
		t.Fatalf("cleanup success was lost: %#v", result)
	}
	evidenceErr, _ := result["evidence_error"].(string)
	if !strings.Contains(evidenceErr, "persist session state") {
		t.Fatalf("result did not identify incomplete evidence: %#v", result)
	}
	if s.sessions[id].Phase != "stopped" || !strings.Contains(s.sessions[id].LastError, "cleanup completed; evidence incomplete") {
		t.Fatalf("session cleanup/evidence state = %+v", s.sessions[id])
	}
}

func TestRestartDoesNotReplaySavedSession(t *testing.T) {
	s, f, id := testService(t)
	other, err := New(f, fakeLauncher{}, s.profiles, s.stateDir, s.worker)
	if err != nil {
		t.Fatal(err)
	}
	if other.sessions[id].Phase != "interrupted" {
		t.Fatal("saved session silently resumed")
	}
	if _, err = other.ManualInput(context.Background(), id, nil); err == nil {
		t.Fatal("interrupted session accepted input")
	}
	if _, err = other.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if f.inputs != 0 {
		t.Fatal("replayed input")
	}
}

func TestInvalidKeyRejectedBeforeDelivery(t *testing.T) {
	if _, err := KeyCodes([]string{"unknown"}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := KeyCodes(nil); err == nil {
		t.Fatal("empty hold accepted")
	}
}

func TestInvalidRawInputDoesNotDegradeSession(t *testing.T) {
	s, f, id := testService(t)
	_, err := s.ManualInput(context.Background(), id, []map[string]any{{"kind": "hold", "keys": []string{"W"}, "ms": 50}})
	if err == nil {
		t.Fatal("invalid native key accepted")
	}
	if s.sessions[id].Phase != "connected" || f.inputs != 0 {
		t.Fatal("validation changed the game session")
	}
}
