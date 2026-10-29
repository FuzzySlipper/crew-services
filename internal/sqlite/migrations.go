package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

type migration struct {
	version int
	sql     string
	// rebuildsTables marks a migration that replaces a table other tables
	// reference. SQLite only allows that with foreign keys off, which cannot be
	// changed inside a transaction; the result is checked before commit.
	rebuildsTables bool
}

//go:embed migrations/001_bootstrap.sql
var migration001 string

//go:embed migrations/002_directory.sql
var migration002 string

//go:embed migrations/003_messages.sql
var migration003 string

//go:embed migrations/004_delivery_protocol.sql
var migration004 string

//go:embed migrations/005_rounds.sql
var migration005 string

//go:embed migrations/006_sessions.sql
var migration006 string

//go:embed migrations/007_receipt_retention.sql
var migration007 string

//go:embed migrations/008_activation_policies.sql
var migration008 string

//go:embed migrations/009_drop_session_projection.sql
var migration009 string

var migrations = []migration{
	{
		version: 1,
		sql:     migration001,
	},
	{
		version: 2,
		sql:     migration002,
	},
	{version: 3, sql: migration003},
	{version: 4, sql: migration004},
	{version: 5, sql: migration005},
	{version: 6, sql: migration006},
	{version: 7, sql: migration007},
	{version: 8, sql: migration008, rebuildsTables: true},
	{version: 9, sql: migration009},
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	for _, candidate := range migrations {
		applied, err := s.applied(ctx, candidate.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := s.apply(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applied(ctx context.Context, version int) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read migration %d: %w", version, err)
	}
	return true, nil
}

func (s *Store) apply(ctx context.Context, candidate migration) error {
	if candidate.rebuildsTables {
		// The store holds a single connection, so the pragma applies to the
		// transaction below.
		if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("disable foreign keys for migration %d: %w", candidate.version, err)
		}
		defer s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", candidate.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, candidate.sql); err != nil {
		return fmt.Errorf("apply migration %d: %w", candidate.version, err)
	}
	if candidate.rebuildsTables {
		rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return fmt.Errorf("check foreign keys after migration %d: %w", candidate.version, err)
		}
		violated := rows.Next()
		if err := rows.Close(); err != nil {
			return fmt.Errorf("check foreign keys after migration %d: %w", candidate.version, err)
		}
		if violated {
			return fmt.Errorf("migration %d left foreign key violations", candidate.version)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, applied_at) VALUES (?, CURRENT_TIMESTAMP)", candidate.version); err != nil {
		return fmt.Errorf("record migration %d: %w", candidate.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", candidate.version, err)
	}
	return nil
}
