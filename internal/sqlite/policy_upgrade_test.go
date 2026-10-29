package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/store"
)

type upgradeClock struct{ now time.Time }

func (c upgradeClock) Now() time.Time { return c.now }

// v7MessageFingerprint and v7RoundFingerprint are the request fingerprints the
// pre-v8 service stored, hashed with the legacy policy spelling (an omitted
// policy defaulted to wake_when_idle before hashing).
func v7MessageFingerprint(t *testing.T, sender, recipient, body, policy string, ttl time.Duration) string {
	t.Helper()
	return v7Hash(t, struct {
		SenderAddress, RecipientAddress, Body, CorrelationID, ReplyToMessageID, ActivationPolicy string
		TTLNanoseconds                                                                           int64
	}{sender, recipient, body, "", "", policy, ttl.Nanoseconds()})
}

func v7RoundFingerprint(t *testing.T, sender, recipient, body, policy string, ttl, roundTTL time.Duration) string {
	t.Helper()
	return v7Hash(t, struct {
		Family                                                                 string
		SenderAddress, RecipientAddress, Body, CorrelationID, ActivationPolicy string
		TTL, RoundTTL                                                          int64
	}{"round_root", sender, recipient, body, "", policy, ttl.Nanoseconds(), roundTTL.Nanoseconds()})
}

func v7Hash(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// TestPolicyMigrationKeepsLegacyOperationsIdempotent accepts messages and
// round roots on a schema v7 database with the retired policy names, upgrades
// it, and retries each operation exactly: the original records replay, and a
// changed payload under the same operation still conflicts.
func TestPolicyMigrationKeepsLegacyOperationsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v7.db")
	clock := upgradeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	for _, sqlText := range []string{migration001, migration002, migration003, migration004, migration005, migration006, migration007} {
		if _, err := db.Exec(sqlText); err != nil {
			t.Fatalf("apply v7 schema: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES (?,?)`, i, timestamp(clock.now)); err != nil {
			t.Fatal(err)
		}
	}
	v7 := &Store{db: db}
	setup, err := service.New(v7, clock, service.WithMaxLeaseDuration(time.Hour), service.WithTokenGenerator(func() (string, error) { return "alpha-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := setup.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.alpha", InstanceID: "alpha", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"agent/alice", "agent/bob"} {
		if _, err := setup.PutBinding(ctx, service.PutBindingRequest{Address: address, ActorAdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, AdapterID: alpha.AdapterID, TargetRef: "opaque/" + address}); err != nil {
			t.Fatal(err)
		}
	}

	type legacyOp struct {
		operation, sent, stored string // policy as the client sent it, and as v7 stored and hashed it
		round                   bool
	}
	ops := []legacyOp{
		{"msg-wake", "wake_when_idle", "wake_when_idle", false},
		{"msg-never", "never_wake", "never_wake", false},
		{"msg-omitted", "", "wake_when_idle", false},
		{"round-wake", "wake_when_idle", "wake_when_idle", true},
		{"round-never", "never_wake", "never_wake", true},
		{"round-omitted", "", "wake_when_idle", true},
	}
	const ttl, roundTTL = 30 * time.Minute, 10 * time.Minute
	accepted := map[string]string{}
	for _, op := range ops {
		body := "body " + op.operation
		submit := store.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: op.operation, MessageID: "m-" + op.operation, DeliveryID: "d-" + op.operation, SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: body, ActivationPolicy: op.stored, ExpiresAt: clock.now.Add(ttl)}
		if op.round {
			submit.Fingerprint = v7RoundFingerprint(t, "agent/alice", "agent/bob", body, op.stored, ttl, roundTTL)
			result, err := v7.BeginRound(ctx, clock.now, store.BeginRoundRequest{SubmitMessageRequest: submit, RoundID: "r-" + op.operation, RoundExpiresAt: clock.now.Add(roundTTL)})
			if err != nil {
				t.Fatalf("v7 begin round %s: %v", op.operation, err)
			}
			accepted[op.operation] = result.Message.MessageID
		} else {
			submit.Fingerprint = v7MessageFingerprint(t, "agent/alice", "agent/bob", body, op.stored, ttl)
			result, err := v7.SubmitMessage(ctx, clock.now, submit)
			if err != nil {
				t.Fatalf("v7 submit %s: %v", op.operation, err)
			}
			accepted[op.operation] = result.Message.MessageID
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade v7: %v", err)
	}
	defer upgraded.Close()
	svc, err := service.New(upgraded, clock, service.WithIDGenerator(func() (string, error) { return "", errors.New("a replay must not generate IDs") }))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		body := "body " + op.operation
		request := service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: op.operation, SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: body, ActivationPolicy: op.sent, TTL: ttl}
		changed := request
		changed.Body = "changed " + body
		if op.round {
			replayed, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: request, RoundTTL: roundTTL})
			if err != nil || !replayed.Replayed || replayed.Message.MessageID != accepted[op.operation] || replayed.Round.RoundID != "r-"+op.operation {
				t.Fatalf("retry round %s = %#v, %v", op.operation, replayed, err)
			}
			if _, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: changed, RoundTTL: roundTTL}); !isCode(err, service.CodeOperationConflict) {
				t.Fatalf("changed round %s error = %v, want operation_conflict", op.operation, err)
			}
			continue
		}
		replayed, err := svc.SubmitMessage(ctx, request)
		if err != nil || !replayed.Replayed || replayed.Message.MessageID != accepted[op.operation] || replayed.Delivery.DeliveryID != "d-"+op.operation {
			t.Fatalf("retry message %s = %#v, %v", op.operation, replayed, err)
		}
		if _, err := svc.SubmitMessage(ctx, changed); !isCode(err, service.CodeOperationConflict) {
			t.Fatalf("changed message %s error = %v, want operation_conflict", op.operation, err)
		}
	}
	// A legacy next_turn operation is not the same request as next_step.
	steer := service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "msg-wake", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "body msg-wake", ActivationPolicy: store.PolicyNextStep, TTL: ttl}
	if _, err := svc.SubmitMessage(ctx, steer); !isCode(err, service.CodeOperationConflict) {
		t.Fatalf("next_step retry of a wake_when_idle operation error = %v, want operation_conflict", err)
	}
}

func isCode(err error, code service.ErrorCode) bool {
	var typed *service.Error
	return errors.As(err, &typed) && typed.Code == code
}
