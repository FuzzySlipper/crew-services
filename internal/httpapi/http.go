// Package httpapi exposes the small local JSON/HTTP boundary.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"crew-services/internal/service"
)

// NewHandler constructs the HTTP boundary around a typed service.
func NewHandler(svc *service.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", readiness(svc))
	mux.HandleFunc("GET /readyz", readiness(svc))
	mux.HandleFunc("POST /v1/adapters/register", registerAdapter(svc))
	mux.HandleFunc("POST /v1/adapters/renew", renewAdapter(svc))
	mux.HandleFunc("PUT /v1/addresses/{address}/binding", putBinding(svc))
	mux.HandleFunc("DELETE /v1/addresses/{address}/binding", unbind(svc))
	mux.HandleFunc("GET /v1/addresses/{address}", resolve(svc))
	mux.HandleFunc("GET /v1/addresses", list(svc))
	mux.HandleFunc("POST /v1/sessions/adopt", adoptSession(svc))
	mux.HandleFunc("GET /v1/sessions", listSessions(svc))
	mux.HandleFunc("GET /v1/sessions/{sessionID}", getSession(svc))
	mux.HandleFunc("PUT /v1/sessions/{sessionID}", updateSession(svc))
	mux.HandleFunc("POST /v1/sessions/{sessionID}/events", appendSessionEvent(svc))
	mux.HandleFunc("GET /v1/session-events", listSessionEvents(svc))
	mux.HandleFunc("GET /v1/session-events/stream", streamSessionEvents(svc))
	mux.HandleFunc("POST /v1/messages", submitMessage(svc))
	mux.HandleFunc("POST /v1/rounds", beginRound(svc))
	mux.HandleFunc("GET /v1/rounds", listRounds(svc))
	mux.HandleFunc("GET /v1/rounds/{roundID}", getRound(svc))
	mux.HandleFunc("POST /v1/rounds/{roundID}/cancel", cancelRound(svc))
	mux.HandleFunc("GET /v1/rounds/{roundID}/wait", waitRound(svc))
	mux.HandleFunc("GET /v1/traffic", traffic(svc))
	mux.HandleFunc("GET /v1/messages", listMessages(svc))
	mux.HandleFunc("GET /v1/messages/{messageID}", getMessage(svc))
	mux.HandleFunc("GET /v1/deliveries", listDeliveries(svc))
	mux.HandleFunc("GET /v1/deliveries/{deliveryID}", getDelivery(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/cancel", cancelDelivery(svc))
	mux.HandleFunc("POST /v1/deliveries/claim", claimDelivery(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/renew", renewClaim(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/release", releaseDelivery(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/begin-dispatch", beginDispatch(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/acknowledge", reconcile(svc, "ack"))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/fail", failDelivery(svc))
	mux.HandleFunc("POST /v1/deliveries/{deliveryID}/outcome-unknown", reconcile(svc, "unknown"))
	mux.HandleFunc("POST /v1/maintenance/reap", reap(svc))
	mux.HandleFunc("GET /v1/mailbox/{address}", mailbox(svc))
	mux.HandleFunc("GET /v1/mailbox/{address}/head", mailboxHead(svc))
	return mux
}

// reap exposes the intentionally explicit, trusted-loopback settlement pass.
// It is not a scheduler: callers choose when to recover stale local work.
func reap(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deliveries, err := svc.SettlePending(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		rounds, err := svc.SettleRounds(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries, "rounds": rounds})
	}
}

type adapterRequest struct {
	AdapterID          string `json:"adapter_id"`
	InstanceID         string `json:"instance_id,omitempty"`
	LeaseToken         string `json:"lease_token,omitempty"`
	PreviousLeaseToken string `json:"previous_lease_token,omitempty"`
	LeaseDuration      string `json:"lease_duration"`
}

type bindingRequest struct {
	ActorAdapterID   string   `json:"actor_adapter_id"`
	LeaseToken       string   `json:"lease_token"`
	AdapterID        string   `json:"adapter_id"`
	TargetRef        string   `json:"target_ref"`
	Capabilities     []string `json:"capabilities"`
	ExpectedRevision *int64   `json:"expected_revision,omitempty"`
}

type unbindRequest struct {
	ActorAdapterID   string `json:"actor_adapter_id"`
	LeaseToken       string `json:"lease_token"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type messageRequest struct {
	ProducerID                  string `json:"producer_id"`
	LeaseToken                  string `json:"lease_token"`
	OperationID                 string `json:"operation_id"`
	SenderAddress               string `json:"sender_address"`
	RecipientAddress            string `json:"recipient_address"`
	Body                        string `json:"body"`
	CorrelationID               string `json:"correlation_id,omitempty"`
	ReplyToMessageID            string `json:"reply_to_message_id,omitempty"`
	ActivationPolicy            string `json:"activation_policy,omitempty"`
	TTL                         string `json:"ttl"`
	ExpectedSenderGeneration    *int64 `json:"expected_sender_generation,omitempty"`
	ExpectedRecipientGeneration *int64 `json:"expected_recipient_generation,omitempty"`
}

func registerAdapter(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body adapterRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		duration, err := time.ParseDuration(body.LeaseDuration)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid lease_duration")})
			return
		}
		lease, err := svc.RegisterAdapter(r.Context(), service.RegisterAdapterRequest{AdapterID: body.AdapterID, InstanceID: body.InstanceID, PreviousLeaseToken: body.PreviousLeaseToken, LeaseDuration: duration})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, lease)
	}
}

func renewAdapter(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body adapterRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		duration, err := time.ParseDuration(body.LeaseDuration)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid lease_duration")})
			return
		}
		lease, err := svc.RenewAdapter(r.Context(), service.RenewAdapterRequest{AdapterID: body.AdapterID, LeaseToken: body.LeaseToken, LeaseDuration: duration})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lease)
	}
}

func putBinding(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body bindingRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		binding, err := svc.PutBinding(r.Context(), service.PutBindingRequest{Address: r.PathValue("address"), ActorAdapterID: body.ActorAdapterID, LeaseToken: body.LeaseToken, AdapterID: body.AdapterID, TargetRef: body.TargetRef, Capabilities: body.Capabilities, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if body.ExpectedRevision == nil {
			status = http.StatusCreated
		}
		writeJSON(w, status, binding)
	}
}

func unbind(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body unbindRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		binding, err := svc.Unbind(r.Context(), service.UnbindRequest{Address: r.PathValue("address"), ActorAdapterID: body.ActorAdapterID, LeaseToken: body.LeaseToken, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, binding)
	}
}

func resolve(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binding, err := svc.Resolve(r.Context(), r.PathValue("address"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, binding)
	}
}

func list(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bindings, err := svc.List(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"addresses": bindings})
	}
}

func submitMessage(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body messageRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		ttl, err := time.ParseDuration(body.TTL)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid ttl")})
			return
		}
		result, err := svc.SubmitMessage(r.Context(), service.SubmitMessageRequest{ProducerID: body.ProducerID, LeaseToken: body.LeaseToken, OperationID: body.OperationID, SenderAddress: body.SenderAddress, RecipientAddress: body.RecipientAddress, Body: body.Body, CorrelationID: body.CorrelationID, ReplyToMessageID: body.ReplyToMessageID, ActivationPolicy: body.ActivationPolicy, TTL: ttl, ExpectedSenderGeneration: body.ExpectedSenderGeneration, ExpectedRecipientGeneration: body.ExpectedRecipientGeneration})
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

func getMessage(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := svc.GetMessage(r.Context(), r.PathValue("messageID"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	}
}
func listMessages(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := svc.ListMessages(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": values})
	}
}
func getDelivery(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := svc.GetDelivery(r.Context(), r.PathValue("deliveryID"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	}
}
func listDeliveries(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := svc.ListDeliveries(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": values})
	}
}
func cancelDelivery(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := svc.CancelDelivery(r.Context(), r.PathValue("deliveryID"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	}
}
func mailbox(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, err := svc.Mailbox(r.Context(), r.PathValue("address"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": values})
	}
}
func mailboxHead(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		generation, err := parseGeneration(r)
		if err != nil {
			writeError(w, err)
			return
		}
		value, err := svc.HeadDelivery(r.Context(), r.PathValue("address"), generation)
		if err != nil {
			writeError(w, err)
			return
		}
		if value == nil {
			writeJSON(w, http.StatusOK, map[string]any{"delivery": nil})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delivery": value})
	}
}

func parseGeneration(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("generation")
	if value == "" {
		return 0, &service.Error{Code: service.CodeInvalid, Err: errors.New("generation is required")}
	}
	generation, err := strconv.ParseInt(value, 10, 64)
	if err != nil || generation <= 0 {
		return 0, &service.Error{Code: service.CodeInvalid, Err: errors.New("generation must be positive")}
	}
	return generation, nil
}

func readiness(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := svc.Ready(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid JSON request")})
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid JSON request")})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	var typed *service.Error
	if !errors.As(err, &typed) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "internal_error"})
		return
	}
	status := http.StatusBadRequest
	switch typed.Code {
	case service.CodeNotFound, service.CodeReplyOriginalNotFound:
		status = http.StatusNotFound
	case service.CodeNotBound:
		status = http.StatusConflict
	case service.CodeConflict, service.CodeOperationConflict, service.CodeStaleRevision, service.CodeLeaseExpired, service.CodeLeaseFenced, service.CodeClaimFenced, service.CodeAdapterMismatch, service.CodeReplySenderMismatch, service.CodeReplyRecipientMismatch, service.CodeReplyGenerationMismatch:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"code": string(typed.Code)})
}
