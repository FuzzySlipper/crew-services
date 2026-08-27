package codexadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Controls owns ephemeral operator actions for the currently running Codex
// child. It is not a fabric delivery adapter: ordinary prompts remain #7234
// fabric messages and all request callbacks disappear when the child does.
type Controls struct {
	mu          sync.Mutex
	fabric      Fabric
	cfg         Config
	native      AppServer
	lease       Lease
	mappings    map[string]Mapping // public session id -> native mapping
	created     map[string]ControlSession
	threads     map[string]NativeThread
	creating    map[string]chan struct{}
	interrupted map[string]struct{}
	persisted   map[string]persistedControlSession
}

// ControlSession is the browser-safe public identity returned after a native
// thread has been adopted and bound by this adapter.
type ControlSession struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
	Status    string `json:"status"`
}

// PendingInteraction excludes native callback details and is valid only until
// the App Server child resolves or exits the request.
type PendingInteraction struct {
	ID               string                `json:"id"`
	SessionID        string                `json:"session_id"`
	Kind             string                `json:"kind"`
	Prompt           string                `json:"prompt,omitempty"`
	AllowedDecisions []string              `json:"allowed_decisions,omitempty"`
	CreatedAt        string                `json:"created_at"`
	Status           string                `json:"status"`
	Capability       string                `json:"capability"`
	Questions        []InteractionQuestion `json:"questions,omitempty"`
	Permissions      []string              `json:"permissions,omitempty"`
}

// InteractionQuestion is the deliberately browser-safe subset of Codex's
// request-user-input schema. Secrets are marked but never logged or stored.
type InteractionQuestion struct {
	ID        string              `json:"id"`
	Header    string              `json:"header"`
	Question  string              `json:"question"`
	Options   []InteractionOption `json:"options,omitempty"`
	Sensitive bool                `json:"sensitive,omitempty"`
}
type InteractionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Capabilities describes adapter-global controls. Session-local controls are
// published with the individual fabric session capabilities.
func (c *Controls) Capabilities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.native == nil || c.lease.LeaseToken == "" {
		return nil
	}
	return []string{"create-codex-session"}
}

func NewControls(fabric Fabric, cfg Config) *Controls {
	persisted, err := loadControlState(cfg.StatePath)
	if err != nil {
		persisted = map[string]persistedControlSession{}
	}
	created, mappings := make(map[string]ControlSession, len(persisted)), make(map[string]Mapping, len(persisted))
	for operation, value := range persisted {
		created[operation] = value.Session
		mappings[value.Session.SessionID] = value.Mapping
	}
	return &Controls{fabric: fabric, cfg: cfg, mappings: mappings, created: created, threads: make(map[string]NativeThread), creating: make(map[string]chan struct{}), interrupted: make(map[string]struct{}), persisted: persisted}
}

func (c *Controls) Attach(native AppServer, lease Lease, mappings []Mapping) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.native, c.lease = native, lease
	if stdio, ok := native.(*StdioAppServer); ok {
		stdio.SetDynamicToolHandler(c.dynamicTool)
	}
	for _, mapping := range mappings {
		// Existing sessions are populated by Observe after their normal adoption.
		_ = mapping
	}
}

func (c *Controls) dynamicTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > 32*1024 {
		return nil, errors.New("Crew dynamic tool arguments are too large")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return nil, errors.New("dynamic tool call must be an object")
	}
	for key := range envelope {
		if key != "threadId" && key != "turnId" && key != "callId" && key != "namespace" && key != "tool" && key != "arguments" {
			return nil, errors.New("unknown dynamic tool call field")
		}
	}
	var call struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		CallID    string          `json:"callId"`
		Tool      string          `json:"tool"`
		Namespace *string         `json:"namespace"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &call) != nil || call.ThreadID == "" || call.TurnID == "" || call.CallID == "" || call.Tool == "" || (call.Namespace != nil && *call.Namespace != "") {
		return nil, errors.New("invalid Crew dynamic tool call")
	}
	c.mu.Lock()
	var sessionID string
	var mapping Mapping
	toolEnabled := false
	for id, value := range c.mappings {
		if value.ThreadID == call.ThreadID {
			sessionID, mapping = id, value
			for _, persisted := range c.persisted {
				if persisted.Session.SessionID == id && persisted.ToolEnabled {
					toolEnabled = true
				}
			}
			break
		}
	}
	lease := c.lease
	c.mu.Unlock()
	if sessionID == "" || !toolEnabled || lease.LeaseToken == "" {
		return nil, errors.New("Crew tools are unavailable for this thread")
	}
	binding, err := c.fabric.Resolve(ctx, mapping.Address)
	if err != nil || !binding.Bound || binding.AdapterID != c.cfg.AdapterID || binding.TargetRef != sessionID {
		return nil, errors.New("Crew tools are no longer authorized for this thread")
	}
	switch call.Tool {
	case "crew_directory":
		var args map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &args) != nil || args == nil || len(args) != 0 {
			return nil, errors.New("crew_directory takes no arguments")
		}
		bindings, err := c.fabric.Bindings(ctx)
		if err != nil {
			return nil, err
		}
		addresses := make([]string, 0)
		for _, value := range bindings {
			if value.Bound && value.TargetRef != "" && value.AdapterID != "" && inboundCapability(value.Capabilities) {
				addresses = append(addresses, value.Address)
			}
		}
		return dynamicResult(true, fmt.Sprintf("routable Crew addresses: %s", strings.Join(addresses, ", "))), nil
	case "crew_message":
		var rawArgs map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &rawArgs) != nil {
			return nil, errors.New("invalid crew_message arguments")
		}
		for key := range rawArgs {
			if key != "recipient" && key != "text" && key != "reply_to_message_id" {
				return nil, errors.New("unknown crew_message argument")
			}
		}
		var args struct {
			Recipient string `json:"recipient"`
			Text      string `json:"text"`
			ReplyTo   string `json:"reply_to_message_id"`
		}
		if json.Unmarshal(call.Arguments, &args) != nil || strings.TrimSpace(args.Recipient) == "" || strings.TrimSpace(args.Text) == "" {
			return nil, errors.New("crew_message requires recipient and text")
		}
		if len(args.Recipient) > 256 || len(args.ReplyTo) > 256 || len(args.Text) > 12_000 {
			return nil, errors.New("crew_message argument exceeds limit")
		}
		recipient, err := c.fabric.Resolve(ctx, args.Recipient)
		if err != nil || !recipient.Bound || !inboundCapability(recipient.Capabilities) {
			return nil, errors.New("Crew recipient is not routable")
		}
		operation := toolOperation(call.ThreadID, call.CallID)
		result, err := c.fabric.Submit(ctx, MessageRequest{ProducerID: c.cfg.AdapterID, LeaseToken: lease.LeaseToken, OperationID: operation, SenderAddress: mapping.Address, RecipientAddress: recipient.Address, Body: args.Text, ReplyToMessageID: args.ReplyTo, ActivationPolicy: "wake_when_idle", TTL: "24h", ExpectedSenderGeneration: &binding.Generation, ExpectedRecipientGeneration: &recipient.Generation})
		if err != nil {
			return nil, err
		}
		return dynamicResult(true, fmt.Sprintf("messageId=%s replayed=%t", result.Message.MessageID, result.Replayed)), nil
	default:
		return nil, errors.New("unsupported Crew dynamic tool")
	}
}
func inboundCapability(values []string) bool {
	for _, value := range values {
		if value == "deliver_when_idle" || value == "durable_next_turn" || value == "wake_inactive" {
			return true
		}
	}
	return false
}
func toolOperation(threadID, callID string) string {
	canonical, _ := json.Marshal(struct {
		ThreadID string `json:"thread_id"`
		CallID   string `json:"call_id"`
	}{ThreadID: threadID, CallID: callID})
	sum := sha256.Sum256(canonical)
	return "codex-tool:" + hex.EncodeToString(sum[:])
}
func dynamicResult(success bool, text string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{"success": success, "contentItems": []map[string]string{{"type": "inputText", "text": text}}})
	return value
}

func (c *Controls) Mappings(base []Mapping) []Mapping {
	c.mu.Lock()
	defer c.mu.Unlock()
	values := append([]Mapping(nil), base...)
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value.ThreadID] = struct{}{}
	}
	for _, value := range c.mappings {
		if _, found := seen[value.ThreadID]; !found {
			values = append(values, value)
			seen[value.ThreadID] = struct{}{}
		}
	}
	return values
}

// UnmaterializedThreads returns the creation receipts that may be queued once
// before Codex can expose their canonical turn history. A persisted create
// mapping from an older adapter has no receipt details, but it still denotes a
// browser-created thread and can safely retain the exact native identity.
func (c *Controls) UnmaterializedThreads() map[string]NativeThread {
	c.mu.Lock()
	defer c.mu.Unlock()
	threads := make(map[string]NativeThread, len(c.persisted))
	for _, value := range c.persisted {
		thread := NativeThread{ID: value.Mapping.ThreadID, Status: "notLoaded"}
		if value.CreatedThread != nil {
			thread = *value.CreatedThread
		}
		threads[thread.ID] = thread
	}
	return threads
}

// Observe records the public session identity associated with one mapping.
func (c *Controls) Observe(session Session, mapping Mapping) {
	c.mu.Lock()
	c.mappings[session.SessionID] = mapping
	c.mu.Unlock()
}

// Create starts one native thread. Current Codex has no creation correlation
// id, so a transport-ambiguous failure is returned rather than replayed.
func (c *Controls) Create(ctx context.Context, operationID, cwd string) (ControlSession, error) {
	var thread NativeThread
	for {
		c.mu.Lock()
		if value, found := c.created[operationID]; found {
			c.mu.Unlock()
			return value, nil
		}
		if existing, found := c.threads[operationID]; found {
			thread = existing
			c.mu.Unlock()
			break
		}
		if wait, found := c.creating[operationID]; found {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ControlSession{}, ctx.Err()
			}
		}
		native := c.native
		if native == nil {
			c.mu.Unlock()
			return ControlSession{}, errors.New("crew-codex control is unavailable")
		}
		wait := make(chan struct{})
		c.creating[operationID] = wait
		c.mu.Unlock()
		created, err := native.StartThread(ctx, cwd)
		c.mu.Lock()
		delete(c.creating, operationID)
		close(wait)
		if err == nil {
			c.threads[operationID] = created
		}
		c.mu.Unlock()
		if err != nil {
			return ControlSession{}, fmt.Errorf("native create outcome unknown: %w", err)
		}
		thread = created
		break
	}
	c.mu.Lock()
	native, lease := c.native, c.lease
	c.mu.Unlock()
	if native == nil || lease.LeaseToken == "" {
		return ControlSession{}, errors.New("crew-codex control is unavailable")
	}
	projector := Projector{Fabric: c.fabric, Native: native, AdapterID: c.cfg.AdapterID, Lease: lease, Capabilities: codexCapabilities, ClaimDuration: c.cfg.ClaimDuration}
	session, err := c.fabric.Adopt(ctx, AdoptRequest{AdapterID: c.cfg.AdapterID, LeaseToken: lease.LeaseToken, AdapterKey: thread.ID, Label: threadLabel(thread), Location: thread.CWD, Status: threadStatus(thread.Status), Capabilities: codexCapabilities})
	if err != nil {
		return ControlSession{}, fmt.Errorf("publish created thread: %w", err)
	}
	mapping := Mapping{Address: "codex/" + thread.ID, ThreadID: thread.ID}
	if _, err = projector.bind(ctx, mapping.Address, session); err != nil {
		return ControlSession{}, err
	}
	value := ControlSession{SessionID: session.SessionID, Label: session.Label, Status: session.Status}
	c.mu.Lock()
	next := make(map[string]persistedControlSession, len(c.persisted)+1)
	for operation, persisted := range c.persisted {
		next[operation] = persisted
	}
	createdThread := thread
	next[operationID] = persistedControlSession{Session: value, Mapping: mapping, ToolEnabled: true, CreatedThread: &createdThread}
	if err := saveControlState(c.cfg.StatePath, next); err != nil {
		c.mu.Unlock()
		return ControlSession{}, err
	}
	c.mappings[session.SessionID] = mapping
	c.created[operationID] = value
	c.persisted = next
	c.mu.Unlock()
	return value, nil
}

func (c *Controls) Interrupt(ctx context.Context, operationID, sessionID, turnID string) error {
	c.mu.Lock()
	if _, found := c.interrupted[operationID]; found {
		c.mu.Unlock()
		return nil
	}
	mapping, found := c.mappings[sessionID]
	native := c.native
	c.mu.Unlock()
	if !found || native == nil {
		return errors.New("Codex session is not controllable")
	}
	thread, err := native.ReadThread(ctx, mapping.ThreadID)
	if err != nil {
		return err
	}
	if !activeTurn(thread, turnID) {
		c.mu.Lock()
		c.interrupted[operationID] = struct{}{}
		c.mu.Unlock()
		return nil
	}
	if err = native.Interrupt(ctx, mapping.ThreadID, turnID); err != nil {
		read, readErr := native.ReadThread(ctx, mapping.ThreadID)
		if readErr != nil || activeTurn(read, turnID) {
			return fmt.Errorf("native interrupt outcome unknown: %w", err)
		}
	}
	c.mu.Lock()
	c.interrupted[operationID] = struct{}{}
	c.mu.Unlock()
	return nil
}

func (c *Controls) Interactions(sessionID string) []PendingInteraction {
	c.mu.Lock()
	mapping, found := c.mappings[sessionID]
	native := c.native
	c.mu.Unlock()
	if !found || native == nil {
		return nil
	}
	values := native.Interactions()
	out := make([]PendingInteraction, 0, len(values))
	for _, value := range values {
		if value.ThreadID == mapping.ThreadID {
			projected := publicInteraction(sessionID, value)
			if len(projected.AllowedDecisions) > 0 {
				out = append(out, projected)
			}
		}
	}
	return out
}

func publicInteraction(sessionID string, value NativeInteraction) PendingInteraction {
	out := PendingInteraction{ID: value.ID, SessionID: sessionID, Kind: value.Method, CreatedAt: value.CreatedAt.Format(time.RFC3339Nano), Status: "pending", Capability: "respond-interactions"}
	var params struct {
		Reason             string   `json:"reason"`
		Message            string   `json:"message"`
		AvailableDecisions []string `json:"availableDecisions"`
	}
	_ = json.Unmarshal(value.Params, &params)
	out.Prompt = params.Reason
	if out.Prompt == "" {
		out.Prompt = params.Message
	}
	out.AllowedDecisions = params.AvailableDecisions
	if len(out.AllowedDecisions) == 0 {
		switch value.Method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			out.AllowedDecisions = []string{"accept", "decline", "cancel"}
		case "item/permissions/requestApproval":
			// The protocol grants an explicit permission profile rather than a
			// decision enum; an empty profile is the browser-safe no-grant reply.
			out.AllowedDecisions = []string{"deny"}
			var permission struct {
				Permissions map[string]json.RawMessage `json:"permissions"`
			}
			if json.Unmarshal(value.Params, &permission) == nil {
				for name := range permission.Permissions {
					out.Permissions = append(out.Permissions, name)
				}
			}
		case "item/tool/requestUserInput":
			var input struct {
				Questions []struct {
					ID       string              `json:"id"`
					Header   string              `json:"header"`
					Question string              `json:"question"`
					IsSecret bool                `json:"isSecret"`
					Options  []InteractionOption `json:"options"`
				} `json:"questions"`
			}
			if json.Unmarshal(value.Params, &input) == nil {
				for _, question := range input.Questions {
					if question.ID != "" && question.Header != "" && question.Question != "" {
						out.Questions = append(out.Questions, InteractionQuestion{ID: question.ID, Header: question.Header, Question: question.Question, Sensitive: question.IsSecret, Options: question.Options})
					}
				}
			}
			if len(out.Questions) > 0 {
				out.AllowedDecisions = []string{"answer"}
			}
		case "mcpServer/elicitation/request":
			out.AllowedDecisions = []string{"decline", "cancel"}
		}
	}
	return out
}

func (c *Controls) Respond(ctx context.Context, sessionID, id, method string, response json.RawMessage) error {
	c.mu.Lock()
	mapping, found := c.mappings[sessionID]
	native := c.native
	c.mu.Unlock()
	if !found || native == nil {
		return errors.New("Codex session is not controllable")
	}
	if !validInteractionResponse(method, response) {
		return errors.New("invalid Codex interaction response")
	}
	matched := false
	for _, pending := range native.Interactions() {
		if pending.ID == id && pending.Method == method && pending.ThreadID == mapping.ThreadID {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("Codex interaction does not belong to this session")
	}
	return native.RespondInteraction(ctx, id, method, response)
}

func activeTurn(thread NativeThread, turnID string) bool {
	for _, turn := range thread.Turns {
		if turn.ID == turnID {
			return turn.Status == "inProgress" || turn.Status == "active"
		}
	}
	return false
}
func validInteractionResponse(method string, response json.RawMessage) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(response, &value) != nil {
		return false
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var v struct {
			Decision string `json:"decision"`
		}
		return json.Unmarshal(response, &v) == nil && (v.Decision == "accept" || v.Decision == "decline" || v.Decision == "cancel")
	case "item/permissions/requestApproval":
		var v struct {
			Permissions map[string]json.RawMessage `json:"permissions"`
			Scope       string                     `json:"scope"`
		}
		return json.Unmarshal(response, &v) == nil && v.Permissions != nil && (v.Scope == "turn" || v.Scope == "session")
	case "item/tool/requestUserInput":
		var v struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		}
		if json.Unmarshal(response, &v) != nil || v.Answers == nil {
			return false
		}
		for _, answer := range v.Answers {
			if answer.Answers == nil {
				return false
			}
		}
		return true
	case "mcpServer/elicitation/request":
		var v struct {
			Action  string          `json:"action"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(response, &v) != nil || (v.Action != "accept" && v.Action != "decline" && v.Action != "cancel") {
			return false
		}
		return v.Action != "accept" || len(v.Content) > 0
	default:
		return false
	}
}
