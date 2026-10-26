package reviewhook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/review"
)

type recordedNote struct {
	key     review.TaskKey
	content string
}

type fakeNotes struct {
	mu    sync.Mutex
	notes []recordedNote
}

func (f *fakeNotes) PostTaskNote(_ context.Context, key review.TaskKey, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, recordedNote{key: key, content: content})
	return nil
}

type fakeExec struct {
	mu     sync.Mutex
	calls  []string
	env    []string
	output string
	err    error
}

func (f *fakeExec) run(_ context.Context, path string, env []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	f.env = env
	return []byte(f.output), f.err
}

func executable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func finalizedJob(verdict, repository string) review.Job {
	return review.Job{
		Admission: review.Admission{
			Key:    review.Key{ProjectID: "den-services", TaskID: 10391, ReviewRoundID: 6958, CorrelationID: "c"},
			Source: &review.ReviewSource{Repository: repository, CommitSHA: "79c43163b43fb957caaf3ce41aa96d4bf946f1d4", Ref: "main"},
		},
		Receipt: &review.Receipt{Verdict: verdict},
	}
}

func newRunner(t *testing.T, path string, exec *fakeExec, notes *fakeNotes) (*Runner, string) {
	t.Helper()
	logDir := t.TempDir()
	runner, err := New(Config{
		Commands: map[string]string{"FuzzySlipper/den-services": path},
		Timeout:  time.Minute,
		LogDir:   logDir,
		Notes:    notes,
		Exec:     exec.run,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return runner, logDir
}

func TestLooksGoodRunsRepositoryHookAndPostsNote(t *testing.T) {
	path := executable(t)
	exec := &fakeExec{output: "deploy: 79c43163b43f\ndeployed: 2\n"}
	notes := &fakeNotes{}
	runner, logDir := newRunner(t, path, exec, notes)

	runner.Finalized(finalizedJob("looks_good", "fuzzyslipper/den-services"))
	runner.Wait()

	if len(exec.calls) != 1 || exec.calls[0] != path {
		t.Fatalf("calls = %v, want one call of %s", exec.calls, path)
	}
	for _, want := range []string{
		"CREW_REVIEW_REPOSITORY=fuzzyslipper/den-services",
		"CREW_REVIEW_COMMIT=79c43163b43fb957caaf3ce41aa96d4bf946f1d4",
		"CREW_REVIEW_PROJECT=den-services",
		"CREW_REVIEW_TASK=10391",
		"CREW_REVIEW_ROUND=6958",
	} {
		if !containsString(exec.env, want) {
			t.Fatalf("hook environment lacks %s", want)
		}
	}
	logged, err := os.ReadFile(filepath.Join(logDir, "den-services-10391-round-6958.log"))
	if err != nil || string(logged) != exec.output {
		t.Fatalf("hook log = %q, %v", logged, err)
	}
	if len(notes.notes) != 1 || notes.notes[0].key != (review.TaskKey{ProjectID: "den-services", TaskID: 10391}) {
		t.Fatalf("notes = %#v", notes.notes)
	}
	content := notes.notes[0].content
	if !strings.Contains(content, "succeeded") || !strings.Contains(content, "deployed: 2") || !strings.Contains(content, "79c43163b43f") {
		t.Fatalf("note = %q", content)
	}
}

func TestHookFailureIsReportedOnTheTask(t *testing.T) {
	exec := &fakeExec{output: "deploy: host step failed (exit 1)\n", err: errors.New("exit status 1")}
	notes := &fakeNotes{}
	runner, _ := newRunner(t, executable(t), exec, notes)

	runner.Finalized(finalizedJob("looks_good", "FuzzySlipper/den-services"))
	runner.Wait()

	if len(notes.notes) != 1 || !strings.Contains(notes.notes[0].content, "failed after") || !strings.Contains(notes.notes[0].content, "exit status 1") {
		t.Fatalf("notes = %#v", notes.notes)
	}
}

func TestHookIgnoresOtherVerdictsRepositoriesAndManualReviews(t *testing.T) {
	exec := &fakeExec{}
	notes := &fakeNotes{}
	runner, _ := newRunner(t, executable(t), exec, notes)

	manual := finalizedJob("looks_good", "FuzzySlipper/den-services")
	manual.Admission.Source = nil
	unreceipted := finalizedJob("looks_good", "FuzzySlipper/den-services")
	unreceipted.Receipt = nil
	for _, job := range []review.Job{
		finalizedJob("changes_requested", "FuzzySlipper/den-services"),
		finalizedJob("looks_good", "FuzzySlipper/crew-services"),
		manual,
		unreceipted,
	} {
		runner.Finalized(job)
	}
	runner.Wait()

	if len(exec.calls) != 0 || len(notes.notes) != 0 {
		t.Fatalf("calls = %v, notes = %#v; want none", exec.calls, notes.notes)
	}
}

func TestNewRejectsCommandsThatCannotRun(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "deploy")
	if err := os.WriteFile(notExecutable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"scripts/deploy.sh", notExecutable, filepath.Join(t.TempDir(), "missing")} {
		_, err := New(Config{Commands: map[string]string{"owner/repo": path}, Timeout: time.Minute, LogDir: t.TempDir()})
		if err == nil {
			t.Fatalf("New() accepted hook %q", path)
		}
	}
}

func TestOutputTailKeepsTheEndAndCannotCloseTheFence(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line")
	}
	lines = append(lines, "```", "last")
	tail := outputTail([]byte(strings.Join(lines, "\n")))
	if strings.Count(tail, "\n")+1 != noteTailLines || !strings.HasSuffix(tail, "'''\nlast") {
		t.Fatalf("tail = %q", tail)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
