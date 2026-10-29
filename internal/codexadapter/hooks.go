package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"crew-services/internal/crewcli"
)

// Handler is the driver's loopback API for its Codex hooks.
func Handler(d *Driver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/threads", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		address, err := d.RegisterThread(r.Context(), body.ThreadID)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
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
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /v1/threads/{threadID}/address", func(w http.ResponseWriter, r *http.Request) {
		address, ok := d.Address(r.PathValue("threadID"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown thread"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// HookInput is the part of a Codex hook's stdin the driver reads. Codex's
// session_id is the thread id.
type HookInput struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// RunHook handles one hook event and returns what to print on stdout. It never
// fails the hook: when the driver is unreachable it returns nothing.
func RunHook(ctx context.Context, driverURL, event string, in HookInput) []byte {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	client := hookClient{base: driverURL}
	switch event {
	case "session-start":
		if in.SessionID == "" {
			return nil
		}
		var out struct {
			Address string `json:"address"`
		}
		if err := client.do(ctx, http.MethodPost, "/v1/threads", map[string]string{"thread_id": in.SessionID}, &out); err != nil || out.Address == "" {
			return nil
		}
		return hookOutput("SessionStart", map[string]any{"additionalContext": fmt.Sprintf("Your crew messaging address is %s. Crew services (for example crew-review, when a review check gate fails) send messages to this thread there; pass it as notify_address where a tool accepts one.", out.Address)})
	case "fill-notify":
		return fillNotify(ctx, client, in)
	}
	return nil
}

// fillNotify adds this thread's address as notify_address to an MCP tool call
// that has none. Codex 0.163 applies PreToolUse updatedInput to MCP tools.
func fillNotify(ctx context.Context, client hookClient, in HookInput) []byte {
	if in.SessionID == "" || len(in.ToolInput) == 0 {
		return nil
	}
	var input map[string]any
	if err := json.Unmarshal(in.ToolInput, &input); err != nil || input == nil {
		return nil
	}
	if existing, ok := input["notify_address"].(string); ok && existing != "" {
		return nil
	}
	var out struct {
		Address string `json:"address"`
	}
	if err := client.do(ctx, http.MethodGet, "/v1/threads/"+url.PathEscape(in.SessionID)+"/address", nil, &out); err != nil || out.Address == "" {
		return nil
	}
	input["notify_address"] = out.Address
	return hookOutput("PreToolUse", map[string]any{"permissionDecision": "allow", "updatedInput": input})
}

func hookOutput(event string, fields map[string]any) []byte {
	fields["hookEventName"] = event
	data, _ := json.Marshal(map[string]any{"hookSpecificOutput": fields})
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("crew-codex returned %s", resp.Status)
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
