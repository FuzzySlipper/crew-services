package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func (s *SQLiteStore) AdmitSubmission(ctx context.Context, request SubmissionRequest, idempotencyKey, materialHash string) (SubmissionRecord, bool, error) {
	if request.ProjectID == "" || request.TaskID <= 0 || request.Repository == "" || request.CommitSHA == "" || request.Ref == "" || request.ReviewSummary == "" || request.Reviewer == "" {
		return SubmissionRecord{}, false, errors.New("invalid review submission")
	}
	if idempotencyKey == "" || materialHash == "" {
		return SubmissionRecord{}, false, errors.New("submission idempotency and material hashes are required")
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return SubmissionRecord{}, false, fmt.Errorf("encode review submission: %w", err)
	}
	now := stamp(s.clock.Now())
	id := uuid.NewString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO crew_review_submissions(id,idem_key,material_hash,request_json,phase,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, idempotencyKey, materialHash, string(requestJSON), SubmissionAccepted, now, now)
	if err == nil {
		record, getErr := s.GetSubmission(ctx, id)
		return record, false, getErr
	}
	var existingHash, existingID string
	if lookupErr := s.db.QueryRowContext(ctx, `SELECT material_hash,id FROM crew_review_submissions WHERE idem_key=?`, idempotencyKey).Scan(&existingHash, &existingID); lookupErr != nil {
		return SubmissionRecord{}, false, err
	}
	if existingHash != materialHash {
		return SubmissionRecord{}, false, ErrConflict
	}
	record, getErr := s.GetSubmission(ctx, existingID)
	return record, true, getErr
}

func (s *SQLiteStore) GetSubmission(ctx context.Context, id string) (SubmissionRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,idem_key,material_hash,request_json,phase,review_round_id,gate_json,job_id,failure,created_at,updated_at FROM crew_review_submissions WHERE id=?`, id)
	return scanSubmission(row)
}

// LatestReusableSubmission returns the newest exact-source submission for a
// task that can still be retried. A failed gate or stale Den round is not a
// reusable source for the manual button: the caller should use the explicit
// best-effort path instead. IDs are collected before loading records because
// this store intentionally uses one SQLite connection.
func (s *SQLiteStore) LatestReusableSubmission(ctx context.Context, projectID string, taskID int64) (SubmissionRecord, bool, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || taskID <= 0 {
		return SubmissionRecord{}, false, errors.New("project_id and task_id are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM crew_review_submissions WHERE json_extract(request_json,'$.project_id')=? AND json_extract(request_json,'$.task_id')=? AND phase NOT IN (?,?) ORDER BY updated_at DESC, created_at DESC, id DESC`, projectID, taskID, SubmissionGateFailed, SubmissionStale)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return SubmissionRecord{}, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SubmissionRecord{}, false, err
	}
	if err := rows.Close(); err != nil {
		return SubmissionRecord{}, false, err
	}
	for _, id := range ids {
		record, err := s.GetSubmission(ctx, id)
		if err != nil {
			return SubmissionRecord{}, false, err
		}
		request := record.Request
		if record.Phase == SubmissionGateFailed || record.Phase == SubmissionStale ||
			request.ProjectID != projectID || request.TaskID != taskID ||
			!validRepository(request.Repository) || !githubCommitPattern.MatchString(strings.ToLower(strings.TrimSpace(request.CommitSHA))) ||
			strings.TrimSpace(request.Ref) == "" || strings.TrimSpace(request.ReviewSummary) == "" {
			continue
		}
		return record, true, nil
	}
	return SubmissionRecord{}, false, nil
}

func scanSubmission(row scanner) (SubmissionRecord, error) {
	var record SubmissionRecord
	var requestJSON, gateJSON sql.NullString
	var roundID sql.NullInt64
	var phase string
	var createdAt, updatedAt string
	if err := row.Scan(&record.ID, &record.IdempotencyKey, &record.MaterialHash, &requestJSON, &phase, &roundID, &gateJSON, &record.JobID, &record.Failure, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubmissionRecord{}, ErrNotFound
		}
		return SubmissionRecord{}, err
	}
	if err := json.Unmarshal([]byte(requestJSON.String), &record.Request); err != nil {
		return SubmissionRecord{}, fmt.Errorf("decode review submission %s: %w", record.ID, err)
	}
	record.Phase = SubmissionPhase(phase)
	if roundID.Valid {
		record.ReviewRoundID = roundID.Int64
	}
	if gateJSON.Valid && gateJSON.String != "" {
		if err := json.Unmarshal([]byte(gateJSON.String), &record.Gate); err != nil {
			return SubmissionRecord{}, fmt.Errorf("decode review submission gate %s: %w", record.ID, err)
		}
	}
	record.CreatedAt = parse(createdAt)
	record.UpdatedAt = parse(updatedAt)
	return record, nil
}

func (s *SQLiteStore) TransitionSubmission(ctx context.Context, id string, expected SubmissionPhase, transition SubmissionTransition) (SubmissionRecord, error) {
	if id == "" || expected == "" || transition.Phase == "" {
		return SubmissionRecord{}, errors.New("invalid review submission transition")
	}
	var gateJSON string
	gateSet := transition.Gate != nil
	if gateSet {
		encoded, err := json.Marshal(*transition.Gate)
		if err != nil {
			return SubmissionRecord{}, fmt.Errorf("encode review submission gate: %w", err)
		}
		gateJSON = string(encoded)
	}
	var roundSet int
	var roundID int64
	if transition.ReviewRoundID != nil {
		roundSet = 1
		roundID = *transition.ReviewRoundID
	}
	var jobSet int
	var jobID string
	if transition.JobID != nil {
		jobSet = 1
		jobID = *transition.JobID
	}
	var failureSet int
	var failure string
	if transition.Failure != nil {
		failureSet = 1
		failure = *transition.Failure
	}
	result, err := s.db.ExecContext(ctx, `UPDATE crew_review_submissions SET phase=?, review_round_id=CASE WHEN ?=1 THEN ? ELSE review_round_id END, gate_json=CASE WHEN ?=1 THEN ? ELSE gate_json END, job_id=CASE WHEN ?=1 THEN ? ELSE job_id END, failure=CASE WHEN ?=1 THEN ? ELSE failure END, updated_at=? WHERE id=? AND phase=?`, transition.Phase, roundSet, roundID, boolInt(gateSet), gateJSON, jobSet, jobID, failureSet, failure, stamp(s.clock.Now()), id, expected)
	if err != nil {
		return SubmissionRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SubmissionRecord{}, err
	}
	if rows != 1 {
		current, getErr := s.GetSubmission(ctx, id)
		if getErr != nil {
			return SubmissionRecord{}, getErr
		}
		return current, ErrSubmissionChanged
	}
	return s.GetSubmission(ctx, id)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

var _ SubmissionStore = (*SQLiteStore)(nil)
var _ ManualReviewSubmissionStore = (*SQLiteStore)(nil)
