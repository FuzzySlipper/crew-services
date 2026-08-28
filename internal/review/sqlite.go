package review

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db       *sql.DB
	clock    Clock
	capacity int
}

func OpenSQLite(ctx context.Context, path string, clock Clock, capacity int) (*SQLiteStore, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	if capacity < 1 {
		return nil, errors.New("capacity must be positive")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, clock: clock, capacity: capacity}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS crew_review_jobs (id TEXT PRIMARY KEY, idem_key TEXT NOT NULL UNIQUE, material_hash TEXT NOT NULL, admission_json TEXT NOT NULL, state TEXT NOT NULL, finalizing_claim INTEGER NOT NULL DEFAULT 0, finalization_json TEXT, receipt_json TEXT, failure TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate crew review database: %w", err)
	}
	// The review DB was introduced without a general migration framework; this
	// one additive column preserves early local databases safely.
	var columns int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('crew_review_jobs') WHERE name='finalizing_claim'`).Scan(&columns); err != nil {
		db.Close()
		return nil, err
	}
	if columns == 0 {
		if _, err = db.ExecContext(ctx, `ALTER TABLE crew_review_jobs ADD COLUMN finalizing_claim INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = s.Recover(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *SQLiteStore) Close() error                    { return s.db.Close() }
func (s *SQLiteStore) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }
func stamp(t time.Time) string                         { return t.UTC().Format(time.RFC3339Nano) }
func parse(s string) time.Time                         { t, _ := time.Parse(time.RFC3339Nano, s); return t.UTC() }
func material(a Admission) (string, error) {
	b, e := json.Marshal(a)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func (s *SQLiteStore) Admit(ctx context.Context, a Admission) (Job, bool, error) {
	if !a.Key.valid() || a.IdempotencyKey == "" || a.Reviewer == "" {
		return Job{}, false, errors.New("invalid review job admission")
	}
	h, e := material(a)
	if e != nil {
		return Job{}, false, e
	}
	b, _ := json.Marshal(a)
	now := stamp(s.clock.Now())
	id := uuid.NewString()
	_, e = s.db.ExecContext(ctx, `INSERT INTO crew_review_jobs(id,idem_key,material_hash,admission_json,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, a.IdempotencyKey, h, string(b), Queued, now, now)
	if e == nil {
		j, _ := s.Get(ctx, id)
		return j, false, nil
	}
	var oldHash, id2 string
	e = s.db.QueryRowContext(ctx, `SELECT material_hash,id FROM crew_review_jobs WHERE idem_key=?`, a.IdempotencyKey).Scan(&oldHash, &id2)
	if e != nil {
		return Job{}, false, e
	}
	if oldHash != h {
		return Job{}, false, ErrConflict
	}
	j, e := s.Get(ctx, id2)
	return j, true, e
}
func (s *SQLiteStore) Get(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,admission_json,state,finalization_json,receipt_json,failure,created_at,updated_at FROM crew_review_jobs WHERE id=?`, id)
	return scanJob(row)
}

type scanner interface{ Scan(...any) error }

func scanJob(row scanner) (Job, error) {
	var j Job
	var a, f, r sql.NullString
	var c, u string
	e := row.Scan(&j.ID, &a, &j.State, &f, &r, &j.Failure, &c, &u)
	if errors.Is(e, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if e != nil {
		return Job{}, e
	}
	if e = json.Unmarshal([]byte(a.String), &j.Admission); e != nil {
		return Job{}, e
	}
	if f.Valid {
		var x Finalization
		e = json.Unmarshal([]byte(f.String), &x)
		j.Finalization = &x
	}
	if r.Valid {
		var x Receipt
		e = json.Unmarshal([]byte(r.String), &x)
		j.Receipt = &x
	}
	j.CreatedAt = parse(c)
	j.UpdatedAt = parse(u)
	return j, e
}
func (s *SQLiteStore) Claim(ctx context.Context) (Job, bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Job{}, false, e
	}
	defer tx.Rollback() // rejects same task already live; capacity is aggregate running only
	// Finalization has durable material already. Prefer reconciling it after a
	// restart before starting any new native work; Den owns idempotency there.
	var finalizingID string
	if e = tx.QueryRowContext(ctx, `SELECT id FROM crew_review_jobs WHERE state=? AND finalizing_claim=0 ORDER BY updated_at LIMIT 1`, Finalizing).Scan(&finalizingID); e == nil {
		result, claimErr := tx.ExecContext(ctx, `UPDATE crew_review_jobs SET finalizing_claim=1,updated_at=? WHERE id=? AND state=? AND finalizing_claim=0`, stamp(s.clock.Now()), finalizingID, Finalizing)
		if claimErr != nil {
			return Job{}, false, claimErr
		}
		claimed, _ := result.RowsAffected()
		if claimed != 1 {
			return Job{}, false, nil
		}
		if e = tx.Commit(); e != nil {
			return Job{}, false, e
		}
		j, e := s.Get(ctx, finalizingID)
		return j, e == nil, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return Job{}, false, e
	}
	row := tx.QueryRowContext(ctx, `SELECT j.id FROM crew_review_jobs j WHERE j.state=? AND NOT EXISTS (SELECT 1 FROM crew_review_jobs x WHERE x.state IN (?,?) AND json_extract(x.admission_json,'$.key.project_id')=json_extract(j.admission_json,'$.key.project_id') AND json_extract(x.admission_json,'$.key.task_id')=json_extract(j.admission_json,'$.key.task_id')) ORDER BY j.created_at LIMIT 1`, Queued, Running, Finalizing)
	var id string
	if e = row.Scan(&id); errors.Is(e, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if e != nil {
		return Job{}, false, e
	}
	var n int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM crew_review_jobs WHERE state=?`, Running).Scan(&n); e != nil {
		return Job{}, false, e
	}
	if n >= s.capacity {
		return Job{}, false, nil
	}
	now := stamp(s.clock.Now())
	if _, e = tx.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,updated_at=? WHERE id=? AND state=?`, Running, now, id, Queued); e != nil {
		return Job{}, false, e
	}
	if e = tx.Commit(); e != nil {
		return Job{}, false, e
	}
	j, e := s.Get(ctx, id)
	return j, true, e
}
func (s *SQLiteStore) PutFinalization(ctx context.Context, id string, f Finalization) (Job, error) {
	b, e := json.Marshal(f)
	if e != nil {
		return Job{}, e
	}
	res, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalization_json=?,updated_at=? WHERE id=? AND state=?`, Finalizing, string(b), stamp(s.clock.Now()), id, Running)
	if e != nil {
		return Job{}, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		current, getErr := s.Get(ctx, id)
		if getErr != nil {
			return Job{}, getErr
		}
		if current.State == Finalizing && current.Finalization != nil {
			existing, _ := json.Marshal(*current.Finalization)
			if string(existing) == string(b) {
				return current, nil
			}
			return Job{}, ErrConflict
		}
		return Job{}, ErrTooLate
	}
	return s.Get(ctx, id)
}
func (s *SQLiteStore) Complete(ctx context.Context, id string, r Receipt) (Job, error) {
	b, e := json.Marshal(r)
	if e != nil {
		return Job{}, e
	}
	_, e = s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalizing_claim=0,receipt_json=?,updated_at=? WHERE id=? AND state=?`, Succeeded, string(b), stamp(s.clock.Now()), id, Finalizing)
	if e != nil {
		return Job{}, e
	}
	return s.Get(ctx, id)
}
func (s *SQLiteStore) Fail(ctx context.Context, id string, state State, why string) (Job, error) {
	if state != Failed && state != Stale && state != Cancelled {
		return Job{}, errors.New("invalid terminal state")
	}
	states := []State{Queued, Running}
	if state == Failed || state == Stale {
		states = append(states, Finalizing)
	}
	_, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalizing_claim=0,failure=?,updated_at=? WHERE id=? AND state IN (`+placeholders(len(states))+`)`, append([]any{state, why, stamp(s.clock.Now()), id}, statesToAny(states)...)...)
	if e != nil {
		return Job{}, e
	}
	return s.Get(ctx, id)
}
func (s *SQLiteStore) Cancel(ctx context.Context, id string) (Job, error) {
	j, e := s.Get(ctx, id)
	if e != nil {
		return Job{}, e
	}
	if j.State == Finalizing || j.State.Terminal() {
		return Job{}, ErrTooLate
	}
	return s.Fail(ctx, id, Cancelled, "cancelled")
}
func (s *SQLiteStore) Recover(ctx context.Context) error {
	_, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,updated_at=? WHERE state=?`, Queued, stamp(s.clock.Now()), Running)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET finalizing_claim=0,updated_at=? WHERE state=?`, stamp(s.clock.Now()), Finalizing)
	return e
}
func placeholders(n int) string {
	out := "?"
	for i := 1; i < n; i++ {
		out += ",?"
	}
	return out
}
func statesToAny(states []State) []any {
	out := make([]any, len(states))
	for i, s := range states {
		out[i] = s
	}
	return out
}
func (s *SQLiteStore) ReleaseFinalization(ctx context.Context, id string) error {
	_, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET finalizing_claim=0,updated_at=? WHERE id=? AND state=?`, stamp(s.clock.Now()), id, Finalizing)
	return e
}
func (s *SQLiteStore) Snapshot(ctx context.Context, limit int) (Snapshot, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	out := Snapshot{Backend: "unavailable", Capacity: s.capacity, Recent: []Projection{}}
	if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM crew_review_jobs WHERE state=?`, Queued).Scan(&out.Queued); e != nil {
		return out, e
	}
	if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM crew_review_jobs WHERE state=?`, Running).Scan(&out.Running); e != nil {
		return out, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,admission_json,state,finalization_json,receipt_json,failure,created_at,updated_at FROM crew_review_jobs WHERE state IN (?,?,?,?) ORDER BY updated_at DESC LIMIT ?`, Succeeded, Failed, Cancelled, Stale, limit)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return out, e
		}
		out.Recent = append(out.Recent, j.Projection())
	}
	return out, rows.Err()
}
