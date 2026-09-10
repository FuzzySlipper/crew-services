package target

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Handler exposes only the local command protocol. It never logs request bodies.
func Handler(controller *Controller) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			writeError(w, errors.New("Unknown route"))
			return
		}
		if r.ContentLength < 1 || r.ContentLength > 65536 {
			writeError(w, errors.New("body size must be an integer in [1, 65536]"))
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		decoder.UseNumber()
		request := map[string]any{}
		if err := decoder.Decode(&request); err != nil {
			writeError(w, err)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, errors.New("one JSON command is required"))
			return
		}
		result, err := controller.Dispatch(context.Background(), request)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	})
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
}
