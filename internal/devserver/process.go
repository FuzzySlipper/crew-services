package devserver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ManagedProcess struct {
	Command *exec.Cmd
	PID     int
}

func startManagedProcess(command string, workDir string, env map[string]string, stdoutPath string, stderrPath string) (*ManagedProcess, error) {
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return nil, fmt.Errorf("creating server stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return nil, fmt.Errorf("creating server stderr log: %w", err)
	}
	defer stderr.Close()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(os.Environ(), env)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting dev server command: %w", err)
	}
	// Reap the launcher when it exits. A short-lived CLI exits first anyway;
	// a long-running owner (the playtest service) would otherwise keep zombies,
	// which liveness checks treat as dead but which never leave the table.
	go func() { _ = cmd.Wait() }()
	return &ManagedProcess{Command: cmd, PID: cmd.Process.Pid}, nil
}

func StopProcessGroup(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	if !processGroupAlive(pid) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing process group %d: %w", pid, err)
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !processZombie(pid)
}

func processZombie(pid int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	closing := strings.LastIndex(string(data), ")")
	if closing < 0 {
		return false
	}
	fields := strings.Fields(string(data)[closing+1:])
	return len(fields) > 0 && fields[0] == "Z"
}

// processGroupAlive reports whether a broker-owned process group still has a
// member. A shell or npm launcher can exit after it starts a long-lived Node
// host, while that host remains in the launcher's process group. Broker
// sessions are therefore owned by the group ID established at start, not just
// the original launcher PID.
func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	return linuxProcessGroupAlive(pgid)
}

func linuxProcessGroupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		state, groupID, ok := linuxProcessStateAndGroup(pid)
		if ok && state != "Z" && groupID == pgid {
			return true
		}
	}
	return false
}

func linuxProcessStateAndGroup(pid int) (string, int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, false
	}
	closing := strings.LastIndex(string(data), ")")
	if closing < 0 {
		return "", 0, false
	}
	fields := strings.Fields(string(data)[closing+1:])
	if len(fields) < 3 {
		return "", 0, false
	}
	groupID, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], groupID, true
}

func mergeEnv(base []string, values map[string]string) []string {
	merged := append([]string(nil), base...)
	for key, value := range values {
		merged = append(merged, key+"="+value)
	}
	return merged
}
