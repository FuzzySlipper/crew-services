package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

const mcpProtocolVersion = "2025-06-18"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lockedMCPEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

type activeMCPRequest struct {
	cancel     context.CancelFunc
	generation uint64
}

func (e *lockedMCPEncoder) Encode(value any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(value)
}

// ServeMCP serves the basic JSON-RPC MCP stdio protocol. It never owns a
// session: each tool call forwards one explicit command to the local service.
func ServeMCP(ctx context.Context, input io.Reader, output io.Writer, service *Client) error {
	if service == nil {
		return fmt.Errorf("playtest MCP requires a client")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	encoder := &lockedMCPEncoder{encoder: json.NewEncoder(output)}
	var requests sync.WaitGroup
	var activeMu sync.Mutex
	active := map[string]activeMCPRequest{}
	var generation uint64
	var writeMu sync.Mutex
	var writeErr error
	recordWriteError := func(err error) {
		if err == nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr == nil {
			writeErr = err
		}
	}
	for scanner.Scan() {
		var request mcpRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			if err := writeMCPError(encoder, nil, -32700, "parse error"); err != nil {
				recordWriteError(err)
			}
			continue
		}
		if request.JSONRPC != "2.0" {
			if err := writeMCPError(encoder, request.ID, -32600, "invalid JSON-RPC version"); err != nil {
				recordWriteError(err)
			}
			continue
		}
		if request.Method == "notifications/cancelled" {
			if key, ok := cancelledRequestKey(request.Params); ok {
				activeMu.Lock()
				pending, found := active[key]
				activeMu.Unlock()
				if found {
					pending.cancel()
				}
			}
			continue
		}
		isNotification := len(request.ID) == 0 || string(request.ID) == "null"
		requestCtx := ctx
		var cancel context.CancelFunc
		var key string
		var requestGeneration uint64
		if !isNotification {
			key, _ = mcpRequestKey(request.ID)
			requestCtx, cancel = context.WithCancel(ctx)
			activeMu.Lock()
			generation++
			requestGeneration = generation
			active[key] = activeMCPRequest{cancel: cancel, generation: requestGeneration}
			activeMu.Unlock()
		}
		requests.Add(1)
		go func(request mcpRequest, requestCtx context.Context, isNotification bool, key string, requestGeneration uint64, cancel context.CancelFunc) {
			defer requests.Done()
			if cancel != nil {
				defer cancel()
				defer func() {
					activeMu.Lock()
					if pending, ok := active[key]; ok && pending.generation == requestGeneration {
						delete(active, key)
					}
					activeMu.Unlock()
				}()
			}
			result, rpcError := handleMCP(requestCtx, request, service)
			if isNotification {
				return
			}
			if rpcError != nil {
				recordWriteError(writeMCPError(encoder, request.ID, rpcError.Code, rpcError.Message))
				return
			}
			recordWriteError(encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result}))
		}(request, requestCtx, isNotification, key, requestGeneration, cancel)
	}
	readErr := scanner.Err()
	activeMu.Lock()
	pending := make([]activeMCPRequest, 0, len(active))
	for _, request := range active {
		pending = append(pending, request)
	}
	active = map[string]activeMCPRequest{}
	activeMu.Unlock()
	for _, request := range pending {
		request.cancel()
	}
	requests.Wait()
	if readErr != nil {
		return fmt.Errorf("read MCP request: %w", readErr)
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if writeErr != nil {
		return fmt.Errorf("write MCP response: %w", writeErr)
	}
	return nil
}

func mcpRequestKey(id json.RawMessage) (string, bool) {
	var value any
	if len(id) == 0 || string(id) == "null" || json.Unmarshal(id, &value) != nil {
		return "", false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(canonical), true
}

func cancelledRequestKey(params json.RawMessage) (string, bool) {
	var notification struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &notification); err != nil {
		return "", false
	}
	return mcpRequestKey(notification.RequestID)
}

func writeMCPError(encoder *lockedMCPEncoder, id json.RawMessage, code int, message string) error {
	if id == nil {
		id = json.RawMessage("null")
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": mcpError{Code: code, Message: message}}); err != nil {
		return fmt.Errorf("write MCP error: %w", err)
	}
	return nil
}

func handleMCP(ctx context.Context, request mcpRequest, service *Client) (any, *mcpError) {
	switch request.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "playtest", "version": "0.1.0"},
		}, nil
	case "notifications/initialized", "notifications/cancelled", "initialized":
		return map[string]any{}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		return callMCPTool(ctx, request.Params, service)
	default:
		return nil, &mcpError{Code: -32601, Message: "method not found"}
	}
}

func callMCPTool(ctx context.Context, raw json.RawMessage, service *Client) (any, *mcpError) {
	var call struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, &mcpError{Code: -32602, Message: "tools/call params must include name and arguments"}
	}
	command, err := mcpCommand(call.Name, call.Arguments)
	if err != nil {
		return nil, &mcpError{Code: -32602, Message: err.Error()}
	}
	result, err := service.Result(ctx, command)
	if err != nil {
		return toolFailure(err), nil
	}
	return toolResult(call.Name, result), nil
}

func toolFailure(err error) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]string{{"type": "text", "text": err.Error()}},
	}
}

func toolResult(name string, result json.RawMessage) map[string]any {
	formatted := formatJSON(result)
	response := map[string]any{
		"content":           []any{map[string]string{"type": "text", "text": formatted}},
		"structuredContent": json.RawMessage(result),
	}
	var path string
	var imageLabel string
	var checkpointImage bool
	switch name {
	case "observe", "capture":
		var ok bool
		path, ok = observePath(result)
		if !ok {
			return response
		}
		imageLabel = "observe image unavailable: "
	case "script":
		var ok bool
		path, ok = checkpointImagePath(result)
		if !ok {
			return response
		}
		imageLabel = "checkpoint image unavailable: "
		checkpointImage = true
	default:
		return response
	}
	image, err := readImage(path)
	if err != nil {
		response["content"] = append(response["content"].([]any), map[string]string{"type": "text", "text": imageLabel + err.Error()})
		return response
	}
	if checkpointImage && image["mimeType"] != "image/png" {
		return response
	}
	response["content"] = append(response["content"].([]any), image)
	return response
}

func formatJSON(value json.RawMessage) string {
	var pretty bytes.Buffer
	if json.Indent(&pretty, value, "", "  ") == nil {
		return pretty.String()
	}
	return string(value)
}

func observePath(result json.RawMessage) (string, bool) {
	var value struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(result, &value); err != nil || strings.TrimSpace(value.Path) == "" {
		return "", false
	}
	return value.Path, true
}

func checkpointImagePath(result json.RawMessage) (string, bool) {
	var value struct {
		Checkpoint struct {
			Data struct {
				Path string `json:"path"`
			} `json:"data"`
		} `json:"checkpoint"`
	}
	if err := json.Unmarshal(result, &value); err != nil || strings.TrimSpace(value.Checkpoint.Data.Path) == "" {
		return "", false
	}
	return value.Checkpoint.Data.Path, true
}

func readImage(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxResponseBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxResponseBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("%s is %s, not an image", path, mime)
	}
	return map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": mime}, nil
}

func mcpCommand(name string, arguments map[string]json.RawMessage) (Request, error) {
	if arguments == nil {
		arguments = map[string]json.RawMessage{}
	}
	command := Request{}
	switch name {
	case "browser", "capture":
		id, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: name, SessionID: id, Data: arguments["data"]}
	case "interaction":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		data := arguments["data"]
		if len(data) == 0 {
			data = json.RawMessage("{}")
		}
		command = Request{Op: "interaction", SessionID: session, Data: data}
	case "games":
		command.Op = "games"
	case "game":
		game, err := requiredString(arguments, "game")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "game", Game: game}
	case "start":
		game, err := requiredString(arguments, "game")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "start", Game: game}
	case "status":
		command.Op = "status"
		command.SessionID, _ = optionalString(arguments, "session_id")
	case "observe":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "observe", SessionID: session}
	case "input":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		steps, ok := arguments["steps"]
		if !ok {
			return Request{}, fmt.Errorf("steps is required")
		}
		steps, err = NormalizeSteps(steps)
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "input", SessionID: session, Steps: steps}
	case "run":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		source, err := requiredString(arguments, "source")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "run", SessionID: session, Source: source}
		if budget, ok, err := optionalInt(arguments, "budget_ms"); err != nil {
			return Request{}, err
		} else if ok {
			command.BudgetMS = &budget
		}
	case "script":
		script, err := requiredString(arguments, "script_id")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "script", ScriptID: script}
	case "cancel", "recover", "stop":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: name, SessionID: session}
	case "resume":
		session, err := requiredString(arguments, "session_id")
		if err != nil {
			return Request{}, err
		}
		command = Request{Op: "resume", SessionID: session, Data: arguments["data"]}
	default:
		return Request{}, fmt.Errorf("unknown playtest tool %q", name)
	}
	return command, nil
}

func requiredString(values map[string]json.RawMessage, name string) (string, error) {
	value, ok := values[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return text, nil
}

func optionalString(values map[string]json.RawMessage, name string) (string, bool) {
	value, ok := values[name]
	if !ok {
		return "", false
	}
	var text string
	if json.Unmarshal(value, &text) != nil {
		return "", false
	}
	return text, true
}

func optionalInt(values map[string]json.RawMessage, name string) (int, bool, error) {
	value, ok := values[name]
	if !ok {
		return 0, false, nil
	}
	var number int
	if err := json.Unmarshal(value, &number); err != nil || number < 1 {
		return 0, false, fmt.Errorf("%s must be a positive integer", name)
	}
	return number, true, nil
}

func mcpTools() []map[string]any {
	return []map[string]any{
		tool("browser", "Browser-only operations: inspect {selector?}, click {selector} or {x,y}, fill {selector,value}, press {key}, near {x,y,max_distance}, select {token,action?:move|click}. data.op selects the operation. Unsupported on native game sessions; actions are never silently translated to synthetic page events.", properties(map[string]any{"session_id": stringField("Session identifier."), "data": anyField("Browser operation object, including op.")}, "session_id", "data")),
		tool("capture", "Capture original image plus comparison metadata. Optional data: label, compare_to (prior capture_id), viewpoint (caller supplied), assistance (caller supplied string array), overlay_policy (preserve only), engine_presentation (boolean override of profile presentation_observations; records separate Engine submitted camera/viewport facts). Does not establish frame freshness or visual acceptance.", properties(map[string]any{"session_id": stringField("Session identifier."), "data": anyField("Optional capture metadata and comparison options.")}, "session_id")),
		tool("games", "List games the local playtest service can start.", properties(map[string]any{})),
		tool("game", "Read one game's service-provided metadata.", properties(map[string]any{"game": stringField("Game identifier.")}, "game")),
		tool("start", "Start a game session in a free configured pool slot; returns session id and slot_id. A full pool queues briefly, then reports pool_busy. A connected result means the stream transport connected, not that the game is visually ready; use observe to inspect readiness.", properties(map[string]any{"game": stringField("Game identifier.")}, "game")),
		tool("status", "Read one session's status, or aggregate pool occupancy and all slots when no session_id is supplied.", properties(map[string]any{"session_id": stringField("Optional session identifier.")})),
		tool("observe", "Capture current session observation. When the result has a local image path, returns it as MCP image content.", properties(map[string]any{"session_id": stringField("Session identifier.")}, "session_id")),
		tool("interaction", "Read the game's optional interaction query at the reticle, or at an explicitly supplied normalized bottom-left cursor. This read-only result is product-provided query evidence, not screenshot freshness or permission to activate anything. Send normal input to act, then query again. Candidate route unknown remains unknown.", properties(map[string]any{"session_id": stringField("Session identifier."), "data": objectField("Optional interaction options: mode reticle (default) or cursor with x, y, and aspect.")}, "session_id")),
		tool("input", "Send structured input steps to a running session.", properties(map[string]any{"session_id": stringField("Session identifier."), "steps": anyField("Input steps object or array.")}, "session_id", "steps")),
		tool("run", "Start an asynchronous JavaScript script and poll its script identifier with script. The default budget is 60000 ms; 100–120000 ms includes yield pauses. Scripts may call controller.hold, keyboard.hold, observe, interaction(options), input, sleep, checkpoint, yieldToAgent, browser(operation), and capture(options).", properties(map[string]any{"session_id": stringField("Session identifier."), "source": stringField("Script source."), "budget_ms": map[string]any{"type": "integer", "minimum": 100, "maximum": 120000, "description": "Optional total execution budget in milliseconds, including yields."}}, "session_id", "source")),
		tool("script", "Read asynchronous script progress or result, including its latest checkpoint and service-owned source/events artifact paths.", properties(map[string]any{"script_id": stringField("Script identifier.")}, "script_id")),
		tool("cancel", "Cancel work in a session.", properties(map[string]any{"session_id": stringField("Session identifier.")}, "session_id")),
		tool("resume", "Resume a paused session with optional service-defined data.", properties(map[string]any{"session_id": stringField("Session identifier."), "data": anyField("Optional resume data.")}, "session_id")),
		tool("recover", "Explicitly restart and recover a session after failure; this is not an automatic retry.", properties(map[string]any{"session_id": stringField("Session identifier.")}, "session_id")),
		tool("stop", "Stop a session.", properties(map[string]any{"session_id": stringField("Session identifier.")}, "session_id")),
	}
}

func tool(name, description string, inputSchema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": inputSchema}
}

func properties(values map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": values, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringField(description string) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "description": description}
}
func objectField(description string) map[string]any {
	return map[string]any{"type": "object", "description": description}
}
func anyField(description string) map[string]any { return map[string]any{"description": description} }
