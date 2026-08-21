package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/store"
)

type Round = store.Round
type BeginRoundResult = store.BeginRoundResult

type BeginRoundRequest struct {
	Root     SubmitMessageRequest
	RoundTTL time.Duration
}

type WaitRoundRequest struct {
	RoundID string
	Timeout time.Duration
}
type WaitRoundResult struct {
	Round           Round `json:"round"`
	TimedOut        bool  `json:"timed_out"`
	ObservedExpired bool  `json:"observed_expired"`
}

const defaultRoundWait = 100 * time.Millisecond
const maxRoundWait = 5 * time.Second

func (s *Service) BeginRound(ctx context.Context, request BeginRoundRequest) (BeginRoundResult, error) {
	if request.RoundTTL <= 0 || request.RoundTTL > s.maxTTLDuration {
		return BeginRoundResult{}, invalid(fmt.Sprintf("round_ttl must be greater than zero and at most %s", s.maxTTLDuration))
	}
	root := request.Root
	for _, field := range []struct{ name, value string }{{"producer_id", root.ProducerID}, {"lease_token", root.LeaseToken}, {"operation_id", root.OperationID}, {"sender_address", root.SenderAddress}, {"recipient_address", root.RecipientAddress}} {
		if err := required(field.name, field.value); err != nil {
			return BeginRoundResult{}, err
		}
	}
	if strings.TrimSpace(root.Body) == "" {
		return BeginRoundResult{}, invalid("body is required")
	}
	if root.ReplyToMessageID != "" {
		return BeginRoundResult{}, invalid("round root cannot be a reply")
	}
	if root.ActivationPolicy == "" {
		root.ActivationPolicy = "wake_when_idle"
	}
	if root.ActivationPolicy != "wake_when_idle" && root.ActivationPolicy != "never_wake" {
		return BeginRoundResult{}, invalid("activation_policy must be wake_when_idle or never_wake")
	}
	if root.TTL <= 0 || root.TTL > s.maxTTLDuration {
		return BeginRoundResult{}, invalid(fmt.Sprintf("ttl must be greater than zero and at most %s", s.maxTTLDuration))
	}
	root.ProducerID, root.OperationID = strings.TrimSpace(root.ProducerID), strings.TrimSpace(root.OperationID)
	root.SenderAddress, root.RecipientAddress = strings.TrimSpace(root.SenderAddress), strings.TrimSpace(root.RecipientAddress)
	root.CorrelationID = strings.TrimSpace(root.CorrelationID)
	fingerprint, err := roundFingerprint(root, request.RoundTTL)
	if err != nil {
		return BeginRoundResult{}, fmt.Errorf("fingerprint round request: %w", err)
	}
	lookup, err := s.store.LookupRoundOperation(ctx, root.ProducerID, root.OperationID)
	if err != nil {
		return BeginRoundResult{}, mapStoreError(err)
	}
	if lookup.Found {
		if lookup.Fingerprint != fingerprint {
			return BeginRoundResult{}, &Error{Code: CodeOperationConflict, Err: store.ErrOperationConflict}
		}
		lookup.Result.Replayed = true
		return lookup.Result, nil
	}
	roundID, err := s.ids()
	if err != nil {
		return BeginRoundResult{}, fmt.Errorf("generate round ID: %w", err)
	}
	messageID, err := s.ids()
	if err != nil {
		return BeginRoundResult{}, fmt.Errorf("generate message ID: %w", err)
	}
	deliveryID, err := s.ids()
	if err != nil {
		return BeginRoundResult{}, fmt.Errorf("generate delivery ID: %w", err)
	}
	now := s.clock.Now().UTC()
	result, err := s.store.BeginRound(ctx, now, store.BeginRoundRequest{SubmitMessageRequest: store.SubmitMessageRequest{ProducerID: root.ProducerID, LeaseToken: root.LeaseToken, OperationID: root.OperationID, Fingerprint: fingerprint, MessageID: messageID, DeliveryID: deliveryID, SenderAddress: root.SenderAddress, RecipientAddress: root.RecipientAddress, Body: root.Body, CorrelationID: root.CorrelationID, ActivationPolicy: root.ActivationPolicy, ExpiresAt: now.Add(root.TTL)}, RoundID: roundID, RoundExpiresAt: now.Add(request.RoundTTL)})
	return result, mapStoreError(err)
}

func roundFingerprint(root SubmitMessageRequest, roundTTL time.Duration) (string, error) {
	v := struct {
		Family                                                                 string
		SenderAddress, RecipientAddress, Body, CorrelationID, ActivationPolicy string
		TTL, RoundTTL                                                          int64
	}{"round_root", root.SenderAddress, root.RecipientAddress, root.Body, root.CorrelationID, root.ActivationPolicy, root.TTL.Nanoseconds(), roundTTL.Nanoseconds()}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) GetRound(ctx context.Context, roundID string) (Round, error) {
	if err := required("round_id", roundID); err != nil {
		return Round{}, err
	}
	v, err := s.store.GetRound(ctx, strings.TrimSpace(roundID))
	return v, mapStoreError(err)
}
func (s *Service) ListRounds(ctx context.Context) ([]Round, error) {
	v, err := s.store.ListRounds(ctx)
	return v, mapStoreError(err)
}
func (s *Service) CancelRound(ctx context.Context, roundID string) (Round, error) {
	if err := required("round_id", roundID); err != nil {
		return Round{}, err
	}
	v, err := s.store.CancelRound(ctx, s.clock.Now().UTC(), strings.TrimSpace(roundID))
	return v, mapStoreError(err)
}
func (s *Service) SettleRounds(ctx context.Context) ([]Round, error) {
	v, err := s.store.SettleRounds(ctx, s.clock.Now().UTC())
	return v, mapStoreError(err)
}

func (s *Service) WaitRound(ctx context.Context, request WaitRoundRequest) (WaitRoundResult, error) {
	if err := required("round_id", request.RoundID); err != nil {
		return WaitRoundResult{}, err
	}
	if request.Timeout < 0 || request.Timeout > maxRoundWait {
		return WaitRoundResult{}, invalid("round wait timeout must be between zero and 5s")
	}
	if request.Timeout == 0 {
		request.Timeout = defaultRoundWait
	}
	deadline := time.Now().Add(request.Timeout)
	for {
		round, err := s.GetRound(ctx, request.RoundID)
		if err != nil {
			return WaitRoundResult{}, err
		}
		if round.Status != "pending" {
			return WaitRoundResult{Round: round}, nil
		}
		if !round.ExpiresAt.After(s.clock.Now().UTC()) {
			return WaitRoundResult{Round: round, ObservedExpired: true}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WaitRoundResult{Round: round, TimedOut: true}, nil
		}
		pause := 10 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return WaitRoundResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}
