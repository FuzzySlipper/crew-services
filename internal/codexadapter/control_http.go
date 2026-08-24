package codexadapter

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// ServeControls exposes the adapter-owned loopback operator surface. The DSH
// plugin proxies this endpoint so browser code never reaches it directly.
func ServeControls(ctx context.Context, listen string, controls *Controls) error {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: controlHandler(controls)}
	go func() { <-ctx.Done(); _ = server.Shutdown(context.Background()) }()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func controlHandler(controls *Controls) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/controls/capabilities", func(w http.ResponseWriter, r *http.Request) {
		controlWrite(w, map[string]any{"capabilities": controls.Capabilities()})
	})
	mux.HandleFunc("POST /v1/controls/threads", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OperationID string `json:"operation_id"`
			CWD         string `json:"cwd"`
		}
		if !controlJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.OperationID) == "" {
			controlError(w, "operation_id is required")
			return
		}
		value, err := controls.Create(r.Context(), body.OperationID, body.CWD)
		if err != nil {
			controlFailure(w, err)
			return
		}
		controlWrite(w, value)
	})
	mux.HandleFunc("GET /v1/controls/interactions", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session_id")
		if id == "" {
			controlError(w, "session_id is required")
			return
		}
		controlWrite(w, map[string]any{"interactions": controls.Interactions(id)})
	})
	mux.HandleFunc("POST /v1/controls/interrupt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OperationID string `json:"operation_id"`
			SessionID   string `json:"session_id"`
			TurnID      string `json:"turn_id"`
		}
		if !controlJSON(w, r, &body) {
			return
		}
		if body.OperationID == "" || body.SessionID == "" || body.TurnID == "" {
			controlError(w, "operation_id, session_id, and turn_id are required")
			return
		}
		if err := controls.Interrupt(r.Context(), body.OperationID, body.SessionID, body.TurnID); err != nil {
			controlFailure(w, err)
			return
		}
		controlWrite(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/controls/interactions/respond", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string          `json:"session_id"`
			ID        string          `json:"id"`
			Method    string          `json:"method"`
			Response  json.RawMessage `json:"response"`
		}
		if !controlJSON(w, r, &body) {
			return
		}
		if body.SessionID == "" || body.ID == "" || body.Method == "" {
			controlError(w, "session_id, id, and method are required")
			return
		}
		if err := controls.Respond(r.Context(), body.SessionID, body.ID, body.Method, body.Response); err != nil {
			controlFailure(w, err)
			return
		}
		controlWrite(w, map[string]bool{"ok": true})
	})
	return mux
}
func controlJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Header.Get("content-type") != "" && !strings.Contains(r.Header.Get("content-type"), "application/json") {
		controlError(w, "application/json is required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if json.NewDecoder(r.Body).Decode(target) != nil {
		controlError(w, "invalid control request")
		return false
	}
	return true
}
func controlWrite(w http.ResponseWriter, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
func controlError(w http.ResponseWriter, text string) {
	w.WriteHeader(http.StatusBadRequest)
	controlWrite(w, map[string]string{"error": text})
}
func controlFailure(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusConflict)
	controlWrite(w, map[string]string{"error": err.Error()})
}
