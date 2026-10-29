package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/store"
)

// TestDropProjectionMigrationKeepsMessagesDeliveriesAndBindings upgrades a v8
// database that holds session projection rows, events and event receipts. The
// projection tables are gone afterwards; bindings, messages and deliveries
// survive unchanged.
func TestDropProjectionMigrationKeepsMessagesDeliveriesAndBindings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v8.db")
	clock := upgradeClock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	v8 := &Store{db: db}
	for _, candidate := range migrations {
		if candidate.version > 8 {
			break
		}
		if err := v8.apply(ctx, candidate); err != nil {
			t.Fatalf("apply v%d: %v", candidate.version, err)
		}
	}
	setup, err := service.New(v8, clock, service.WithMaxLeaseDuration(time.Hour), service.WithTokenGenerator(func() (string, error) { return "alpha-token", nil }))
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
	submitted, err := v8.SubmitMessage(ctx, clock.now, store.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "op", Fingerprint: "f", MessageID: "m", DeliveryID: "d", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "kept", ActivationPolicy: "next_turn", ExpiresAt: clock.now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO sessions(session_id,adapter_id,adapter_key,label,location,status,capabilities_json,revision,created_at,updated_at) VALUES ('s1','adapter.alpha','key','Work','local','idle','[]',1,'2026-09-04T00:00:00Z','2026-09-04T00:00:00Z')`,
		`INSERT INTO session_events(event_id,session_id,sequence,event_type,payload_json,occurred_at,recorded_at) VALUES ('e1','s1',1,'state','{}','2026-09-04T00:00:00Z','2026-09-04T00:00:00Z')`,
		`INSERT INTO session_event_operation_receipts(adapter_id,operation_id,fingerprint,event_id,created_at) VALUES ('adapter.alpha','event-op','f','e1','2026-09-04T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed projection: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade v8: %v", err)
	}
	defer upgraded.Close()
	var applied int
	if err := upgraded.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 9`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration 9 applied = %d %v", applied, err)
	}
	var leftovers int
	if err := upgraded.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name IN ('sessions','session_events','session_event_operation_receipts')`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("projection schema objects left = %d", leftovers)
	}
	for _, address := range []string{"agent/alice", "agent/bob"} {
		binding, err := upgraded.Resolve(ctx, address)
		if err != nil || !binding.Bound || binding.TargetRef != "opaque/"+address {
			t.Fatalf("binding %s = %#v %v", address, binding, err)
		}
	}
	message, err := upgraded.GetMessage(ctx, submitted.Message.MessageID)
	if err != nil || message.Body != "kept" {
		t.Fatalf("message = %#v %v", message, err)
	}
	delivery, err := upgraded.GetDelivery(ctx, submitted.Delivery.DeliveryID)
	if err != nil || delivery.State != "queued" || delivery.MessageID != "m" {
		t.Fatalf("delivery = %#v %v", delivery, err)
	}
}
