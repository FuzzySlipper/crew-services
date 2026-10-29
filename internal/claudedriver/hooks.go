package claudedriver

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
)

// Handler is the driver's loopback API for its own hooks.
func Handler(d *Driver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"session_id"`
			Socket    string `json:"socket"`
			Token     string `json:"token"`
			Cwd       string `json:"cwd"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		address, err := d.RegisterSession(r.Context(), body.SessionID, body.Socket, body.Token, body.Cwd)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
	})
	mux.HandleFunc("POST /v1/sessions/{sessionID}/state", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Busy bool `json:"busy"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		d.SetBusy(r.PathValue("sessionID"), body.Busy)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/sessions/{sessionID}/address", func(w http.ResponseWriter, r *http.Request) {
		address, ok := d.Address(r.PathValue("sessionID"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
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

// HookInput is the part of Claude Code's hook stdin the driver reads.
type HookInput struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// HookEnv carries the session's own inbox, which Claude Code exports to hooks.
type HookEnv struct {
	Socket string
	Token  string
}

// RunHook handles one hook event and returns what to print on stdout. It never
// fails the hook: when the driver is unreachable it returns nothing, so a
// stopped driver costs a session only the connect timeout.
func RunHook(ctx context.Context, driverURL, event string, in HookInput, env HookEnv) []byte {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	client := hookClient{base: driverURL}
	switch event {
	case "session-start":
		if in.SessionID == "" || env.Socket == "" {
			return nil
		}
		var out struct {
			Address string `json:"address"`
		}
		if err := client.do(ctx, http.MethodPost, "/v1/sessions", map[string]string{"session_id": in.SessionID, "socket": env.Socket, "token": env.Token, "cwd": in.Cwd}, &out); err != nil || out.Address == "" {
			return nil
		}
		return hookOutput("SessionStart", map[string]any{"additionalContext": fmt.Sprintf("Your crew messaging address is %s. Crew services (for example crew-review, when a review check gate fails) send messages to this session there; pass it as notify_address where a tool accepts one.", out.Address)})
	case "busy", "idle":
		if in.SessionID == "" {
			return nil
		}
		_ = client.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(in.SessionID)+"/state", map[string]bool{"busy": event == "busy"}, nil)
		return nil
	case "fill-notify":
		return fillNotify(ctx, client, in)
	}
	return nil
}

// fillNotify adds this session's address as notify_address to a tool call
// that has none, leaving every other argument as Claude wrote it.
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
	if err := client.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(in.SessionID)+"/address", nil, &out); err != nil || out.Address == "" {
		return nil
	}
	input["notify_address"] = out.Address
	return hookOutput("PreToolUse", map[string]any{"updatedInput": input})
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
