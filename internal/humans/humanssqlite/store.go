// Package humanssqlite is crew-humans' SQLite store.
package humanssqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/humans"

	_ "modernc.org/sqlite"
)

//go:embed migrations/001_humans.sql
var migration001 string

var migrations = []string{migration001}

// Store is crew-humans' records in one SQLite file.
type Store struct {
	db *sql.DB
}

var _ humans.Store = (*Store)(nil)

// Open opens path and applies every migration it has not seen.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open humans database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("prepare humans database: %w", err)
		}
	}
	for i, script := range migrations {
		version := i + 1
		var seen int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&seen); err != nil {
			return err
		}
		if seen > 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, script); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("humans migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, stamp(time.Now())); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// stamp is a fixed-width UTC time, so stored times sort as text.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func parseStamp(value string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000000000Z", value)
	return t
}

func optional(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

func optionalTime(value sql.NullString) *time.Time {
	if !value.Valid {
		return nil
	}
	t := parseStamp(value.String)
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertNotice records n and its deliveries in one transaction, or returns
// the notice the source already sent under the same key.
func (s *Store) InsertNotice(ctx context.Context, n humans.Notice) (humans.Notice, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return humans.Notice{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO notices (id, person, source, idempotency_key, title, body, link, reason, grp, urgent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source, idempotency_key) DO NOTHING`,
		n.ID, n.Person, n.Source, n.IdempotencyKey, n.Title, n.Text, n.Link, n.Reason, n.Group, boolInt(n.Urgent), stamp(n.CreatedAt))
	if err != nil {
		return humans.Notice{}, false, err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM notices WHERE source = ? AND idempotency_key = ?`, n.Source, n.IdempotencyKey).Scan(&id); err != nil {
			return humans.Notice{}, false, err
		}
		existing, err := getNotice(ctx, tx, id)
		return existing, false, err
	}
	for _, d := range n.Deliveries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries (notice_id, route, kind, state, attempts, last_error, next_attempt_at, give_up_at, sent_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, n.ID, d.Route, d.Kind, d.State, d.Attempts, d.LastError, stamp(d.NextAttemptAt), stamp(d.GiveUpAt), optional(d.SentAt)); err != nil {
			return humans.Notice{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return humans.Notice{}, false, err
	}
	return n, true, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const noticeColumns = `id, person, source, idempotency_key, title, body, link, reason, grp, urgent, created_at, read_at`

type scanner interface{ Scan(dest ...any) error }

func scanNotice(row scanner) (humans.Notice, error) {
	var n humans.Notice
	var urgent int
	var created string
	var read sql.NullString
	if err := row.Scan(&n.ID, &n.Person, &n.Source, &n.IdempotencyKey, &n.Title, &n.Text, &n.Link, &n.Reason, &n.Group, &urgent, &created, &read); err != nil {
		return humans.Notice{}, err
	}
	n.Urgent, n.CreatedAt, n.ReadAt = urgent == 1, parseStamp(created), optionalTime(read)
	return n, nil
}

func getNotice(ctx context.Context, q querier, id string) (humans.Notice, error) {
	n, err := scanNotice(q.QueryRowContext(ctx, `SELECT `+noticeColumns+` FROM notices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return humans.Notice{}, humans.ErrNotFound
	}
	if err != nil {
		return humans.Notice{}, err
	}
	deliveries, err := listDeliveries(ctx, q, `WHERE notice_id = ? ORDER BY route`, id)
	n.Deliveries = deliveries
	return n, err
}

func (s *Store) GetNotice(ctx context.Context, id string) (humans.Notice, error) {
	return getNotice(ctx, s.db, id)
}

func (s *Store) ListNotices(ctx context.Context, person string, unreadOnly bool, before string, limit int) ([]humans.Notice, error) {
	query := `SELECT ` + noticeColumns + ` FROM notices WHERE person = ?`
	args := []any{person}
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}
	if before != "" {
		query += ` AND id < ?`
		args = append(args, before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var notices []humans.Notice
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		notices = append(notices, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range notices {
		if notices[i].Deliveries, err = listDeliveries(ctx, s.db, `WHERE notice_id = ? ORDER BY route`, notices[i].ID); err != nil {
			return nil, err
		}
	}
	return notices, nil
}

func (s *Store) CountUnread(ctx context.Context, person string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE person = ? AND read_at IS NULL`, person).Scan(&count)
	return count, err
}

func (s *Store) MarkRead(ctx context.Context, person string, ids []string, through, group string, at time.Time) (int, error) {
	query := `UPDATE notices SET read_at = ? WHERE person = ? AND read_at IS NULL`
	args := []any{stamp(at), person}
	if len(ids) > 0 {
		query += ` AND id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	} else if through != "" {
		query += ` AND id <= ?`
		args = append(args, through)
	} else {
		query += ` AND grp = ?`
		args = append(args, group)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return int(rows), nil
}

const deliveryColumns = `notice_id, route, kind, state, attempts, last_error, next_attempt_at, give_up_at, sent_at`

func listDeliveries(ctx context.Context, q querier, where string, args ...any) ([]humans.Delivery, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []humans.Delivery{}
	for rows.Next() {
		var d humans.Delivery
		var next, giveUp string
		var sent sql.NullString
		if err := rows.Scan(&d.NoticeID, &d.Route, &d.Kind, &d.State, &d.Attempts, &d.LastError, &next, &giveUp, &sent); err != nil {
			return nil, err
		}
		d.NextAttemptAt, d.GiveUpAt, d.SentAt = parseStamp(next), parseStamp(giveUp), optionalTime(sent)
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]humans.Delivery, error) {
	return listDeliveries(ctx, s.db, `WHERE state = ? AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?`, humans.DeliveryPending, stamp(now), limit)
}

func (s *Store) UpdateDelivery(ctx context.Context, d humans.Delivery) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET state = ?, attempts = ?, last_error = ?, next_attempt_at = ?, sent_at = ? WHERE notice_id = ? AND route = ?`,
		d.State, d.Attempts, d.LastError, stamp(d.NextAttemptAt), optional(d.SentAt), d.NoticeID, d.Route)
	return err
}
