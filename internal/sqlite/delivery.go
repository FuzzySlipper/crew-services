package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"crew-services/internal/store"
)

func (s *Store) LookupDeliveryOperation(ctx context.Context, adapterID, operationID string) (store.DeliveryOperationLookup, error) {
	return lookupDeliveryReceipt(ctx, s.db, adapterID, operationID)
}

func lookupDeliveryReceipt(ctx context.Context, q queryer, adapterID, operationID string) (store.DeliveryOperationLookup, error) {
	var fingerprint, encoded string
	err := q.QueryRowContext(ctx, `SELECT fingerprint, result_json FROM adapter_operation_receipts WHERE adapter_id=? AND operation_id=?`, adapterID, operationID).Scan(&fingerprint, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeliveryOperationLookup{}, nil
	}
	if err != nil {
		return store.DeliveryOperationLookup{}, fmt.Errorf("read delivery operation receipt: %w", err)
	}
	var result store.DeliveryOperationResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return store.DeliveryOperationLookup{}, fmt.Errorf("decode delivery operation receipt: %w", err)
	}
	return store.DeliveryOperationLookup{Found: true, Fingerprint: fingerprint, Result: result}, nil
}

// DeliveryOperation owns every adapter-driven delivery transition. Receipt
// replay happens before lease/binding/state checks and is repeated inside the
// transaction to make concurrent first attempts deterministic.
func (s *Store) DeliveryOperation(ctx context.Context, now time.Time, r store.DeliveryOperationRequest) (store.DeliveryOperationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.DeliveryOperationResult{}, fmt.Errorf("begin delivery operation: %w", err)
	}
	defer tx.Rollback()
	if old, err := lookupDeliveryReceipt(ctx, tx, r.AdapterID, r.OperationID); err != nil {
		return store.DeliveryOperationResult{}, err
	} else if old.Found {
		if old.Fingerprint != r.Fingerprint {
			return store.DeliveryOperationResult{}, store.ErrOperationConflict
		}
		old.Result.Replayed = true
		old.Result.Claim.Replayed = true
		return old.Result, nil
	}
	var result store.DeliveryOperationResult
	switch r.Kind {
	case "claim":
		result, err = s.claim(ctx, tx, now, r)
	case "renew", "release", "begin_dispatch", "fail_claimed":
		result, err = s.claimTransition(ctx, tx, now, r)
	case "acknowledge", "fail", "outcome_unknown":
		result, err = s.reconcile(ctx, tx, now, r)
	default:
		err = fmt.Errorf("unknown delivery operation %q", r.Kind)
	}
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return store.DeliveryOperationResult{}, fmt.Errorf("encode delivery operation receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_operation_receipts (adapter_id,operation_id,kind,fingerprint,result_json,created_at) VALUES (?,?,?,?,?,?)`, r.AdapterID, r.OperationID, r.Kind, r.Fingerprint, string(encoded), timestamp(now)); err != nil {
		return store.DeliveryOperationResult{}, fmt.Errorf("record delivery operation receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.DeliveryOperationResult{}, fmt.Errorf("commit delivery operation: %w", err)
	}
	return result, nil
}

func (s *Store) claim(ctx context.Context, tx *sql.Tx, now time.Time, r store.DeliveryOperationRequest) (store.DeliveryOperationResult, error) {
	lease, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken)
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	binding, found, err := readBinding(ctx, tx, r.Address)
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	if !found {
		return store.DeliveryOperationResult{}, store.ErrNotFound
	}
	if !binding.Bound {
		return store.DeliveryOperationResult{}, store.ErrNotBound
	}
	if binding.AdapterID != r.AdapterID {
		return store.DeliveryOperationResult{}, store.ErrAdapterMismatch
	}
	if binding.Generation != r.Generation {
		return store.DeliveryOperationResult{}, store.ErrConflict
	}
	for {
		head, found, err := headForUpdate(ctx, tx, r.Address, r.Generation)
		if err != nil {
			return store.DeliveryOperationResult{}, err
		}
		if !found {
			return noClaim("no_work", nil), nil
		}
		if head.State != "queued" {
			head = publicDelivery(head)
			return noClaim("head_"+head.State, &head), nil
		}
		if terminal, reason, err := queuedInvalid(ctx, tx, now, head); err != nil {
			return store.DeliveryOperationResult{}, err
		} else if terminal {
			state := "failed"
			if reason == "ttl_expired" {
				state = "expired"
			}
			if err := terminalize(ctx, tx, now, head.DeliveryID, state, reason); err != nil {
				return store.DeliveryOperationResult{}, err
			}
			continue
		}
		message, found, err := readMessage(ctx, tx, head.MessageID)
		if err != nil {
			return store.DeliveryOperationResult{}, err
		}
		if !found {
			return store.DeliveryOperationResult{}, store.ErrNotFound
		}
		action, reason := claimAction(message.ActivationPolicy, binding.Capabilities, r.Availability)
		if action == "" {
			head = publicDelivery(head)
			return noClaim(reason, &head), nil
		}
		expires := now.UTC().Add(r.ClaimDuration)
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='claimed',attempt_count=attempt_count+1,claim_owner_adapter_id=?,claim_owner_instance_id=?,claim_token=?,claim_expires_at=?,claimed_at=?,dispatch_action=? WHERE delivery_id=? AND state='queued'`, r.AdapterID, lease.InstanceID, r.GeneratedClaimToken, timestamp(expires), timestamp(now), action, head.DeliveryID)
		if err != nil {
			return store.DeliveryOperationResult{}, fmt.Errorf("claim delivery: %w", err)
		}
		updated, _, err := readDelivery(ctx, tx, head.DeliveryID)
		if err != nil {
			return store.DeliveryOperationResult{}, err
		}
		updated = publicDelivery(updated)
		return store.DeliveryOperationResult{Claim: store.ClaimResult{Claimed: true, Message: &message, Delivery: &updated, ClaimToken: r.GeneratedClaimToken, DispatchAction: action}}, nil
	}
}
func noClaim(reason string, head *store.Delivery) store.DeliveryOperationResult {
	return store.DeliveryOperationResult{Claim: store.ClaimResult{Claimed: false, Reason: reason, Head: head}}
}
func headForUpdate(ctx context.Context, q queryer, address string, generation int64) (store.Delivery, bool, error) {
	row := q.QueryRowContext(ctx, deliveryProjection+` FROM deliveries WHERE recipient_address=? AND recipient_generation=? AND state NOT IN ('delivered','failed','expired','cancelled','outcome_unknown') ORDER BY accepted_sequence ASC LIMIT 1`, address, generation)
	v, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Delivery{}, false, nil
	}
	if err != nil {
		return store.Delivery{}, false, err
	}
	return v, true, nil
}
func queuedInvalid(ctx context.Context, q queryer, now time.Time, d store.Delivery) (bool, string, error) {
	if !d.ExpiresAt.After(now) {
		return true, "ttl_expired", nil
	}
	b, found, err := readBinding(ctx, q, d.RecipientAddress)
	if err != nil {
		return false, "", err
	}
	if !found || !b.Bound {
		return true, "recipient_unbound", nil
	}
	if b.Generation != d.RecipientGeneration {
		return true, "binding_generation_changed", nil
	}
	return false, "", nil
}
func claimAction(policy string, caps []string, availability string) (string, string) {
	has := func(want string) bool {
		for _, v := range caps {
			if v == want {
				return true
			}
		}
		return false
	}
	if policy == "never_wake" {
		if availability == store.AvailabilityIdle && has(store.CapabilityDeliverWhenIdle) {
			return "deliver_at_idle", ""
		}
		return "", "not_eligible"
	}
	switch availability {
	case store.AvailabilityBusy:
		if has(store.CapabilityDurableNextTurn) {
			return "register_next_turn", ""
		}
		if has(store.CapabilityPendingNotice) {
			// A notification is informational only: it leaves executable work
			// queued and never becomes a dispatch or delivery fact.
			return "", "pending_notification"
		}
	case store.AvailabilityIdle:
		if has(store.CapabilityDeliverWhenIdle) {
			return "deliver_at_idle", ""
		}
	case store.AvailabilityInactive:
		if has(store.CapabilityWakeInactive) {
			return "wake_then_deliver", ""
		}
	}
	return "", "not_eligible"
}

func (s *Store) claimTransition(ctx context.Context, tx *sql.Tx, now time.Time, r store.DeliveryOperationRequest) (store.DeliveryOperationResult, error) {
	d, found, err := readDelivery(ctx, tx, r.DeliveryID)
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	if !found {
		return store.DeliveryOperationResult{}, store.ErrNotFound
	}
	lease, err := claimFence(ctx, tx, now, d, r)
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	_ = lease
	switch r.Kind {
	case "renew":
		if d.State != "claimed" {
			return store.DeliveryOperationResult{}, store.ErrConflict
		}
		expires := now.Add(r.ClaimDuration)
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET claim_expires_at=? WHERE delivery_id=?`, timestamp(expires), d.DeliveryID)
	case "release":
		if d.State != "claimed" {
			return store.DeliveryOperationResult{}, store.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='queued',claim_owner_adapter_id='',claim_owner_instance_id='',claim_token='',claim_expires_at=NULL,claimed_at=NULL,dispatch_action='' WHERE delivery_id=?`, d.DeliveryID)
	case "begin_dispatch":
		if d.State != "claimed" {
			return store.DeliveryOperationResult{}, store.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='dispatching',native_attempt_ref=?,dispatching_at=? WHERE delivery_id=?`, r.NativeAttemptRef, timestamp(now), d.DeliveryID)
	case "fail_claimed":
		if d.State != "claimed" {
			return store.DeliveryOperationResult{}, store.ErrConflict
		}
		err = terminalize(ctx, tx, now, d.DeliveryID, "failed", "adapter_failed")
	}
	if err != nil {
		return store.DeliveryOperationResult{}, fmt.Errorf("transition %s: %w", r.Kind, err)
	}
	updated, _, err := readDelivery(ctx, tx, d.DeliveryID)
	return store.DeliveryOperationResult{Delivery: publicDelivery(updated)}, err
}
func claimFence(ctx context.Context, tx *sql.Tx, now time.Time, d store.Delivery, r store.DeliveryOperationRequest) (store.AdapterLease, error) {
	lease, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken)
	if err != nil {
		return store.AdapterLease{}, err
	}
	if d.ClaimOwnerAdapterID != r.AdapterID || d.ClaimOwnerInstanceID != lease.InstanceID || d.ClaimToken != r.ClaimToken {
		return store.AdapterLease{}, store.ErrClaimFenced
	}
	if d.ClaimExpiresAt == nil || !d.ClaimExpiresAt.After(now) {
		return store.AdapterLease{}, store.ErrLeaseExpired
	}
	return lease, nil
}
func (s *Store) reconcile(ctx context.Context, tx *sql.Tx, now time.Time, r store.DeliveryOperationRequest) (store.DeliveryOperationResult, error) {
	d, found, err := readDelivery(ctx, tx, r.DeliveryID)
	if err != nil {
		return store.DeliveryOperationResult{}, err
	}
	if !found {
		return store.DeliveryOperationResult{}, store.ErrNotFound
	}
	if d.State != "dispatching" {
		return store.DeliveryOperationResult{}, store.ErrConflict
	}
	if d.NativeAttemptRef != r.NativeAttemptRef {
		return store.DeliveryOperationResult{}, store.ErrClaimFenced
	}
	if _, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken); err != nil {
		return store.DeliveryOperationResult{}, err
	}
	// Dispatching is the native-side-effect boundary. Its durable claim owner,
	// rather than a later mutable address binding, authorizes reconciliation.
	if d.ClaimOwnerAdapterID != r.AdapterID {
		return store.DeliveryOperationResult{}, store.ErrAdapterMismatch
	}
	state, reason := "delivered", "native_accepted"
	if r.Kind == "fail" {
		state, reason = "failed", "native_failed"
	}
	if r.Kind == "outcome_unknown" {
		state, reason = "outcome_unknown", "native_outcome_unknown"
	}
	if err := terminalize(ctx, tx, now, d.DeliveryID, state, reason); err != nil {
		return store.DeliveryOperationResult{}, err
	}
	updated, _, err := readDelivery(ctx, tx, d.DeliveryID)
	return store.DeliveryOperationResult{Delivery: publicDelivery(updated)}, err
}
func terminalize(ctx context.Context, tx *sql.Tx, now time.Time, id, state, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET state=?,terminal_reason=?,terminal_at=? WHERE delivery_id=?`, state, reason, timestamp(now), id)
	if err != nil {
		return err
	}
	return bridgeRoundDeliveryTerminal(ctx, tx, now, id, state, reason)
}
