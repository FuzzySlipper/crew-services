package dshdriver

import (
	"encoding/json"
	"io"
	"net/http"

	"crew-services/internal/crewcli"
)

// Handler is the driver's loopback API for crew-dsh send, who and address.
func Handler(d *Driver) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": d.Sessions()})
	})
	mux.HandleFunc("GET /v1/sessions/{sessionID}/address", func(w http.ResponseWriter, r *http.Request) {
		address, err := d.Address(r.Context(), r.PathValue("sessionID"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
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
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
