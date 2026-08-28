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
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('crew_review_jobs') WHERE name='round_identity'`).Scan(&columns); err != nil {
		db.Close()
		return nil, err
	}
	if columns == 0 {
		if _, err = db.ExecContext(ctx, `ALTER TABLE crew_review_jobs ADD COLUMN round_identity TEXT`); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS crew_review_jobs_round_identity ON crew_review_jobs(round_identity) WHERE round_identity IS NOT NULL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate crew review round identity: %w", err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS crew_review_submissions (id TEXT PRIMARY KEY, idem_key TEXT NOT NULL UNIQUE, material_hash TEXT NOT NULL, request_json TEXT NOT NULL, phase TEXT NOT NULL, review_round_id INTEGER NOT NULL DEFAULT 0, gate_json TEXT, job_id TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate crew review submissions: %w", err)
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
	return s.admit(ctx, a, "")
}

func (s *SQLiteStore) admit(ctx context.Context, a Admission, roundIdentity string) (Job, bool, error) {
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
	_, e = s.db.ExecContext(ctx, `INSERT INTO crew_review_jobs(id,idem_key,material_hash,admission_json,state,created_at,updated_at,round_identity) VALUES(?,?,?,?,?,?,?,?)`, id, a.IdempotencyKey, h, string(b), Queued, now, now, nullableRoundIdentity(roundIdentity))
	if e == nil {
		j, _ := s.Get(ctx, id)
		return j, false, nil
	}
	var oldHash, id2 string
	if lookupErr := s.db.QueryRowContext(ctx, `SELECT material_hash,id FROM crew_review_jobs WHERE idem_key=?`, a.IdempotencyKey).Scan(&oldHash, &id2); lookupErr != nil {
		return Job{}, false, e
	}
	if oldHash != h {
		return Job{}, false, ErrConflict
	}
	j, e := s.Get(ctx, id2)
	return j, true, e
}

func nullableRoundIdentity(identity string) any {
	if identity == "" {
		return nil
	}
	return identity
}

func reviewRoundIdentity(key Key) string {
	identity, _ := json.Marshal(struct {
		ProjectID     string `json:"project_id"`
		TaskID        int64  `json:"task_id"`
		ReviewRoundID int64  `json:"review_round_id"`
	}{key.ProjectID, key.TaskID, key.ReviewRoundID})
	digest := sha256.Sum256(identity)
	return "crew-review-round:" + hex.EncodeToString(digest[:])
}

// AdmitRound is the round-scoped idempotency guard for the managed submission
// path. The ordinary Admission idempotency key protects one caller retry; the
// round lookup also protects two distinct callers that race after Den reused
// one current round.
func (s *SQLiteStore) AdmitRound(ctx context.Context, a Admission) (Job, bool, error) {
	if !a.Key.valid() || a.IdempotencyKey == "" || a.Reviewer == "" {
		return Job{}, false, errors.New("invalid review job admission")
	}
	h, err := material(a)
	if err != nil {
		return Job{}, false, err
	}
	var existingHash, existingID string
	err = s.db.QueryRowContext(ctx, `SELECT material_hash,id FROM crew_review_jobs WHERE idem_key=?`, a.IdempotencyKey).Scan(&existingHash, &existingID)
	if err == nil {
		if existingHash != h {
			return Job{}, false, ErrConflict
		}
		job, getErr := s.Get(ctx, existingID)
		return job, true, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, err
	}
	// review_round_id is inside the immutable admission envelope. SQLite's
	// JSON extraction keeps the round guard additive without another job table
	// migration and is already used by same-task serialization below.
	err = s.db.QueryRowContext(ctx, `SELECT id FROM crew_review_jobs WHERE json_extract(admission_json,'$.key.project_id')=? AND json_extract(admission_json,'$.key.task_id')=? AND json_extract(admission_json,'$.key.review_round_id')=? ORDER BY created_at LIMIT 1`, a.Key.ProjectID, a.Key.TaskID, a.Key.ReviewRoundID).Scan(&existingID)
	if err == nil {
		job, getErr := s.Get(ctx, existingID)
		return job, true, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, err
	}
	roundIdentity := reviewRoundIdentity(a.Key)
	job, replayed, err := s.admit(ctx, a, roundIdentity)
	if err == nil {
		return job, replayed, nil
	}
	// A distinct submission key can race past the read above. The unique round
	// index makes one insert win; reconcile the loser to that durable winner.
	var winnerID string
	if lookupErr := s.db.QueryRowContext(ctx, `SELECT id FROM crew_review_jobs WHERE round_identity=?`, roundIdentity).Scan(&winnerID); lookupErr == nil {
		job, getErr := s.Get(ctx, winnerID)
		return job, true, getErr
	}
	return Job{}, false, err
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

// ClaimPreferred is the small scheduler hook used for an already-retained
// task. It still gives durable finalization reconciliation priority.
func (s *SQLiteStore) ClaimPreferred(ctx context.Context, task TaskKey) (Job, bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Job{}, false, e
	}
	defer tx.Rollback()
	var finalizingID string
	if e = tx.QueryRowContext(ctx, `SELECT id FROM crew_review_jobs WHERE state=? AND finalizing_claim=0 ORDER BY updated_at LIMIT 1`, Finalizing).Scan(&finalizingID); e == nil {
		result, claimErr := tx.ExecContext(ctx, `UPDATE crew_review_jobs SET finalizing_claim=1,updated_at=? WHERE id=? AND state=? AND finalizing_claim=0`, stamp(s.clock.Now()), finalizingID, Finalizing)
		if claimErr != nil {
			return Job{}, false, claimErr
		}
		n, _ := result.RowsAffected()
		if n != 1 {
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
	row := tx.QueryRowContext(ctx, `SELECT j.id FROM crew_review_jobs j WHERE j.state=? AND json_extract(j.admission_json,'$.key.project_id')=? AND json_extract(j.admission_json,'$.key.task_id')=? AND NOT EXISTS (SELECT 1 FROM crew_review_jobs x WHERE x.state IN (?,?) AND json_extract(x.admission_json,'$.key.project_id')=json_extract(j.admission_json,'$.key.project_id') AND json_extract(x.admission_json,'$.key.task_id')=json_extract(j.admission_json,'$.key.task_id')) ORDER BY j.created_at LIMIT 1`, Queued, task.ProjectID, task.TaskID, Running, Finalizing)
	var id string
	if e = row.Scan(&id); errors.Is(e, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if e != nil {
		return Job{}, false, e
	}
	now := stamp(s.clock.Now())
	result, e := tx.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,updated_at=? WHERE id=? AND state=?`, Running, now, id, Queued)
	if e != nil {
		return Job{}, false, e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return Job{}, false, nil
	}
	if e = tx.Commit(); e != nil {
		return Job{}, false, e
	}
	j, e := s.Get(ctx, id)
	return j, e == nil, e
}
func (s *SQLiteStore) PutFinalization(ctx context.Context, id string, f Finalization) (Job, error) {
	b, e := json.Marshal(f)
	if e != nil {
		return Job{}, e
	}
	// The runtime callback's executing lane owns this finalization until it
	// reconciles or explicitly releases it for retry. Without this claim, a
	// second runner can reconcile the stored result before the callback's turn
	// returns and cause that lane to lose its changes-requested affinity.
	res, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalizing_claim=1,finalization_json=?,updated_at=? WHERE id=? AND state=?`, Finalizing, string(b), stamp(s.clock.Now()), id, Running)
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
func (s *SQLiteStore) Requeue(ctx context.Context, id string) (Job, error) {
	_, e := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,updated_at=? WHERE id=? AND state=?`, Queued, stamp(s.clock.Now()), id, Running)
	if e != nil {
		return Job{}, e
	}
	return s.Get(ctx, id)
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
