package agydriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"crew-services/internal/crewcli"
)

// Handler is the driver's loopback API for its hooks and the crew commands.
func Handler(d *Driver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Every hook registers (or refreshes) its conversation, because agy runs
	// SessionStart only for a new conversation, not a resumed one.
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body registrationJSON
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		reach, err := d.RegisterSession(r.Context(), body.registration())
		if errors.Is(err, ErrSubagent) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"address": reach.Address, "wakeable": reach.Wakeable, "seats": reach.Seats})
	})
	mux.HandleFunc("POST /v1/sessions/{conversationID}/state", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Busy bool `json:"busy"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		d.SetBusy(r.PathValue("conversationID"), body.Busy)
		w.WriteHeader(http.StatusNoContent)
	})
	// Called by the PreInvocation hook, which runs only inside a turn. The
	// answer's context, when not empty, is a crew message for the hook to
	// inject as a system message.
	mux.HandleFunc("POST /v1/sessions/{conversationID}/turn-context", func(w http.ResponseWriter, r *http.Request) {
		text, err := d.TurnContext(r.Context(), r.PathValue("conversationID"))
		if err != nil {
			d.log.Warn("turn context", "conversation", r.PathValue("conversationID"), "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"context": text})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": d.Sessions()})
	})
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		var body crewcli.SendRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 512*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		result, err := d.Send(r.Context(), body)
		if err != nil {
			crewcli.WriteDriverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /v1/seat-write", crewcli.ServeSeatWrite(d.SeatWrite))
	// crewcli.ServeModel reads the {sessionID} path value.
	mux.HandleFunc("GET /v1/sessions/{sessionID}/model", crewcli.ServeModel(func(r *http.Request, id string) string { return d.Model(id) }))
	mux.HandleFunc("GET /v1/sessions/{conversationID}/address", func(w http.ResponseWriter, r *http.Request) {
		address, ok := d.Address(r.PathValue("conversationID"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
	})
	return mux
}

type registrationJSON struct {
	ConversationID string `json:"conversation_id"`
	PID            int    `json:"pid"`
	Start          int64  `json:"start"`
	Hub            bool   `json:"hub,omitempty"`
	Cwd            string `json:"cwd"`
	Transcript     string `json:"transcript"`
}

func (r registrationJSON) registration() Registration {
	return Registration{ConversationID: r.ConversationID, Proc: Proc{PID: r.PID, Start: r.Start, Hub: r.Hub}, Cwd: r.Cwd, Transcript: r.Transcript}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// HookInput is the part of agy's hook stdin the driver reads. agy encodes
// hook payloads as protojson, so keys are camelCase.
type HookInput struct {
	ConversationID string `json:"conversationId"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcriptPath"`
	HookEventName  string `json:"hookEventName"`
	ToolCall       struct {
		Name string                     `json:"name"`
		Args map[string]json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// HookOptions are the hook's own settings, from its command line.
type HookOptions struct {
	// DenServer is the name of the Den MCP server in agy's MCP config, whose
	// submit_task_for_review gets this conversation's notify_address.
	DenServer string
}

// RunHook handles one hook event and returns what to print on stdout. It never
// fails the hook: when the driver is unreachable it returns nothing, which agy
// reads as "no opinion" for every event. (An empty JSON object would not do:
// agy treats a PreToolUse answer without a decision as a denial.)
func RunHook(ctx context.Context, driverURL, event string, in HookInput, proc Proc, opts HookOptions) []byte {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if in.ConversationID == "" {
		return nil
	}
	client := hookClient{base: driverURL}
	register := func() (struct {
		Address  string   `json:"address"`
		Wakeable bool     `json:"wakeable"`
		Seats    []string `json:"seats"`
	}, error) {
		var out struct {
			Address  string   `json:"address"`
			Wakeable bool     `json:"wakeable"`
			Seats    []string `json:"seats"`
		}
		err := client.do(ctx, http.MethodPost, "/v1/sessions", registrationJSON{ConversationID: in.ConversationID, PID: proc.PID, Start: proc.Start, Hub: proc.Hub, Cwd: in.Cwd, Transcript: in.TranscriptPath}, &out)
		return out, err
	}
	switch event {
	case "session-start":
		out, err := register()
		if err != nil || out.Address == "" {
			return nil
		}
		return injectSystemMessage(SessionContext(out.Address, out.Wakeable, out.Seats, crewcli.PersonFor(in.ConversationID)))
	case "step":
		// PreInvocation runs before each model call of a running turn, so a
		// message it injects can never start one.
		if _, err := register(); err != nil {
			return nil
		}
		var out struct {
			Context string `json:"context"`
		}
		if err := client.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(in.ConversationID)+"/turn-context", map[string]string{}, &out); err != nil || out.Context == "" {
			return nil
		}
		return injectSystemMessage(out.Context)
	case "idle":
		if _, err := register(); err != nil {
			return nil
		}
		_ = client.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(in.ConversationID)+"/state", map[string]bool{"busy": false}, nil)
		return nil
	case "fill-notify":
		return fillNotify(ctx, client, in, opts)
	}
	return nil
}

// SessionContext is what a conversation is told when it starts: its crew
// address, how to use the board and seats, and who it works for.
func SessionContext(address string, wakeable bool, seats []string, person string) string {
	text := fmt.Sprintf("Your crew messaging address is %s. Crew services (for example crew-review, when a review check gate fails) send messages to this conversation there; pass it as notify_address where a tool accepts one. Send crew messages and list sessions with `crew-agy send` and `crew-agy who`.", address)
	if !wakeable {
		text += " This agy process was not started with the crew token (ANTIGRAVITY_SERVER_CSRF_TOKEN), so crew messages reach this conversation only while it is working, never by starting a turn."
	}
	return text + crewcli.BoardContext("crew-agy") + crewcli.SeatContext("crew-agy", seats) + crewcli.PersonContext(person)
}

// fillNotify adds this conversation's address as notify_address to a Den
// submit_task_for_review call that has none. agy routes MCP tools through
// call_mcp_tool {ServerName, ToolName, Arguments}; a PreToolUse overwrite
// replaces top-level arguments, so the whole Arguments object goes back. The
// decision is "ask", which leaves the call to agy's own permission rules (an
// always-allowed call still runs without a prompt); printing nothing for every
// other call leaves those untouched.
func fillNotify(ctx context.Context, client hookClient, in HookInput, opts HookOptions) []byte {
	if in.ToolCall.Name != "call_mcp_tool" {
		return nil
	}
	var server, tool string
	_ = json.Unmarshal(in.ToolCall.Args["ServerName"], &server)
	_ = json.Unmarshal(in.ToolCall.Args["ToolName"], &tool)
	denServer := opts.DenServer
	if denServer == "" {
		denServer = "den"
	}
	if server != denServer || tool != "submit_task_for_review" {
		return nil
	}
	arguments := map[string]any{}
	if raw := in.ToolCall.Args["Arguments"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
			return nil
		}
	}
	if existing, ok := arguments["notify_address"].(string); ok && existing != "" {
		return nil
	}
	var out struct {
		Address string `json:"address"`
	}
	if err := client.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(in.ConversationID)+"/address", nil, &out); err != nil || out.Address == "" {
		return nil
	}
	arguments["notify_address"] = out.Address
	data, _ := json.Marshal(map[string]any{"decision": "ask", "overwrite": map[string]any{"Arguments": arguments}})
	return data
}

// injectSystemMessage is a SessionStart or PreInvocation answer that adds text
// to the conversation as a system message.
func injectSystemMessage(text string) []byte {
	data, _ := json.Marshal(map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": text}}}})
	return data
}

type hookClient struct{ base string }

func (c hookClient) do(ctx context.Context, method, path string, body, target any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errors.New(resp.Status)
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
