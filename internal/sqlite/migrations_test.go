package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOpenMigratesFreshAndExistingDatabaseIdempotently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "crew-messaging.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer second.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 7 {
		t.Fatalf("migration count = %d, want 7", count)
	}
	var name string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'service_metadata'").Scan(&name); err != nil {
		t.Fatalf("service metadata table missing: %v", err)
	}
}

func TestDeliveryProtocolMigrationPreservesV3LedgerRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sqlText := range []string{migration001, migration002, migration003} {
		if _, err := db.Exec(sqlText); err != nil {
			t.Fatalf("apply v3 schema: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES (?,?)`, i, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO messages(message_id,producer_id,operation_id,request_fingerprint,sender_address,recipient_address,body,correlation_id,reply_to_message_id,activation_policy,created_at,expires_at,sender_generation,recipient_generation) VALUES ('m','p','o','f','a','b','body','','','wake_when_idle','2026-08-20T00:00:00Z','2026-08-21T00:00:00Z',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deliveries(delivery_id,message_id,recipient_address,recipient_generation,state,created_at,expires_at) VALUES ('d','m','b',1,'queued','2026-08-20T00:00:00Z','2026-08-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade v3: %v", err)
	}
	defer opened.Close()
	var state string
	var attempts int
	if err := opened.db.QueryRow(`SELECT state,attempt_count FROM deliveries WHERE delivery_id='d'`).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attempts != 0 {
		t.Fatalf("upgraded row = %q attempts %d", state, attempts)
	}
}

func TestReceiptRetentionMigrationDropsOnlyNoWorkClaimReceipts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sqlText := range []string{migration001, migration002, migration003, migration004, migration005, migration006} {
		if _, err := db.Exec(sqlText); err != nil {
			t.Fatalf("apply v6 schema: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES (?,?)`, i, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for _, receipt := range []struct{ operation, kind, result string }{
		{"empty", "claim", `{"claim":{"claimed":false,"reason":"no_work"}}`},
		{"claimed", "claim", `{"claim":{"claimed":true,"claim_token":"t"}}`},
		{"begin", "begin_dispatch", `{"claim":{"claimed":false},"delivery":{"delivery_id":"d"}}`},
	} {
		if _, err := db.Exec(`INSERT INTO adapter_operation_receipts(adapter_id,operation_id,kind,fingerprint,result_json,created_at) VALUES ('a',?,?,'f',?,'2026-08-20T00:00:00Z')`, receipt.operation, receipt.kind, receipt.result); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade v6: %v", err)
	}
	defer opened.Close()
	rows, err := opened.db.Query(`SELECT operation_id FROM adapter_operation_receipts ORDER BY operation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, operation)
	}
	if len(kept) != 2 || kept[0] != "begin" || kept[1] != "claimed" {
		t.Fatalf("kept receipts = %v, want [begin claimed]", kept)
	}
}
