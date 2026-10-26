//go:build unix

package reviewhook

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out hook's children must not outlive it, and the next queued hook
// must start only after they are gone.
func TestTimeoutKillsTheHooksChildrenBeforeTheNextHook(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "deploy")
	body := "#!/bin/sh\n" +
		"sleep 120 &\n" +
		"echo $! > " + dir + "/child-$CREW_REVIEW_ROUND.pid\n" +
		"for f in " + dir + "/child-*.pid; do pid=$(cat \"$f\"); if [ \"$f\" != " + dir + "/child-$CREW_REVIEW_ROUND.pid ] && kill -0 \"$pid\" 2>/dev/null; then echo overlap > " + dir + "/overlap; fi; done\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	notes := &fakeNotes{}
	runner, err := New(Config{
		Commands: map[string]string{"FuzzySlipper/den-services": script},
		Timeout:  500 * time.Millisecond,
		LogDir:   filepath.Join(dir, "logs"),
		Notes:    notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := finalizedJob("looks_good", "FuzzySlipper/den-services")
	second := finalizedJob("looks_good", "FuzzySlipper/den-services")
	second.Admission.Key.ReviewRoundID++

	runner.Finalized(first)
	runner.Finalized(second)
	runner.Wait()

	if _, err := os.Stat(filepath.Join(dir, "overlap")); err == nil {
		t.Fatal("the second hook started while the first hook's child was still running")
	}
	for _, round := range []int64{first.Admission.Key.ReviewRoundID, second.Admission.Key.ReviewRoundID} {
		raw, err := os.ReadFile(filepath.Join(dir, "child-"+strconv.FormatInt(round, 10)+".pid"))
		if err != nil {
			t.Fatalf("round %d hook did not run: %v", round, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if !gone(pid, 5*time.Second) {
			t.Fatalf("round %d child %d survived its hook's timeout", round, pid)
		}
	}
	if len(notes.notes) != 2 || !strings.Contains(notes.notes[0].content, "failed after") {
		t.Fatalf("notes = %#v", notes.notes)
	}
}

// gone waits for pid to disappear; an orphaned child is reaped by init shortly
// after it is killed.
func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
