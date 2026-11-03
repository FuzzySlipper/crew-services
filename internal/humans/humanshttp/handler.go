// Package humanshttp is crew-humans' loopback HTTP API: a thin layer over
// humans.Service (docs/crew-humans.md, The notify contract).
package humanshttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"crew-services/internal/humans"
)

func NewHandler(svc *humans.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/people", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"site": svc.Site(), "people": svc.People()})
	})
	mux.HandleFunc("GET /v1/people/{name}", func(w http.ResponseWriter, r *http.Request) {
		person, err := svc.GetPerson(r.PathValue("name"))
		reply(w, person, err)
	})
	mux.HandleFunc("POST /v1/notices", func(w http.ResponseWriter, r *http.Request) {
		var request humans.NotifyRequest
		if !decode(w, r, &request) {
			return
		}
		result, replayed, err := svc.Notify(r.Context(), request)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	})
	mux.HandleFunc("GET /v1/notices", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("limit"))
		inbox, err := svc.Notices(r.Context(), q.Get("person"), q.Get("state") == "unread", q.Get("before"), n)
		reply(w, inbox, err)
	})
	mux.HandleFunc("GET /v1/notices/{id}", func(w http.ResponseWriter, r *http.Request) {
		notice, err := svc.GetNotice(r.Context(), r.PathValue("id"))
		reply(w, notice, err)
	})
	mux.HandleFunc("POST /v1/notices/read", func(w http.ResponseWriter, r *http.Request) {
		var request humans.MarkReadRequest
		if !decode(w, r, &request) {
			return
		}
		marked, err := svc.MarkRead(r.Context(), request)
		reply(w, map[string]int{"marked": marked}, err)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(humans.CodeInvalid), "error": "invalid JSON"})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeError(w http.ResponseWriter, err error) {
	var typed *humans.Error
	if !errors.As(err, &typed) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "internal_error", "error": err.Error()})
		return
	}
	status := http.StatusBadRequest
	switch typed.Code {
	case humans.CodeUnknownPerson, humans.CodeNotFound:
		status = http.StatusNotFound
	case humans.CodeConflict:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"code": string(typed.Code), "error": typed.Err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
