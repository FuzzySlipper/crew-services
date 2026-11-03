package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SubmissionForJob returns the newest submission that admitted jobID.
func (s *SQLiteStore) SubmissionForJob(ctx context.Context, jobID string) (SubmissionRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM crew_review_submissions WHERE job_id=? ORDER BY updated_at DESC, created_at DESC LIMIT 1`, jobID)
	record, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return SubmissionRecord{}, false, nil
	}
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	return record, true, nil
}

// FailWithNotices fails a job and records its notices in one transaction.
func (s *SQLiteStore) FailWithNotices(ctx context.Context, id, why string, notices []*Notice) (Job, bool, error) {
	for _, notice := range notices {
		if notice == nil || notice.OperationID == "" || notice.Recipient == "" || notice.Body == "" {
			return Job{}, false, errors.New("invalid review failure notice")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	now := stamp(s.clock.Now())
	result, err := tx.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalizing_claim=0,failure=?,updated_at=? WHERE id=? AND state IN (?,?,?)`, Failed, why, now, id, Queued, Running, Finalizing)
	if err != nil {
		return Job{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Job{}, false, err
	}
	for _, notice := range notices {
		if rows != 1 {
			break
		}
		if err := insertNotice(ctx, tx, notice, now, s.clock.Now()); err != nil {
			return Job{}, false, fmt.Errorf("record review failure notice: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	job, err := s.Get(ctx, id)
	return job, rows == 1, err
}

// RequeueAfter returns a running job to the queue and keeps claims off it
// until at.
func (s *SQLiteStore) RequeueAfter(ctx context.Context, id string, at time.Time) (Job, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,run_after=?,updated_at=? WHERE id=? AND state=?`, Queued, at.UnixNano(), stamp(s.clock.Now()), id, Running); err != nil {
		return Job{}, err
	}
	return s.Get(ctx, id)
}

var (
	_ FailureNoticeStore  = (*SQLiteStore)(nil)
	_ DelayedRequeueStore = (*SQLiteStore)(nil)
)

// CompleteWithNotices stores a job's Den receipt and records its verdict
// notices in one transaction. Only the transition out of finalizing records
// them, so a retried finalization never notifies twice.
func (s *SQLiteStore) CompleteWithNotices(ctx context.Context, id string, r Receipt, notices []*Notice) (Job, error) {
	for _, notice := range notices {
		if notice == nil || notice.OperationID == "" || notice.Recipient == "" || notice.Body == "" {
			return Job{}, errors.New("invalid review verdict notice")
		}
	}
	receipt, err := json.Marshal(r)
	if err != nil {
		return Job{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	now := stamp(s.clock.Now())
	result, err := tx.ExecContext(ctx, `UPDATE crew_review_jobs SET state=?,finalizing_claim=0,receipt_json=?,updated_at=? WHERE id=? AND state=?`, Succeeded, string(receipt), now, id, Finalizing)
	if err != nil {
		return Job{}, err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return Job{}, err
	} else if rows == 1 {
		for _, notice := range notices {
			if err := insertNotice(ctx, tx, notice, now, s.clock.Now()); err != nil {
				return Job{}, fmt.Errorf("record review verdict notice: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return s.Get(ctx, id)
}
