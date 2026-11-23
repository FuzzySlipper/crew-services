// Package agydriver delivers crew fabric messages into Antigravity CLI (agy)
// conversations through each agy process's own language server. It never
// starts, stops or interrupts an agy process: it posts a message the
// conversation reads itself, and hands messages to a running turn through the
// conversation's own PreInvocation hook.
package agydriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Every agy process runs its own language server on two random loopback
// ports, one HTTPS and one plain HTTP. The HTTP port speaks gRPC (h2c) and
// Connect JSON, and every call needs the server's CSRF token in the
// x-antigravity-csrf-token header. agy passes the address and token only to
// the commands its agent runs (ANTIGRAVITY_LS_ADDRESS, ANTIGRAVITY_CSRF_TOKEN),
// not to hooks or MCP servers, so the driver finds the port from the process's
// listening sockets and knows the token only because agy was started with
// ANTIGRAVITY_SERVER_CSRF_TOKEN set to the value in the driver's token file.
//
// Two calls are used, both read from agy 1.5.15:
//
//   - exa.language_server_pb.LanguageServerService/SendAgentMessage
//     {"recipient": <conversation id>, "content": <text>} is what `agy
//     agentapi send-message` (the swarm's agent-to-agent message) sends. It
//     writes the message to brain/<id>/.system_generated/messages/<uuid>.json
//     and answers {}. An idle conversation starts a turn with it; a running
//     turn reads it at its next step boundary. Either way it enters the
//     conversation as a SYSTEM_MESSAGE step "[Message] … content=<text>". It
//     answers {} for any recipient, even one that does not exist, so the
//     driver checks the conversation first.
//   - jetski.product.v1.ConversationService/GetConversation
//     {"conversationId": …} returns state.runState: RUN_STATE_IDLE,
//     RUN_STATE_RUNNING or RUN_STATE_CANCELLING.

const (
	sendAgentMessagePath = "/exa.language_server_pb.LanguageServerService/SendAgentMessage"
	getConversationPath  = "/jetski.product.v1.ConversationService/GetConversation"
)

// Run states GetConversation reports.
const (
	RunStateIdle       = "RUN_STATE_IDLE"
	RunStateRunning    = "RUN_STATE_RUNNING"
	RunStateCancelling = "RUN_STATE_CANCELLING"
)

// ErrGone means the language server did not take the request: nothing
// listens on the address, so the process has exited or is starting.
var ErrGone = errors.New("agy language server is not accepting connections")

// RefusedError is an answer from the language server refusing a call, with
// its Connect error code (for example "unauthenticated" when agy was not
// started with the driver's token). Nothing was inserted.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("agy language server refused the call: %s: %s", e.Code, e.Message)
}

// Native is the part of an agy language server the driver uses.
type Native interface {
	// Conversation reads a conversation's run state and parent.
	Conversation(ctx context.Context, addr, token, conversationID string) (ConversationState, error)
	// Send posts content to a conversation. An error other than ErrGone or a
	// RefusedError leaves it unknown whether the message was taken.
	Send(ctx context.Context, addr, token, conversationID, content string) error
}

// LanguageServer speaks Connect JSON to agy's plain HTTP port.
type LanguageServer struct {
	Client *http.Client
}

func (l LanguageServer) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// ConversationState is what GetConversation says about a conversation.
type ConversationState struct {
	RunState string
	// Parent is set for a conversation another one started (a subagent).
	Parent string
}

// Running reports whether a turn is in progress.
func (c ConversationState) Running() bool {
	return c.RunState == RunStateRunning || c.RunState == RunStateCancelling
}

type conversationJSON struct {
	ID       string `json:"id"`
	Metadata struct {
		ParentConversationID string `json:"parentConversationId"`
	} `json:"metadata"`
	State struct {
		RunState string `json:"runState"`
	} `json:"state"`
}

func (l LanguageServer) Conversation(ctx context.Context, addr, token, conversationID string) (ConversationState, error) {
	var out struct {
		conversationJSON
		Conversation *conversationJSON `json:"conversation"`
	}
	if err := l.call(ctx, addr, token, getConversationPath, map[string]string{"conversationId": conversationID}, &out); err != nil {
		return ConversationState{}, err
	}
	// The answer is the Conversation itself; accept a wrapped one too.
	c := out.conversationJSON
	if out.Conversation != nil {
		c = *out.Conversation
	}
	state := ConversationState{RunState: c.State.RunState, Parent: c.Metadata.ParentConversationID}
	if state.RunState == "" {
		state.RunState = RunStateIdle
	}
	return state, nil
}

func (l LanguageServer) Send(ctx context.Context, addr, token, conversationID, content string) error {
	return l.call(ctx, addr, token, sendAgentMessagePath, map[string]string{"recipient": conversationID, "content": content}, nil)
}

func (l LanguageServer) call(ctx context.Context, addr, token, path string, body, target any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-antigravity-csrf-token", token)
	resp, err := l.client().Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: %v", ErrGone, err)
		}
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var connectErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &connectErr) == nil && connectErr.Code != "" {
			return &RefusedError{Code: connectErr.Code, Message: connectErr.Message}
		}
		// The HTTPS port answers plain HTTP with 400 and text.
		return &RefusedError{Code: "http_" + strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(raw))}
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// Proc identifies one agy process. Start is its start time in clock ticks
// since boot (/proc/<pid>/stat field 22), so a reused pid is not mistaken for
// the process that registered.
type Proc struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

// Procs reads agy processes from a /proc tree.
type Procs struct {
	Root string // "/proc" when empty
}

func (p Procs) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// stat returns a process's command name, parent pid and start time.
func (p Procs) stat(pid int) (comm string, ppid int, start int64, err error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, 0, err
	}
	return parseStat(string(data))
}

// parseStat reads /proc/<pid>/stat: "pid (comm) state ppid … starttime …".
// The command name may hold spaces and parentheses, so it ends at the last ')'.
func parseStat(line string) (comm string, ppid int, start int64, err error) {
	open := strings.IndexByte(line, '(')
	end := strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return "", 0, 0, errors.New("malformed stat")
	}
	comm = line[open+1 : end]
	fields := strings.Fields(line[end+1:])
	// fields[0] is state (field 3); ppid is field 4, starttime field 22.
	if len(fields) < 20 {
		return "", 0, 0, errors.New("short stat")
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, err
	}
	start, err = strconv.ParseInt(fields[19], 10, 64)
	return comm, ppid, start, err
}

// FindAgy walks up from pid (a hook's parent) to the agy process that runs
// the hook. Hooks run under `sh -c`, so agy is usually one or two levels up.
func (p Procs) FindAgy(pid int) (Proc, bool) {
	for i := 0; i < 6 && pid > 1; i++ {
		comm, ppid, start, err := p.stat(pid)
		if err != nil {
			return Proc{}, false
		}
		if comm == "agy" {
			return Proc{PID: pid, Start: start}, true
		}
		pid = ppid
	}
	return Proc{}, false
}

// Alive reports whether proc is still the same running process.
func (p Procs) Alive(proc Proc) bool {
	if proc.PID <= 0 {
		return false
	}
	comm, _, start, err := p.stat(proc.PID)
	return err == nil && comm == "agy" && start == proc.Start
}

// ListenPorts lists the loopback TCP ports proc listens on.
func (p Procs) ListenPorts(proc Proc) ([]int, error) {
	fds, err := os.ReadDir(filepath.Join(p.root(), strconv.Itoa(proc.PID), "fd"))
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(p.root(), strconv.Itoa(proc.PID), "fd", fd.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	var ports []int
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(proc.PID), "net", table))
		if err != nil {
			continue
		}
		ports = append(ports, listeningPorts(string(data), inodes)...)
	}
	return ports, nil
}

// listeningPorts reads a /proc/net/tcp table for loopback sockets in LISTEN
// state whose inode is in inodes.
func listeningPorts(table string, inodes map[string]bool) []int {
	var ports []int
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "0A" || !inodes[f[9]] {
			continue
		}
		host, portHex, ok := strings.Cut(f[1], ":")
		if !ok || !loopbackHex(host) {
			continue
		}
		port, err := strconv.ParseInt(portHex, 16, 32)
		if err == nil {
			ports = append(ports, int(port))
		}
	}
	return ports
}

// loopbackHex reports whether a /proc/net/tcp address is 127.0.0.1 or ::1
// (or ::ffff:127.0.0.1). IPv4 is little-endian hex: 0100007F.
func loopbackHex(host string) bool {
	switch host {
	case "0100007F", "00000000000000000000000001000000", "0000000000000000FFFF00000100007F":
		return true
	}
	return false
}

// ConversationArg returns the conversation an agy command line resumes with
// --conversation, or "".
func ConversationArg(args []string) string {
	for i, a := range args {
		switch {
		case (a == "--conversation" || a == "-conversation") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--conversation="):
			return strings.TrimPrefix(a, "--conversation=")
		case strings.HasPrefix(a, "-conversation="):
			return strings.TrimPrefix(a, "-conversation=")
		}
	}
	return ""
}

// Resuming lists running agy processes started with --conversation, by the
// conversation each resumes. A resumed conversation runs no SessionStart
// hook, so this is how the driver finds it before its first turn.
func (p Procs) Resuming() map[string]Proc {
	out := map[string]Proc{}
	entries, err := os.ReadDir(p.root())
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, _, start, err := p.stat(pid)
		if err != nil || comm != "agy" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.root(), e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(args) > 1 {
			if id := ConversationArg(args[1:]); id != "" {
				out[id] = Proc{PID: pid, Start: start}
			}
		}
	}
	return out
}
