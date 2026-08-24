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
	"sync"
	"time"
)

// AppServer is the small Codex protocol surface owned by this adapter. It is
// injectable so delivery behavior can be proven without Codex.
type AppServer interface {
	Initialize(context.Context) error
	ListThreads(context.Context) ([]NativeThread, error)
	ReadThread(context.Context, string) (NativeThread, error)
	QueueAdd(context.Context, string, string, string) (QueuedSubmission, error)
	QueueList(context.Context, string) ([]QueuedSubmission, error)
	StartThread(context.Context, string) (NativeThread, error)
	Interrupt(context.Context, string, string) error
	Interactions() []NativeInteraction
	RespondInteraction(context.Context, string, string, json.RawMessage) error
	Close() error
}

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

// NativeThread is the canonical history shape consumed by the projector.
// It accepts only fields this adapter owns; omitted native item types stay out
// of the durable projection rather than becoming an accidental transcript API.
type NativeThread struct {
	ID     string
	Name   string
	CWD    string
	Status string
	Turns  []NativeTurn
}

type NativeTurn struct {
	ID     string
	Status string
	Items  []NativeItem
}

type NativeItem struct {
	ID       string
	Type     string
	Text     string
	ClientID string
	Content  []NativeContent
}

// QueuedSubmission is the durable non-interrupting native admission receipt.
// ClientUserMessageID is adapter-owned and survives App Server restarts.
type QueuedSubmission struct {
	ID                  string
	ClientUserMessageID string
}

type NativeContent struct {
	Type string
	Text string
}

// StdioAppServer supervises one child and a line-delimited JSON-RPC 2.0
// connection. Notifications are read and intentionally ignored: canonical
// thread/read history is the replayable projection authority.
type StdioAppServer struct {
	command *exec.Cmd
	stdin   io.WriteCloser

	mu             sync.Mutex
	writeMu        sync.Mutex
	responseMu     sync.Mutex
	nextID         int64
	pending        map[string]chan rpcResponse
	interactions   map[string]pendingInteraction
	interactionSeq uint64
	instancePrefix string
	dynamicHandler func(context.Context, json.RawMessage) (json.RawMessage, error)
	toolContext    context.Context
	toolCancel     context.CancelFunc
	closing        bool
	closed         bool
	done           chan struct{}
	err            error

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
	toolContext, toolCancel := context.WithCancel(context.Background())
	client := &StdioAppServer{command: cmd, stdin: stdin, pending: make(map[string]chan rpcResponse), interactions: make(map[string]pendingInteraction), done: make(chan struct{}), handshakeDone: make(chan struct{}), instancePrefix: hex.EncodeToString(prefix), toolContext: toolContext, toolCancel: toolCancel}
	go client.read(stdout)
	go client.wait()
	return client, nil
}

// StartThread creates a new native Codex thread. The current protocol has no
// client operation identity for this effect, so a lost response is surfaced to
// the caller rather than retried into a duplicate thread.
func (c *StdioAppServer) StartThread(ctx context.Context, cwd string) (NativeThread, error) {
	var response struct {
		Thread nativeThreadWire `json:"thread"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return NativeThread{}, err
	}
	params := map[string]any{}
	params["dynamicTools"] = crewDynamicTools()
	if cwd != "" {
		params["cwd"] = cwd
	}
	if err := c.sendRequest(ctx, "thread/start", params, &response); err != nil {
		return NativeThread{}, err
	}
	return response.Thread.native(), nil
}

func crewDynamicTools() []map[string]any {
	return []map[string]any{{"type": "function", "name": "crew_directory", "description": "List routable Crew addresses.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}, {"type": "function", "name": "crew_message", "description": "Send one Crew message to a routable address.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"recipient": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}, "reply_to_message_id": map[string]any{"type": "string"}}, "required": []string{"recipient", "text"}}}}
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

// RespondInteraction settles one exact pending server request with its native
// response payload. A stale, resolved, or wrong-type interaction is rejected.
func (c *StdioAppServer) RespondInteraction(ctx context.Context, id, method string, response json.RawMessage) error {
	c.responseMu.Lock()
	defer c.responseMu.Unlock()
	c.mu.Lock()
	pending, found := c.interactions[id]
	if !found || pending.method != method {
		c.mu.Unlock()
		return errors.New("Codex interaction is no longer pending")
	}
	c.mu.Unlock()
	if !json.Valid(response) {
		return errors.New("Codex interaction response must be JSON")
	}
	message, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: pending.id, Result: response})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = writeFrame(c.stdin, append(message, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write %s interaction response: %w", method, err)
	}
	c.mu.Lock()
	if current, found := c.interactions[id]; found && rpcIDKey(current.id) == rpcIDKey(pending.id) && current.method == method {
		delete(c.interactions, id)
	}
	c.mu.Unlock()
	return nil
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

func (c *StdioAppServer) ListThreads(ctx context.Context) ([]NativeThread, error) {
	var response struct {
		Data []nativeThreadWire `json:"data"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return nil, err
	}
	if err := c.sendRequest(ctx, "thread/list", map[string]any{}, &response); err != nil {
		return nil, err
	}
	threads := make([]NativeThread, 0, len(response.Data))
	for _, thread := range response.Data {
		threads = append(threads, thread.native())
	}
	return threads, nil
}

func (c *StdioAppServer) ReadThread(ctx context.Context, threadID string) (NativeThread, error) {
	var response struct {
		Thread nativeThreadWire `json:"thread"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return NativeThread{}, err
	}
	if err := c.sendRequest(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &response); err != nil {
		return NativeThread{}, err
	}
	return response.Thread.native(), nil
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
	if c.command.Process != nil {
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
	ID     string  `json:"id"`
	Name   *string `json:"name"`
	CWD    string  `json:"cwd"`
	Status struct {
		Type string `json:"type"`
	} `json:"status"`
	Turns []nativeTurnWire `json:"turns"`
}

type nativeTurnWire struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Items  []nativeItemWire `json:"items"`
}

type nativeItemWire struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Text     string              `json:"text"`
	ClientID string              `json:"clientId"`
	Content  []nativeContentWire `json:"content"`
}

type nativeContentWire struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (v nativeThreadWire) native() NativeThread {
	name := ""
	if v.Name != nil {
		name = *v.Name
	}
	turns := make([]NativeTurn, 0, len(v.Turns))
	for _, turn := range v.Turns {
		items := make([]NativeItem, 0, len(turn.Items))
		for _, item := range turn.Items {
			content := make([]NativeContent, 0, len(item.Content))
			for _, part := range item.Content {
				content = append(content, NativeContent{Type: part.Type, Text: part.Text})
			}
			items = append(items, NativeItem{ID: item.ID, Type: item.Type, Text: item.Text, ClientID: item.ClientID, Content: content})
		}
		turns = append(turns, NativeTurn{ID: turn.ID, Status: turn.Status, Items: items})
	}
	return NativeThread{ID: v.ID, Name: name, CWD: v.CWD, Status: v.Status.Type, Turns: turns}
}
