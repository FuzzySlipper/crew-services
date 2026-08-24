package httpapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"crew-services/internal/service"
)

const maxSessionRequestBytes int64 = 1 << 20

type adoptSessionRequest struct {
	AdapterID    string   `json:"adapter_id"`
	LeaseToken   string   `json:"lease_token"`
	AdapterKey   string   `json:"adapter_key"`
	Label        string   `json:"label"`
	Location     string   `json:"location,omitempty"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}
type updateSessionRequest struct {
	AdapterID        string   `json:"adapter_id"`
	LeaseToken       string   `json:"lease_token"`
	ExpectedRevision int64    `json:"expected_revision"`
	Label            string   `json:"label"`
	Location         string   `json:"location,omitempty"`
	Status           string   `json:"status"`
	Capabilities     []string `json:"capabilities"`
}
type appendSessionEventRequest struct {
	AdapterID        string          `json:"adapter_id"`
	LeaseToken       string          `json:"lease_token"`
	ExpectedRevision int64           `json:"expected_revision"`
	OperationID      string          `json:"operation_id"`
	EventType        string          `json:"event_type"`
	Payload          json.RawMessage `json:"payload"`
	OccurredAt       *time.Time      `json:"occurred_at,omitempty"`
}

func adoptSession(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body adoptSessionRequest
		if !decodeSessionJSON(w, r, &body) {
			return
		}
		v, err := svc.AdoptSession(r.Context(), service.AdoptSessionRequest{AdapterID: body.AdapterID, LeaseToken: body.LeaseToken, AdapterKey: body.AdapterKey, Label: body.Label, Location: body.Location, Status: body.Status, Capabilities: body.Capabilities})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func updateSession(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body updateSessionRequest
		if !decodeSessionJSON(w, r, &body) {
			return
		}
		v, err := svc.UpdateSession(r.Context(), service.UpdateSessionRequest{AdapterID: body.AdapterID, LeaseToken: body.LeaseToken, SessionID: r.PathValue("sessionID"), ExpectedRevision: body.ExpectedRevision, Label: body.Label, Location: body.Location, Status: body.Status, Capabilities: body.Capabilities})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func getSession(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := svc.GetSession(r.Context(), r.PathValue("sessionID"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func listSessions(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := queryLimit(r)
		if err != nil {
			writeError(w, err)
			return
		}
		v, err := svc.ListSessions(r.Context(), limit)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": v})
	}
}
func appendSessionEvent(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body appendSessionEventRequest
		if !decodeSessionJSON(w, r, &body) {
			return
		}
		occurred := time.Time{}
		if body.OccurredAt != nil {
			occurred = *body.OccurredAt
		}
		v, err := svc.AppendSessionEvent(r.Context(), service.AppendSessionEventRequest{AdapterID: body.AdapterID, LeaseToken: body.LeaseToken, SessionID: r.PathValue("sessionID"), ExpectedRevision: body.ExpectedRevision, OperationID: body.OperationID, EventType: body.EventType, Payload: body.Payload, OccurredAt: occurred})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, v)
	}
}
func listSessionEvents(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cursor, err := eventCursor(r)
		if err != nil {
			writeError(w, err)
			return
		}
		limit, err := queryLimit(r)
		if err != nil {
			writeError(w, err)
			return
		}
		v, err := svc.ListSessionEvents(r.Context(), r.URL.Query().Get("session_id"), cursor, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": v})
	}
}

// streamSessionEvents provides bounded replay followed by a small polling loop;
// it keeps the event log as the sole source of truth.
func streamSessionEvents(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cursor, err := eventCursor(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if last := strings.TrimSpace(r.Header.Get("Last-Event-ID")); last != "" {
			parsed, parseErr := strconv.ParseInt(last, 10, 64)
			if parseErr != nil || parsed < 0 {
				writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid Last-Event-ID")})
				return
			}
			if parsed > cursor {
				cursor = parsed
			}
		}
		limit, err := queryLimit(r)
		if err != nil {
			writeError(w, err)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, errors.New("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		writer := bufio.NewWriter(w)
		for {
			events, err := svc.ListSessionEvents(r.Context(), r.URL.Query().Get("session_id"), cursor, limit)
			if err != nil {
				return
			}
			for _, event := range events {
				encoded, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(writer, "id: %d\nevent: session_event\ndata: %s\n\n", event.Cursor, encoded)
				cursor = event.Cursor
			}
			if len(events) > 0 {
				_ = writer.Flush()
				flusher.Flush()
				continue
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func decodeSessionJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionRequestBytes)
	return decodeJSON(w, r, target)
}
func queryLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid limit")}
	}
	return parsed, nil
}
func eventCursor(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("cursor")
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid cursor")}
	}
	return parsed, nil
}
