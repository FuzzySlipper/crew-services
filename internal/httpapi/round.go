package httpapi

import (
	"errors"
	"net/http"
	"time"

	"crew-services/internal/service"
)

type roundRequest struct {
	ProducerID       string `json:"producer_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	SenderAddress    string `json:"sender_address"`
	RecipientAddress string `json:"recipient_address"`
	Body             string `json:"body"`
	CorrelationID    string `json:"correlation_id,omitempty"`
	ActivationPolicy string `json:"activation_policy,omitempty"`
	TTL              string `json:"ttl"`
	RoundTTL         string `json:"round_ttl"`
}

func beginRound(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body roundRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		ttl, err := time.ParseDuration(body.TTL)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid ttl")})
			return
		}
		roundTTL, err := time.ParseDuration(body.RoundTTL)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid round_ttl")})
			return
		}
		result, err := svc.BeginRound(r.Context(), service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: body.ProducerID, LeaseToken: body.LeaseToken, OperationID: body.OperationID, SenderAddress: body.SenderAddress, RecipientAddress: body.RecipientAddress, Body: body.Body, CorrelationID: body.CorrelationID, ActivationPolicy: body.ActivationPolicy, TTL: ttl}, RoundTTL: roundTTL})
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
}
func getRound(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := svc.GetRound(r.Context(), r.PathValue("roundID"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func listRounds(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := svc.ListRounds(r.Context())
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rounds": v})
	}
}
func cancelRound(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := svc.CancelRound(r.Context(), r.PathValue("roundID"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func waitRound(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var timeout time.Duration
		var err error
		if raw := r.URL.Query().Get("timeout"); raw != "" {
			timeout, err = time.ParseDuration(raw)
			if err != nil {
				writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid timeout")})
				return
			}
		}
		v, e := svc.WaitRound(r.Context(), service.WaitRoundRequest{RoundID: r.PathValue("roundID"), Timeout: timeout})
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func traffic(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := svc.Traffic(r.Context())
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
