package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const submissionColumns = "id,idem_key,material_hash,request_json,phase,review_round_id,gate_json,job_id,failure,created_at,updated_at,notify_address,gate_auto_rechecks"

// storedRequest is the request material kept in request_json. The notify
// address lives in its own column because a resend may change it.
func storedRequest(request SubmissionRequest) ([]byte, error) {
	request.NotifyAddress = ""
	return json.Marshal(request)
}

func (s *SQLiteStore) AdmitSubmission(ctx context.Context, request SubmissionRequest, idempotencyKey, materialHash string) (SubmissionRecord, bool, error) {
	if request.ProjectID == "" || request.TaskID <= 0 || request.Repository == "" || request.CommitSHA == "" || request.Ref == "" || request.ReviewSummary == "" || request.Reviewer == "" {
		return SubmissionRecord{}, false, errors.New("invalid review submission")
	}
	if idempotencyKey == "" || materialHash == "" {
		return SubmissionRecord{}, false, errors.New("submission idempotency and material hashes are required")
	}
	requestJSON, err := storedRequest(request)
	if err != nil {
		return SubmissionRecord{}, false, fmt.Errorf("encode review submission: %w", err)
	}
	now := stamp(s.clock.Now())
	id := uuid.NewString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO crew_review_submissions(id,idem_key,material_hash,request_json,phase,created_at,updated_at,notify_address) VALUES(?,?,?,?,?,?,?,?)`, id, idempotencyKey, materialHash, string(requestJSON), SubmissionAccepted, now, now, request.NotifyAddress)
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
	row := s.db.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM crew_review_submissions WHERE id=?`, id)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM crew_review_submissions WHERE json_extract(request_json,'$.project_id')=? AND json_extract(request_json,'$.task_id')=? AND phase NOT IN (?,?,?) ORDER BY updated_at DESC, created_at DESC, id DESC`, projectID, taskID, SubmissionGateFailed, SubmissionStale, SubmissionRejected)
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

func (s *SQLiteStore) SubmissionByKey(ctx context.Context, idempotencyKey string) (SubmissionRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM crew_review_submissions WHERE idem_key=?`, idempotencyKey)
	return scanSubmission(row)
}

// ReviseSubmission replaces the request material of a submission that has not
// admitted a reviewer job. It returns to accepted so the next advance re-reads
// Den state; the recorded gate is kept.
func (s *SQLiteStore) ReviseSubmission(ctx context.Context, revision SubmissionRevision) (SubmissionRecord, error) {
	if revision.ID == "" || revision.ExpectedHash == "" || revision.MaterialHash == "" {
		return SubmissionRecord{}, errors.New("invalid review submission revision")
	}
	requestJSON, err := storedRequest(revision.Request)
	if err != nil {
		return SubmissionRecord{}, fmt.Errorf("encode review submission: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE crew_review_submissions SET request_json=?, notify_address=CASE WHEN ?<>'' THEN ? ELSE notify_address END, material_hash=?, phase=?, review_round_id=CASE WHEN ?=1 THEN 0 ELSE review_round_id END, failure='', updated_at=? WHERE id=? AND material_hash=? AND phase<>?`,
		string(requestJSON), revision.Request.NotifyAddress, revision.Request.NotifyAddress, revision.MaterialHash, SubmissionAccepted, boolInt(revision.ResetRound), stamp(s.clock.Now()), revision.ID, revision.ExpectedHash, SubmissionJobAdmitted)
	if err != nil {
		return SubmissionRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SubmissionRecord{}, err
	}
	if rows != 1 {
		return SubmissionRecord{}, ErrConflict
	}
	return s.GetSubmission(ctx, revision.ID)
}

func (s *SQLiteStore) RetireSubmission(ctx context.Context, id string, idempotencyKey string) error {
	if id == "" || idempotencyKey == "" {
		return errors.New("invalid review submission retirement")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE crew_review_submissions SET idem_key=? WHERE id=? AND idem_key=? AND phase=?`,
		idempotencyKey+":retired:"+id, id, idempotencyKey, SubmissionJobAdmitted)
	return err
}

// ListUnfinishedSubmissions returns every submission that has not reached a
// terminal phase, oldest update first. All are returned so a long-waiting
// submission is never starved or aged out of the background advance.
func (s *SQLiteStore) ListUnfinishedSubmissions(ctx context.Context) ([]SubmissionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM crew_review_submissions WHERE phase NOT IN (?,?,?,?,?) ORDER BY updated_at, id`,
		SubmissionGateFailed, SubmissionJobAdmitted, SubmissionStale, SubmissionSourceMissing, SubmissionRejected)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	records := make([]SubmissionRecord, 0, len(ids))
	for _, id := range ids {
		record, err := s.GetSubmission(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func scanSubmission(row scanner) (SubmissionRecord, error) {
	var record SubmissionRecord
	var requestJSON, gateJSON sql.NullString
	var roundID sql.NullInt64
	var phase string
	var createdAt, updatedAt, notifyAddress string
	if err := row.Scan(&record.ID, &record.IdempotencyKey, &record.MaterialHash, &requestJSON, &phase, &roundID, &gateJSON, &record.JobID, &record.Failure, &createdAt, &updatedAt, &notifyAddress, &record.GateAutoRechecks); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubmissionRecord{}, ErrNotFound
		}
		return SubmissionRecord{}, err
	}
	if err := json.Unmarshal([]byte(requestJSON.String), &record.Request); err != nil {
		return SubmissionRecord{}, fmt.Errorf("decode review submission %s: %w", record.ID, err)
	}
	record.Request.NotifyAddress = notifyAddress
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SubmissionRecord{}, err
	}
	defer tx.Rollback()
	now := stamp(s.clock.Now())
	result, err := tx.ExecContext(ctx, `UPDATE crew_review_submissions SET phase=?, review_round_id=CASE WHEN ?=1 THEN ? ELSE review_round_id END, gate_json=CASE WHEN ?=1 THEN ? ELSE gate_json END, job_id=CASE WHEN ?=1 THEN ? ELSE job_id END, failure=CASE WHEN ?=1 THEN ? ELSE failure END, gate_auto_rechecks=gate_auto_rechecks+?, updated_at=? WHERE id=? AND phase=?`, transition.Phase, roundSet, roundID, boolInt(gateSet), gateJSON, jobSet, jobID, failureSet, failure, boolInt(transition.GateAutoRecheck), now, id, expected)
	if err != nil {
		return SubmissionRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SubmissionRecord{}, err
	}
	for _, notice := range append([]*Notice{transition.Notice}, transition.Notices...) {
		if rows != 1 || notice == nil {
			continue
		}
		if notice.OperationID == "" || notice.SubmissionID != id || notice.Recipient == "" || notice.Body == "" {
			return SubmissionRecord{}, errors.New("invalid review submission notice")
		}
		// A notice for the same operation (the same failed gate) is kept as
		// it is: it may already be sent.
		if _, err := tx.ExecContext(ctx, `INSERT INTO crew_review_notices(operation_id,submission_id,kind,recipient,body,state,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(operation_id) DO NOTHING`,
			notice.OperationID, id, notice.Kind, notice.Recipient, notice.Body, NoticePending, now, now, noticeStamp(s.clock.Now())); err != nil {
			return SubmissionRecord{}, fmt.Errorf("record review submission notice: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
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

var (
	_ SubmissionStore             = (*SQLiteStore)(nil)
	_ ManualReviewSubmissionStore = (*SQLiteStore)(nil)
	_ SubmissionRevisionStore     = (*SQLiteStore)(nil)
	_ SubmissionAdvanceStore      = (*SQLiteStore)(nil)
)

func (s *SQLiteStore) SetSubmissionNotifyAddress(ctx context.Context, id, address string) (SubmissionRecord, error) {
	if id == "" || address == "" {
		return SubmissionRecord{}, errors.New("invalid review submission notify address")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE crew_review_submissions SET notify_address=? WHERE id=?`, address, id); err != nil {
		return SubmissionRecord{}, err
	}
	return s.GetSubmission(ctx, id)
}

const noticeColumns = `operation_id,submission_id,kind,recipient,body,state,attempts,last_error,message_id,created_at,updated_at,sent_at,next_attempt_at`

// noticeStamp is a fixed-width UTC timestamp, so next_attempt_at compares in
// time order as text. parse reads it like any RFC 3339 stamp.
func noticeStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// ListPendingNotices returns pending notices that are due, earliest scheduled
// attempt first. A notice that failed goes behind every notice already due,
// so no single notice can hold the front of the queue.
func (s *SQLiteStore) ListPendingNotices(ctx context.Context, limit int) ([]Notice, error) {
	if limit < 1 {
		limit = 1
	}
	return s.queryNotices(ctx, `SELECT `+noticeColumns+` FROM crew_review_notices WHERE state=? AND next_attempt_at<=? ORDER BY next_attempt_at, created_at, operation_id LIMIT ?`, NoticePending, noticeStamp(s.clock.Now()), limit)
}

func (s *SQLiteStore) ListSubmissionNotices(ctx context.Context, submissionID string) ([]Notice, error) {
	return s.queryNotices(ctx, `SELECT `+noticeColumns+` FROM crew_review_notices WHERE submission_id=? ORDER BY created_at, operation_id`, submissionID)
}

func (s *SQLiteStore) queryNotices(ctx context.Context, query string, args ...any) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notices := make([]Notice, 0)
	for rows.Next() {
		var notice Notice
		var kind, state, createdAt, updatedAt, sentAt, nextAttemptAt string
		if err := rows.Scan(&notice.OperationID, &notice.SubmissionID, &kind, &notice.Recipient, &notice.Body, &state, &notice.Attempts, &notice.LastError, &notice.MessageID, &createdAt, &updatedAt, &sentAt, &nextAttemptAt); err != nil {
			return nil, err
		}
		notice.Kind, notice.State = NoticeKind(kind), NoticeState(state)
		notice.CreatedAt, notice.UpdatedAt = parse(createdAt), parse(updatedAt)
		if sentAt != "" {
			notice.SentAt = parse(sentAt)
		}
		if nextAttemptAt != "" {
			notice.NextAttemptAt = parse(nextAttemptAt)
		}
		notices = append(notices, notice)
	}
	return notices, rows.Err()
}

// MarkNoticeSent records the fabric's acceptance. Only a pending notice moves.
func (s *SQLiteStore) MarkNoticeSent(ctx context.Context, operationID, messageID string) error {
	now := stamp(s.clock.Now())
	_, err := s.db.ExecContext(ctx, `UPDATE crew_review_notices SET state=?, message_id=?, attempts=attempts+1, last_error='', sent_at=?, updated_at=? WHERE operation_id=? AND state=?`, NoticeSent, messageID, now, now, operationID, NoticePending)
	return err
}

// MarkNoticeAttempt records a failed send and when the notice is next due.
func (s *SQLiteStore) MarkNoticeAttempt(ctx context.Context, operationID, failure string, nextAttemptAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE crew_review_notices SET attempts=attempts+1, last_error=?, next_attempt_at=?, updated_at=? WHERE operation_id=? AND state=?`, boundedNoticeError(failure), noticeStamp(nextAttemptAt), stamp(s.clock.Now()), operationID, NoticePending)
	return err
}

func (s *SQLiteStore) MarkNoticeAbandoned(ctx context.Context, operationID, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE crew_review_notices SET state=?, last_error=?, updated_at=? WHERE operation_id=? AND state=?`, NoticeAbandoned, boundedNoticeError(reason), stamp(s.clock.Now()), operationID, NoticePending)
	return err
}

func boundedNoticeError(message string) string {
	const limit = 2048
	if len(message) > limit {
		return message[:limit]
	}
	return message
}

var (
	_ NoticeStore        = (*SQLiteStore)(nil)
	_ NotifyAddressStore = (*SQLiteStore)(nil)
)
