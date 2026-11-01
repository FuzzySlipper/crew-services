package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"crew-services/internal/service"
	"crew-services/internal/store"
)

func registerSeatRoutes(mux *http.ServeMux, svc *service.Service) {
	mux.HandleFunc("GET /v1/seats", listSeats(svc))
	mux.HandleFunc("GET /v1/seats/{seat}", getSeat(svc))
	mux.HandleFunc("GET /v1/seats/{seat}/holdings", listSeatHoldings(svc))
	mux.HandleFunc("POST /v1/seats/{seat}/take", seatWrite(svc.TakeSeat))
	mux.HandleFunc("POST /v1/seats/{seat}/leave", seatWrite(svc.LeaveSeat))
	mux.HandleFunc("POST /v1/seats/{seat}/relieve", seatWrite(svc.RelieveSeat))
	mux.HandleFunc("POST /v1/seats/{seat}/retire", seatWrite(svc.RetireSeat))
	for _, kind := range []string{store.SeatTextCharter, store.SeatTextHandoff} {
		mux.HandleFunc("PUT /v1/seats/{seat}/"+kind, setSeatText(svc, kind))
		mux.HandleFunc("GET /v1/seats/{seat}/"+kind, getSeatText(svc, kind))
		mux.HandleFunc("GET /v1/seats/{seat}/"+kind+"/versions", listSeatTexts(svc, kind))
	}
}

var errInvalidVersion = errors.New("version must be a positive integer")

type seatRequest struct {
	AdapterID      string `json:"adapter_id"`
	LeaseToken     string `json:"lease_token"`
	OperationID    string `json:"operation_id"`
	SessionAddress string `json:"session_address"`
	Note           string `json:"note,omitempty"`
}

type seatTextRequest struct {
	seatRequest
	Body            string `json:"body"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
}

func (b seatRequest) service(seat string) service.SeatRequest {
	return service.SeatRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, SessionAddress: b.SessionAddress, SeatAddress: seat, Note: b.Note}
}

func writeSeatResult(w http.ResponseWriter, result service.SeatOperationResult, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func seatWrite(operation func(ctx context.Context, request service.SeatRequest) (service.SeatOperationResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body seatRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		result, err := operation(r.Context(), body.service(r.PathValue("seat")))
		writeSeatResult(w, result, err)
	}
}

func setSeatText(svc *service.Service, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body seatTextRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		result, err := svc.SetSeatText(r.Context(), service.SetSeatTextRequest{SeatRequest: body.seatRequest.service(r.PathValue("seat")), Kind: kind, Body: body.Body, ExpectedVersion: body.ExpectedVersion})
		writeSeatResult(w, result, err)
	}
}

func listSeats(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		values, err := svc.ListSeats(r.Context(), store.SeatFilter{Scope: query.Get("scope"), Holder: query.Get("holder"), State: query.Get("state")})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"seats": values})
	}
}

func getSeat(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := svc.GetSeat(r.Context(), r.PathValue("seat"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	}
}

func getSeatText(svc *service.Service, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var version int64
		if raw := r.URL.Query().Get("version"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				writeError(w, &service.Error{Code: service.CodeInvalid, Err: errInvalidVersion})
				return
			}
			version = parsed
		}
		value, err := svc.GetSeatText(r.Context(), r.PathValue("seat"), kind, version)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	}
}

func listSeatTexts(svc *service.Service, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := svc.ListSeatTexts(r.Context(), r.PathValue("seat"), kind)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": values})
	}
}

func listSeatHoldings(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := svc.ListSeatHoldings(r.Context(), r.PathValue("seat"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"holdings": values})
	}
}
