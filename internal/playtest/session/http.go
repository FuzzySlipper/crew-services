package session

import (
	"context"
	"encoding/json"
	"net/http"
)

type Commander interface {
	Command(context.Context, Request) (any, error)
}

func (s *Service) Handler() http.Handler { return CommandHandler(s) }
func CommandHandler(s Commander) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
		decoder.DisallowUnknownFields()
		var request Request
		if err := decoder.Decode(&request); err != nil {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		result, err := s.Command(r.Context(), request)
		response := map[string]any{"ok": err == nil, "result": result}
		if err != nil {
			response["error"] = err.Error()
			w.WriteHeader(400)
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	return mux
}
