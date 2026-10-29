package codexadapter

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NativeInteraction is an ephemeral server-to-client request. It is valid
// only for this App Server child and is deliberately never projected as a
// durable recoverable fact.
type NativeInteraction struct {
	ID        string          `json:"id"`
	Method    string          `json:"method"`
	ThreadID  string          `json:"thread_id,omitempty"`
	TurnID    string          `json:"turn_id,omitempty"`
	Params    json.RawMessage `json:"params"`
	CreatedAt time.Time       `json:"created_at"`
}

// NativeThread is the thread metadata this package reads from Codex.
type NativeThread struct {
	ID     string
	Name   string
	CWD    string
	Status string
	// Ephemeral threads are never written to disk and cannot take queued input.
	Ephemeral bool
	// AcceptsInput is false for threads such as subagents that refuse direct
	// user input.
	AcceptsInput bool
}

// QueuedSubmission is Codex's receipt for one queued turn. The app-server
// keeps its queue on disk and starts queued items itself once the thread is
// idle; ClientUserMessageID is the caller's own identity for the item.
type QueuedSubmission struct {
	ID                  string
	ClientUserMessageID string
}

// StartedTurn is the native acceptance receipt for an ephemeral review turn.
type StartedTurn struct{ ID string }

type EphemeralThreadOptions struct {
	CWD                   string
	DeveloperInstructions string
	DynamicTools          []map[string]any
	ReadOnly              bool
}
type EphemeralTurnOptions struct {
	Model  string
	Effort string
}
type TurnCompletion struct {
	ThreadID string
	TurnID   string
	Status   string
	// Error is Codex's message for a failed or interrupted turn.
	Error string
	// LastMessage is the turn's final agent message, kept for diagnostics.
	LastMessage string
}
type EphemeralServer interface {
	Initialize(context.Context) error
	StartEphemeralThread(context.Context, EphemeralThreadOptions) (NativeThread, error)
	StartEphemeralTurn(context.Context, string, string, EphemeralTurnOptions) (StartedTurn, error)
	WaitTurn(context.Context, string, string) (TurnCompletion, error)
	ForgetThread(string)
	Interactions() []NativeInteraction
	Interrupt(context.Context, string, string) error
	SetDynamicToolHandler(func(context.Context, json.RawMessage) (json.RawMessage, error))
	Close() error
}

// StdioAppServer is one JSON-RPC 2.0 connection to a Codex app-server: either
// a child it started over stdio (crew-review's ephemeral workers) or the
// shared daemon's control socket (DialDaemon), which it never supervises.
type StdioAppServer struct {
	command *exec.Cmd
	// stdin writes one JSON-RPC message per Write call.
	stdin io.WriteCloser
	// notify, when set, sees every server notification. It runs on the read
	// loop and must not block.
	notify func(method string, params json.RawMessage)

	mu               sync.Mutex
	writeMu          sync.Mutex
	responseMu       sync.Mutex
	nextID           int64
	pending          map[string]chan rpcResponse
	interactions     map[string]pendingInteraction
	interactionSeq   uint64
	instancePrefix   string
	dynamicHandler   func(context.Context, json.RawMessage) (json.RawMessage, error)
	toolContext      context.Context
	toolCancel       context.CancelFunc
	turnWaiters      map[string]chan TurnCompletion
	completedTurns   map[string]TurnCompletion
	agentMessages    map[string]string
	ephemeralThreads map[string]struct{}
	closing          bool
	closed           bool
	done             chan struct{}
	err              error

	handshakeDone     chan struct{}
	handshakeComplete bool
	handshakeStarted  bool
	handshakeErr      error
}

// SetDynamicToolHandler installs adapter-owned handling for tools advertised
// only on newly created threads. Existing projected threads never call it.
func (c *StdioAppServer) SetDynamicToolHandler(handler func(context.Context, json.RawMessage) (json.RawMessage, error)) {
	c.mu.Lock()
	c.dynamicHandler = handler
	c.mu.Unlock()
}

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

type pendingInteraction struct {
	id     json.RawMessage
	method string
	value  NativeInteraction
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

// StartStdioAppServer starts a managed current Codex App Server child.
func StartStdioAppServer(command string, args []string) (*StdioAppServer, error) {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex App Server: %w", err)
	}
	prefix := make([]byte, 12)
	if _, err := rand.Read(prefix); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("random interaction prefix: %w", err)
	}
	client := newAppServerClient(stdin, hex.EncodeToString(prefix))
	client.command = cmd
	go client.read(stdout)
	go client.wait()
	return client, nil
}

func newAppServerClient(writer io.WriteCloser, instancePrefix string) *StdioAppServer {
	toolContext, toolCancel := context.WithCancel(context.Background())
	return &StdioAppServer{stdin: writer, pending: make(map[string]chan rpcResponse), interactions: make(map[string]pendingInteraction), turnWaiters: make(map[string]chan TurnCompletion), completedTurns: make(map[string]TurnCompletion), ephemeralThreads: make(map[string]struct{}), done: make(chan struct{}), handshakeDone: make(chan struct{}), instancePrefix: instancePrefix, toolContext: toolContext, toolCancel: toolCancel}
}

// SetNotificationHandler installs a non-blocking observer for server
// notifications.
func (c *StdioAppServer) SetNotificationHandler(handler func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.notify = handler
	c.mu.Unlock()
}

func (c *StdioAppServer) StartEphemeralThread(ctx context.Context, options EphemeralThreadOptions) (NativeThread, error) {
	var response struct {
		Thread nativeThreadWire `json:"thread"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return NativeThread{}, err
	}
	params := map[string]any{"ephemeral": true, "dynamicTools": options.DynamicTools}
	if options.CWD != "" {
		params["cwd"] = options.CWD
	}
	if options.DeveloperInstructions != "" {
		params["developerInstructions"] = options.DeveloperInstructions
	}
	if options.ReadOnly {
		params["sandbox"] = "read-only"
		params["approvalPolicy"] = "never"
	}
	if err := c.sendRequest(ctx, "thread/start", params, &response); err != nil {
		return NativeThread{}, err
	}
	thread := response.Thread.native()
	c.mu.Lock()
	c.ephemeralThreads[thread.ID] = struct{}{}
	c.mu.Unlock()
	return thread, nil
}
func (c *StdioAppServer) StartEphemeralTurn(ctx context.Context, threadID, text string, options EphemeralTurnOptions) (StartedTurn, error) {
	return c.startTurn(ctx, threadID, text, "crew-review:"+threadID+":"+strconv.FormatInt(time.Now().UnixNano(), 10), options)
}
func turnKey(threadID, turnID string) string { return threadID + "\x00" + turnID }
func (c *StdioAppServer) WaitTurn(ctx context.Context, threadID, turnID string) (TurnCompletion, error) {
	key := turnKey(threadID, turnID)
	c.mu.Lock()
	if _, tracked := c.ephemeralThreads[threadID]; !tracked {
		c.mu.Unlock()
		return TurnCompletion{}, errors.New("Codex ephemeral thread is no longer tracked")
	}
	if done, ok := c.completedTurns[key]; ok {
		delete(c.completedTurns, key)
		c.mu.Unlock()
		return done, nil
	}
	if c.closed {
		err := c.terminalErrorLocked()
		c.mu.Unlock()
		return TurnCompletion{}, err
	}
	wait := make(chan TurnCompletion, 1)
	c.turnWaiters[key] = wait
	c.mu.Unlock()
	select {
	case done, ok := <-wait:
		if !ok {
			return TurnCompletion{}, c.terminalError()
		}
		return done, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.turnWaiters, key)
		c.mu.Unlock()
		return TurnCompletion{}, ctx.Err()
	case <-c.done:
		return TurnCompletion{}, c.terminalError()
	}
}
func (c *StdioAppServer) ForgetThread(threadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ephemeralThreads, threadID)
	for key := range c.turnWaiters {
		if strings.HasPrefix(key, threadID+"\x00") {
			delete(c.turnWaiters, key)
		}
	}
	for key := range c.completedTurns {
		if strings.HasPrefix(key, threadID+"\x00") {
			delete(c.completedTurns, key)
		}
	}
	for key := range c.agentMessages {
		if strings.HasPrefix(key, threadID+"\x00") {
			delete(c.agentMessages, key)
		}
	}
}

// Interrupt requests cancellation of one exact active native turn.
func (c *StdioAppServer) Interrupt(ctx context.Context, threadID, turnID string) error {
	if err := c.awaitInitialized(ctx); err != nil {
		return err
	}
	var ignored map[string]any
	return c.sendRequest(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, &ignored)
}

// Interactions returns a snapshot of unresolved server requests for this child.
func (c *StdioAppServer) Interactions() []NativeInteraction {
	c.mu.Lock()
	defer c.mu.Unlock()
	values := make([]NativeInteraction, 0, len(c.interactions))
	for _, value := range c.interactions {
		values = append(values, value.value)
	}
	return values
}

func (c *StdioAppServer) Initialize(ctx context.Context) error {
	c.mu.Lock()
	if c.handshakeComplete {
		err := c.handshakeErr
		c.mu.Unlock()
		return err
	}
	if c.handshakeStarted {
		done := c.handshakeDone
		c.mu.Unlock()
		return c.waitHandshake(ctx, done)
	}
	if c.closed || c.closing {
		err := c.terminalErrorLocked()
		c.mu.Unlock()
		return err
	}
	c.handshakeStarted = true
	c.mu.Unlock()

	var ignored map[string]any
	err := c.sendRequest(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "crew-codex", "title": "Crew Codex Adapter", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true, "requestAttestation": false},
	}, &ignored)
	if err == nil {
		err = c.notifyInitialized()
	}
	c.completeHandshake(err)
	return err
}

func (c *StdioAppServer) startTurn(ctx context.Context, threadID, text, clientUserMessageID string, options EphemeralTurnOptions) (StartedTurn, error) {
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return StartedTurn{}, err
	}
	params := map[string]any{
		"threadId": threadID, "input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}, "clientUserMessageId": clientUserMessageID,
	}
	if options.Model != "" {
		params["model"] = options.Model
	}
	if options.Effort != "" {
		params["effort"] = options.Effort
	}
	if err := c.sendRequest(ctx, "turn/start", params, &response); err != nil {
		return StartedTurn{}, err
	}
	return StartedTurn{ID: response.Turn.ID}, nil
}

// QueueAdd persists a turn for automatic FIFO start once Codex considers the
// thread idle. It does not steer or interrupt an active turn.
func (c *StdioAppServer) QueueAdd(ctx context.Context, threadID, text, clientUserMessageID string) (QueuedSubmission, error) {
	var response struct {
		QueuedSubmission struct {
			ID                  string `json:"id"`
			ClientUserMessageID string `json:"clientUserMessageId"`
		} `json:"queuedSubmission"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return QueuedSubmission{}, err
	}
	if err := c.sendRequest(ctx, "thread/queue/add", map[string]any{
		"threadId": threadID, "input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}, "clientUserMessageId": clientUserMessageID,
	}, &response); err != nil {
		return QueuedSubmission{}, err
	}
	return QueuedSubmission{ID: response.QueuedSubmission.ID, ClientUserMessageID: response.QueuedSubmission.ClientUserMessageID}, nil
}

// QueueList rereads all durable queued submissions for one exact thread.
func (c *StdioAppServer) QueueList(ctx context.Context, threadID string) ([]QueuedSubmission, error) {
	if err := c.awaitInitialized(ctx); err != nil {
		return nil, err
	}
	var values []QueuedSubmission
	var cursor *string
	for {
		var response struct {
			Data []struct {
				ID                  string `json:"id"`
				ClientUserMessageID string `json:"clientUserMessageId"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": threadID, "limit": 100}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.sendRequest(ctx, "thread/queue/list", params, &response); err != nil {
			return nil, err
		}
		for _, value := range response.Data {
			values = append(values, QueuedSubmission{ID: value.ID, ClientUserMessageID: value.ClientUserMessageID})
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			return values, nil
		}
		cursor = response.NextCursor
	}
}

func (c *StdioAppServer) sendRequest(ctx context.Context, method string, params any, target any) error {
	c.mu.Lock()
	if c.closed || c.closing {
		err := c.terminalErrorLocked()
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	idRaw := json.RawMessage(strconv.AppendInt(nil, id, 10))
	key := rpcIDKey(idRaw)
	response := make(chan rpcResponse, 1)
	c.pending[key] = response
	c.mu.Unlock()

	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.removePending(key)
		return fmt.Errorf("encode %s: %w", method, err)
	}
	c.writeMu.Lock()
	err = writeFrame(c.stdin, append(message, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(key)
		return fmt.Errorf("write %s: %w", method, err)
	}
	select {
	case result, ok := <-response:
		if !ok {
			return c.terminalError()
		}
		if result.err != nil {
			return result.err
		}
		if err := json.Unmarshal(result.result, target); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		return ctx.Err()
	case <-c.done:
		return c.terminalError()
	}
}

// notifyInitialized writes the required client notification after the
// initialize response. It intentionally holds neither state mutex while the
// child pipe write can block.
func (c *StdioAppServer) notifyInitialized() error {
	c.mu.Lock()
	if c.closed || c.closing {
		err := c.terminalErrorLocked()
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = writeFrame(c.stdin, append(message, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write initialized: %w", err)
	}
	return nil
}

func (c *StdioAppServer) awaitInitialized(ctx context.Context) error {
	c.mu.Lock()
	if c.handshakeComplete {
		err := c.handshakeErr
		c.mu.Unlock()
		return err
	}
	if !c.handshakeStarted {
		c.mu.Unlock()
		return errors.New("Codex App Server must be initialized before issuing requests")
	}
	done := c.handshakeDone
	c.mu.Unlock()
	return c.waitHandshake(ctx, done)
}

func (c *StdioAppServer) waitHandshake(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		c.mu.Lock()
		err := c.handshakeErr
		c.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.terminalError()
	}
}

func (c *StdioAppServer) completeHandshake(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handshakeComplete {
		return
	}
	c.handshakeErr = err
	c.handshakeComplete = true
	close(c.handshakeDone)
}

func (c *StdioAppServer) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32*1024), 4*1024*1024)
	for scanner.Scan() {
		c.handleFrame(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("read Codex App Server output: %w", err))
	}
}

func (c *StdioAppServer) wait() {
	err := c.command.Wait()
	if err != nil {
		c.fail(fmt.Errorf("Codex App Server exited: %w", err))
		return
	}
	c.fail(errors.New("Codex App Server exited"))
}

func (c *StdioAppServer) Close() error {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	if c.command != nil && c.command.Process != nil {
		_ = c.command.Process.Kill()
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		return errors.New("timed out stopping Codex App Server")
	}
	return nil
}

func (c *StdioAppServer) handleFrame(frame []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return
	}
	if envelope.Method != "" && len(envelope.ID) == 0 {
		c.mu.Lock()
		notify := c.notify
		c.mu.Unlock()
		if notify != nil {
			notify(envelope.Method, envelope.Params)
		}
	}
	if envelope.Method == "item/completed" {
		var completed struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(envelope.Params, &completed) == nil && completed.Item.Type == "agentMessage" && strings.TrimSpace(completed.Item.Text) != "" {
			c.mu.Lock()
			if _, tracked := c.ephemeralThreads[completed.ThreadID]; tracked {
				if c.agentMessages == nil {
					c.agentMessages = make(map[string]string)
				}
				c.agentMessages[turnKey(completed.ThreadID, completed.TurnID)] = completed.Item.Text
			}
			c.mu.Unlock()
		}
		return
	}
	if envelope.Method == "turn/completed" {
		var completed struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(envelope.Params, &completed) == nil && completed.ThreadID != "" && completed.Turn.ID != "" {
			value := TurnCompletion{ThreadID: completed.ThreadID, TurnID: completed.Turn.ID, Status: completed.Turn.Status}
			if completed.Turn.Error != nil {
				value.Error = completed.Turn.Error.Message
			}
			key := turnKey(value.ThreadID, value.TurnID)
			c.mu.Lock()
			value.LastMessage = c.agentMessages[key]
			delete(c.agentMessages, key)
			if wait := c.turnWaiters[key]; wait != nil {
				delete(c.turnWaiters, key)
				wait <- value
				close(wait)
			} else if _, tracked := c.ephemeralThreads[value.ThreadID]; tracked {
				c.completedTurns[key] = value
			}
			c.mu.Unlock()
		}
		return
	}
	if envelope.Method != "" && len(envelope.ID) > 0 && string(envelope.ID) != "null" {
		if !validRPCID(envelope.ID) {
			return
		}
		if envelope.Method == "item/tool/call" {
			c.mu.Lock()
			handler := c.dynamicHandler
			c.mu.Unlock()
			if handler != nil {
				id, params := append(json.RawMessage(nil), envelope.ID...), append(json.RawMessage(nil), envelope.Params...)
				go func() {
					ctx, cancel := context.WithTimeout(c.toolContext, 5*time.Second)
					defer cancel()
					result, err := handler(ctx, params)
					if err != nil {
						result, _ = json.Marshal(map[string]any{"success": false, "contentItems": []map[string]string{{"type": "inputText", "text": err.Error()}}})
					}
					if err := c.writeServerResult(id, result); err != nil {
						c.fail(fmt.Errorf("write dynamic tool result: %w", err))
					}
				}()
				return
			}
		}
		interaction := NativeInteraction{Method: envelope.Method, Params: append(json.RawMessage(nil), envelope.Params...), CreatedAt: time.Now().UTC()}
		var scope struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		_ = json.Unmarshal(envelope.Params, &scope)
		interaction.ThreadID, interaction.TurnID = scope.ThreadID, scope.TurnID
		c.mu.Lock()
		if !c.closed {
			c.interactionSeq++
			interaction.ID = fmt.Sprintf("interaction-%s-%d", c.instancePrefix, c.interactionSeq)
			c.interactions[interaction.ID] = pendingInteraction{id: append(json.RawMessage(nil), envelope.ID...), method: envelope.Method, value: interaction}
		}
		c.mu.Unlock()
		return
	}
	if envelope.Method == "serverRequest/resolved" {
		var resolved struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(envelope.Params, &resolved) == nil && validRPCID(resolved.RequestID) {
			c.responseMu.Lock()
			c.mu.Lock()
			for key, pending := range c.interactions {
				if rpcIDKey(pending.id) == rpcIDKey(resolved.RequestID) {
					delete(c.interactions, key)
				}
			}
			c.mu.Unlock()
			c.responseMu.Unlock()
		}
		return
	}
	if len(envelope.ID) == 0 || !validRPCID(envelope.ID) {
		return
	}
	c.mu.Lock()
	key := rpcIDKey(envelope.ID)
	response := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if response != nil {
		response <- rpcResponse{result: envelope.Result, err: envelope.Error}
		close(response)
	}
}

func (c *StdioAppServer) writeServerResult(id json.RawMessage, result json.RawMessage) error {
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": json.RawMessage(result)})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.stdin, append(message, '\n'))
}

func writeFrame(writer io.Writer, frame []byte) error {
	written, err := writer.Write(frame)
	if err == nil && written != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func validRPCID(value json.RawMessage) bool {
	var stringID string
	if json.Unmarshal(value, &stringID) == nil {
		return stringID != ""
	}
	var numberID json.Number
	return json.Unmarshal(value, &numberID) == nil && numberID.String() != ""
}

// rpcIDKey preserves JSON-RPC's distinct number and string identifier spaces:
// numeric 1 must never route a response intended for string "1".
func rpcIDKey(value json.RawMessage) string {
	var stringID string
	if json.Unmarshal(value, &stringID) == nil {
		return "s:" + stringID
	}
	var number json.Number
	if json.Unmarshal(value, &number) == nil {
		return "n:" + number.String()
	}
	return ""
}

func (c *StdioAppServer) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *StdioAppServer) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	if c.toolCancel != nil {
		c.toolCancel()
	}
	if !c.handshakeComplete {
		c.handshakeErr = err
		c.handshakeComplete = true
		close(c.handshakeDone)
	}
	for id, response := range c.pending {
		delete(c.pending, id)
		close(response)
	}
	for id, waiter := range c.turnWaiters {
		delete(c.turnWaiters, id)
		close(waiter)
	}
	c.completedTurns = make(map[string]TurnCompletion)
	c.agentMessages = nil
	c.ephemeralThreads = make(map[string]struct{})
	c.interactions = make(map[string]pendingInteraction)
	c.closed = true
	close(c.done)
	c.mu.Unlock()
}

func (c *StdioAppServer) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminalErrorLocked()
}

func (c *StdioAppServer) terminalErrorLocked() error {
	if c.err != nil {
		return c.err
	}
	return errors.New("Codex App Server is closed")
}

type nativeThreadWire struct {
	ID                   string  `json:"id"`
	Name                 *string `json:"name"`
	CWD                  string  `json:"cwd"`
	Ephemeral            bool    `json:"ephemeral"`
	CanAcceptDirectInput *bool   `json:"canAcceptDirectInput"`
	Status               struct {
		Type string `json:"type"`
	} `json:"status"`
}

func (v nativeThreadWire) native() NativeThread {
	name := ""
	if v.Name != nil {
		name = *v.Name
	}
	accepts := v.CanAcceptDirectInput == nil || *v.CanAcceptDirectInput
	return NativeThread{ID: v.ID, Name: name, CWD: v.CWD, Status: v.Status.Type, Ephemeral: v.Ephemeral, AcceptsInput: accepts}
}
