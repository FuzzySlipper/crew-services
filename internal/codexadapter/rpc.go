package codexadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	Close() error
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

	mu      sync.Mutex
	writeMu sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
	closing bool
	closed  bool
	done    chan struct{}
	err     error

	handshakeDone     chan struct{}
	handshakeComplete bool
	handshakeStarted  bool
	handshakeErr      error
}

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
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
	client := &StdioAppServer{command: cmd, stdin: stdin, pending: make(map[int64]chan rpcResponse), done: make(chan struct{}), handshakeDone: make(chan struct{})}
	go client.read(stdout)
	go client.wait()
	return client, nil
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
	response := make(chan rpcResponse, 1)
	c.pending[id] = response
	c.mu.Unlock()

	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.removePending(id)
		return fmt.Errorf("encode %s: %w", method, err)
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(message, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(id)
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
		c.removePending(id)
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
	_, err = c.stdin.Write(append(message, '\n'))
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
		var envelope struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil || envelope.ID == nil {
			continue // Notifications and malformed unsolicited frames are non-authoritative here.
		}
		c.mu.Lock()
		response := c.pending[*envelope.ID]
		delete(c.pending, *envelope.ID)
		c.mu.Unlock()
		if response != nil {
			response <- rpcResponse{result: envelope.Result, err: envelope.Error}
			close(response)
		}
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

func (c *StdioAppServer) removePending(id int64) {
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
	if !c.handshakeComplete {
		c.handshakeErr = err
		c.handshakeComplete = true
		close(c.handshakeDone)
	}
	for id, response := range c.pending {
		delete(c.pending, id)
		close(response)
	}
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
