package httpapi

import (
	"errors"
	"net/http"
	"time"

	"crew-services/internal/service"
)

type claimRequest struct {
	AdapterID           string `json:"adapter_id"`
	LeaseToken          string `json:"lease_token"`
	OperationID         string `json:"operation_id"`
	RecipientAddress    string `json:"recipient_address"`
	RecipientGeneration int64  `json:"recipient_generation"`
	Availability        string `json:"availability"`
	ClaimDuration       string `json:"claim_duration"`
}
type claimMutationRequest struct {
	AdapterID        string `json:"adapter_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	ClaimToken       string `json:"claim_token"`
	ClaimDuration    string `json:"claim_duration,omitempty"`
	NativeAttemptRef string `json:"native_attempt_ref,omitempty"`
}
type reconciliationRequest struct {
	AdapterID        string `json:"adapter_id"`
	LeaseToken       string `json:"lease_token"`
	OperationID      string `json:"operation_id"`
	NativeAttemptRef string `json:"native_attempt_ref"`
}

func claimDelivery(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body claimRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		d, err := time.ParseDuration(body.ClaimDuration)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid claim_duration")})
			return
		}
		result, err := svc.ClaimDelivery(r.Context(), service.ClaimDeliveryRequest{AdapterID: body.AdapterID, LeaseToken: body.LeaseToken, OperationID: body.OperationID, RecipientAddress: body.RecipientAddress, RecipientGeneration: body.RecipientGeneration, Availability: body.Availability, ClaimDuration: d})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
func renewClaim(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b claimMutationRequest
		if !decodeJSON(w, r, &b) {
			return
		}
		d, err := time.ParseDuration(b.ClaimDuration)
		if err != nil {
			writeError(w, &service.Error{Code: service.CodeInvalid, Err: errors.New("invalid claim_duration")})
			return
		}
		v, err := svc.RenewClaim(r.Context(), service.RenewClaimRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, DeliveryID: r.PathValue("deliveryID"), ClaimToken: b.ClaimToken, ClaimDuration: d})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func releaseDelivery(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b claimMutationRequest
		if !decodeJSON(w, r, &b) {
			return
		}
		v, err := svc.ReleaseDelivery(r.Context(), service.ReleaseDeliveryRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, DeliveryID: r.PathValue("deliveryID"), ClaimToken: b.ClaimToken})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func beginDispatch(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b claimMutationRequest
		if !decodeJSON(w, r, &b) {
			return
		}
		v, err := svc.BeginDispatch(r.Context(), service.BeginDispatchRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, DeliveryID: r.PathValue("deliveryID"), ClaimToken: b.ClaimToken, NativeAttemptRef: b.NativeAttemptRef})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func reconcile(svc *service.Service, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b reconciliationRequest
		if !decodeJSON(w, r, &b) {
			return
		}
		req := service.ReconcileDeliveryRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, DeliveryID: r.PathValue("deliveryID"), NativeAttemptRef: b.NativeAttemptRef}
		var v service.Delivery
		var err error
		switch kind {
		case "ack":
			v, err = svc.Acknowledge(r.Context(), req)
		case "unknown":
			v, err = svc.OutcomeUnknown(r.Context(), req)
		case "fail":
			v, err = svc.FailDelivery(r.Context(), service.FailDeliveryRequest{AdapterID: req.AdapterID, LeaseToken: req.LeaseToken, OperationID: req.OperationID, DeliveryID: req.DeliveryID, NativeAttemptRef: req.NativeAttemptRef})
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
func failDelivery(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b claimMutationRequest
		if !decodeJSON(w, r, &b) {
			return
		}
		req := service.FailDeliveryRequest{AdapterID: b.AdapterID, LeaseToken: b.LeaseToken, OperationID: b.OperationID, DeliveryID: r.PathValue("deliveryID"), ClaimToken: b.ClaimToken, NativeAttemptRef: b.NativeAttemptRef}
		v, err := svc.FailDelivery(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
