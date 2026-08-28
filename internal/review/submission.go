package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	submissionSchema        = "crew_review.submission.v1"
	maxReviewSummaryBytes   = 64 * 1024
	defaultSubmissionAuthor = "@reviewer"
)

var githubCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// SubmitTaskForReview is the managed, runtime-neutral entry point. It owns
// the durable submission handoff and local job admission, while every review
// fact and gate state is read from Den through the narrow adapter interface.
func (s *Service) SubmitTaskForReview(ctx context.Context, request SubmissionRequest) (SubmissionReceipt, bool, error) {
	normalized, err := normalizeSubmissionRequest(request)
	if err != nil {
		return SubmissionReceipt{}, false, err
	}
	store, ok := s.store.(SubmissionStore)
	if !ok {
		return SubmissionReceipt{}, false, ErrSubmissionStore
	}
	idempotencyKey := strings.TrimSpace(normalized.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = submissionTargetKey(normalized)
	}
	materialHash, err := submissionMaterialHash(normalized)
	if err != nil {
		return SubmissionReceipt{}, false, err
	}
	record, replayed, err := store.AdmitSubmission(ctx, normalized, idempotencyKey, materialHash)
	if err != nil {
		return SubmissionReceipt{}, replayed, err
	}
	receipt, err := s.advanceSubmission(ctx, store, record)
	receipt.Replayed = replayed
	return receipt, replayed, err
}

func normalizeSubmissionRequest(request SubmissionRequest) (SubmissionRequest, error) {
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Repository = strings.TrimSpace(request.Repository)
	request.CommitSHA = strings.ToLower(strings.TrimSpace(request.CommitSHA))
	request.Ref = strings.TrimSpace(request.Ref)
	request.BaseCommit = strings.ToLower(strings.TrimSpace(request.BaseCommit))
	request.ReviewSummary = strings.TrimSpace(request.ReviewSummary)
	request.Reviewer = strings.TrimSpace(request.Reviewer)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.Reviewer == "" {
		request.Reviewer = defaultSubmissionAuthor
	}
	if request.ProjectID == "" {
		return SubmissionRequest{}, errors.New("project_id is required")
	}
	if request.TaskID <= 0 {
		return SubmissionRequest{}, errors.New("task_id must be positive")
	}
	if !validRepository(request.Repository) {
		return SubmissionRequest{}, errors.New("repository must be owner/name")
	}
	if !githubCommitPattern.MatchString(request.CommitSHA) {
		return SubmissionRequest{}, errors.New("commit_sha must be a full 40-character SHA")
	}
	if request.BaseCommit != "" && !githubCommitPattern.MatchString(request.BaseCommit) {
		return SubmissionRequest{}, errors.New("base_commit must be a full 40-character SHA when supplied")
	}
	if request.Ref == "" {
		return SubmissionRequest{}, errors.New("ref is required")
	}
	checks := make([]string, 0, len(request.RequiredChecks))
	seen := make(map[string]struct{}, len(request.RequiredChecks))
	for _, raw := range request.RequiredChecks {
		check := strings.TrimSpace(raw)
		if check == "" {
			return SubmissionRequest{}, errors.New("required_checks cannot contain empty checks")
		}
		if _, exists := seen[check]; exists {
			continue
		}
		seen[check] = struct{}{}
		checks = append(checks, check)
	}
	sort.Strings(checks)
	request.RequiredChecks = checks
	if request.ReviewSummary == "" {
		return SubmissionRequest{}, errors.New("review_summary_md is required")
	}
	if len([]byte(request.ReviewSummary)) > maxReviewSummaryBytes {
		return SubmissionRequest{}, fmt.Errorf("review_summary_md exceeds %d bytes", maxReviewSummaryBytes)
	}
	return request, nil
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(parts[0], " \t\r\n") && !strings.ContainsAny(parts[1], " \t\r\n")
}

func submissionTargetKey(request SubmissionRequest) string {
	identity, _ := json.Marshal(struct {
		ProjectID      string   `json:"project_id"`
		TaskID         int64    `json:"task_id"`
		Repository     string   `json:"repository"`
		CommitSHA      string   `json:"commit_sha"`
		Ref            string   `json:"ref"`
		RequiredChecks []string `json:"required_checks"`
		BaseCommit     string   `json:"base_commit,omitempty"`
	}{request.ProjectID, request.TaskID, request.Repository, request.CommitSHA, request.Ref, request.RequiredChecks, request.BaseCommit})
	digest := sha256.Sum256(identity)
	return "crew-review-target:" + hex.EncodeToString(digest[:])
}

func submissionMaterialHash(request SubmissionRequest) (string, error) {
	material, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode review submission material: %w", err)
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) advanceSubmission(ctx context.Context, store SubmissionStore, record SubmissionRecord) (SubmissionReceipt, error) {
	if record.Phase == SubmissionGateFailed || record.Phase == SubmissionStale || record.Phase == SubmissionJobAdmitted {
		return submissionReceipt(record), nil
	}
	den, ok := s.den.(SubmissionDenClient)
	if !ok {
		return s.submissionUnavailable(ctx, store, record, "review_submission_backend_unavailable", "the configured Den adapter does not support managed submission", true)
	}

	if record.ReviewRoundID == 0 {
		round, err := den.RequestReview(ctx, record.Request)
		if err != nil {
			if errors.Is(err, ErrStaleRound) {
				return s.submissionStale(ctx, store, record, err.Error())
			}
			return s.submissionUnavailable(ctx, store, record, "den_request_review_unavailable", err.Error(), true)
		}
		if round.ID <= 0 {
			return s.submissionUnavailable(ctx, store, record, "den_request_review_invalid", "Den request_review returned no review round", false)
		}
		record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{
			Phase:         SubmissionRoundRecorded,
			ReviewRoundID: &round.ID,
			Failure:       emptyStringPtr(),
		})
		if err != nil {
			return SubmissionReceipt{}, err
		}
	}

	gate := record.Gate
	var err error
	if len(record.Request.RequiredChecks) == 0 {
		gate = noRequiredChecksGate(record.Request)
		if record.Gate != gate || record.Phase != SubmissionGatePassed {
			record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{
				Phase:   SubmissionGatePassed,
				Gate:    &gate,
				Failure: emptyStringPtr(),
			})
			if err != nil {
				return SubmissionReceipt{}, err
			}
		}
	} else if gate.Handle == "" {
		gate, err = den.WatchGitHubChecks(ctx, submissionGateRequest(record.Request))
		if err != nil {
			return s.submissionUnavailable(ctx, store, record, "den_watch_github_checks_unavailable", err.Error(), true)
		}
		if gate.Status == "" {
			return s.submissionUnavailable(ctx, store, record, "den_watch_github_checks_invalid", "Den watch_github_checks returned no gate status", false)
		}
		record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{
			Phase:   gatePhase(gate.Status),
			Gate:    &gate,
			Failure: emptyStringPtr(),
		})
		if err != nil {
			return SubmissionReceipt{}, err
		}
	} else if record.Gate.Status == "pending" || record.Phase == SubmissionGatePending || record.Phase == SubmissionRoundRecorded || record.Phase == SubmissionUnavailable {
		gate, err = den.GetGitHubCheckGate(ctx, submissionGateRequest(record.Request))
		if err != nil {
			return s.submissionUnavailable(ctx, store, record, "den_get_github_check_gate_unavailable", err.Error(), true)
		}
		if gate.Status == "" {
			return s.submissionUnavailable(ctx, store, record, "den_get_github_check_gate_invalid", "Den get_github_check_gate returned no gate status", false)
		}
		if gate.Status != record.Gate.Status || record.Phase == SubmissionUnavailable {
			record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{
				Phase:   gatePhase(gate.Status),
				Gate:    &gate,
				Failure: emptyStringPtr(),
			})
			if err != nil {
				return SubmissionReceipt{}, err
			}
		}
	}

	if gate.Status == "pending" {
		if record.Phase != SubmissionGatePending {
			record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionGatePending, Gate: &gate, Failure: emptyStringPtr()})
			if err != nil {
				return SubmissionReceipt{}, err
			}
		}
		receipt := submissionReceipt(record)
		receipt.Retryable = true
		return receipt, nil
	}
	if gate.Status != "passed" {
		reason := gate.Status
		if reason == "" {
			reason = "unknown"
		}
		return s.submissionGateFailed(ctx, store, record, gate, "github gate is "+reason)
	}
	if record.Phase != SubmissionGatePassed {
		record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionGatePassed, Gate: &gate, Failure: emptyStringPtr()})
		if err != nil {
			return SubmissionReceipt{}, err
		}
	}

	key := Key{
		ProjectID:     record.Request.ProjectID,
		TaskID:        record.Request.TaskID,
		ReviewRoundID: record.ReviewRoundID,
		CorrelationID: "crew-review-submission:" + record.ID,
	}
	current, err := s.den.GetReviewContext(ctx, key)
	if err != nil {
		if errors.Is(err, ErrStaleRound) {
			return s.submissionStale(ctx, store, record, err.Error())
		}
		return s.submissionUnavailable(ctx, store, record, "den_review_context_unavailable", err.Error(), true)
	}
	if !current.ReviewableFor(key) {
		if current.NextState == "gate_pending" {
			return s.submissionUnavailable(ctx, store, record, "den_review_context_pending", "Den still reports the review gate as pending", true)
		}
		return s.submissionStale(ctx, store, record, "Den current review context is no longer source_review_ready")
	}
	job, _, err := s.admitRound(ctx, Admission{
		IdempotencyKey: "crew-review-round:" + record.Request.ProjectID + ":" + strconv.FormatInt(record.ReviewRoundID, 10),
		Key:            key,
		Reviewer:       record.Request.Reviewer,
		Workspace:      current.Workspace,
		Branch:         record.Request.Ref,
		Gate:           gate,
	})
	if err != nil {
		return s.submissionUnavailable(ctx, store, record, "review_job_admission_failed", err.Error(), true)
	}
	jobID := job.ID
	record, err = s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionJobAdmitted, JobID: &jobID, Gate: &gate, Failure: emptyStringPtr()})
	if err != nil {
		return SubmissionReceipt{}, err
	}
	return submissionReceipt(record), nil
}

func submissionGateRequest(request SubmissionRequest) GateRequest {
	return GateRequest{ProjectID: request.ProjectID, TaskID: request.TaskID, Repository: request.Repository, CommitSHA: request.CommitSHA, Ref: request.Ref, RequiredChecks: request.RequiredChecks, RequestedBy: request.Reviewer}
}

// noRequiredChecksGate records the deliberate no-gate path in the same
// provider-neutral evidence shape as a Den GitHub gate. The public field is
// still required, but an empty list means this submission has no checks to
// watch; Den review context remains the authority before local job admission.
func noRequiredChecksGate(request SubmissionRequest) GateEvidence {
	return GateEvidence{
		Repository:     request.Repository,
		Ref:            request.Ref,
		CommitSHA:      request.CommitSHA,
		Status:         "passed",
		TerminalReason: "no_required_checks",
	}
}

func gatePhase(status string) SubmissionPhase {
	if status == "pending" {
		return SubmissionGatePending
	}
	if status == "passed" {
		return SubmissionGatePassed
	}
	return SubmissionGateFailed
}

func (s *Service) transitionSubmission(ctx context.Context, store SubmissionStore, record SubmissionRecord, transition SubmissionTransition) (SubmissionRecord, error) {
	updated, err := store.TransitionSubmission(ctx, record.ID, record.Phase, transition)
	if errors.Is(err, ErrSubmissionChanged) {
		return store.GetSubmission(ctx, record.ID)
	}
	return updated, err
}

func (s *Service) submissionUnavailable(ctx context.Context, store SubmissionStore, record SubmissionRecord, code, message string, retryable bool) (SubmissionReceipt, error) {
	message = strings.TrimSpace(message)
	updated, err := s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionUnavailable, Failure: &message})
	if err != nil {
		return SubmissionReceipt{}, err
	}
	receipt := submissionReceipt(updated)
	receipt.OK = false
	receipt.Retryable = retryable
	receipt.ErrorCode = code
	receipt.Error = message
	receipt.Summary = message
	return receipt, nil
}

func (s *Service) submissionStale(ctx context.Context, store SubmissionStore, record SubmissionRecord, message string) (SubmissionReceipt, error) {
	message = strings.TrimSpace(message)
	updated, err := s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionStale, Failure: &message})
	if err != nil {
		return SubmissionReceipt{}, err
	}
	receipt := submissionReceipt(updated)
	receipt.OK = false
	receipt.ErrorCode = "stale_review_round"
	receipt.Error = message
	receipt.Summary = message
	return receipt, nil
}

func (s *Service) submissionGateFailed(ctx context.Context, store SubmissionStore, record SubmissionRecord, gate GateEvidence, message string) (SubmissionReceipt, error) {
	updated, err := s.transitionSubmission(ctx, store, record, SubmissionTransition{Phase: SubmissionGateFailed, Gate: &gate, Failure: &message})
	if err != nil {
		return SubmissionReceipt{}, err
	}
	receipt := submissionReceipt(updated)
	receipt.OK = false
	receipt.ErrorCode = "github_gate_failed"
	receipt.Error = message
	receipt.Summary = message
	return receipt, nil
}

func submissionReceipt(record SubmissionRecord) SubmissionReceipt {
	receipt := SubmissionReceipt{
		Schema:         submissionSchema,
		SchemaVersion:  1,
		OK:             record.Phase != SubmissionGateFailed && record.Phase != SubmissionStale && record.Phase != SubmissionUnavailable,
		SubmissionID:   record.ID,
		ProjectID:      record.Request.ProjectID,
		TaskID:         record.Request.TaskID,
		Repository:     record.Request.Repository,
		CommitSHA:      record.Request.CommitSHA,
		Ref:            record.Request.Ref,
		RequiredChecks: append([]string(nil), record.Request.RequiredChecks...),
		BaseCommit:     record.Request.BaseCommit,
		Reviewer:       record.Request.Reviewer,
		Phase:          record.Phase,
		ReviewRoundID:  record.ReviewRoundID,
		JobID:          record.JobID,
		Error:          record.Failure,
		Summary:        submissionSummary(record),
	}
	if record.Gate.Handle != "" {
		receipt.GateID = record.Gate.Handle
	}
	receipt.GateStatus = record.Gate.Status
	receipt.GateTerminalReason = record.Gate.TerminalReason
	if receipt.GateTerminalReason == "" && record.Gate.Status != "" && record.Gate.Status != "pending" && record.Gate.Status != "passed" {
		receipt.GateTerminalReason = record.Gate.Status
	}
	if receipt.Error == "" && record.Gate.FailureSummary != "" {
		receipt.Error = record.Gate.FailureSummary
	}
	switch record.Phase {
	case SubmissionGateFailed:
		receipt.OK = false
		receipt.Retryable = false
		receipt.ErrorCode = "github_gate_failed"
		receipt.Summary = firstNonEmpty(record.Failure, submissionSummary(record))
	case SubmissionStale:
		receipt.OK = false
		receipt.Retryable = false
		receipt.ErrorCode = "stale_review_round"
		receipt.Summary = firstNonEmpty(record.Failure, submissionSummary(record))
	case SubmissionJobAdmitted:
		receipt.OK = true
		receipt.Retryable = false
	}
	return receipt
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func submissionSummary(record SubmissionRecord) string {
	switch record.Phase {
	case SubmissionAccepted, SubmissionRoundRecorded:
		return "Review submission is waiting for Den review setup."
	case SubmissionUnavailable:
		return "Review submission is temporarily unavailable; retry the same request."
	case SubmissionGatePending:
		return "Review submission is waiting for the exact GitHub check gate."
	case SubmissionGatePassed, SubmissionJobAdmitted:
		return "Review submission was admitted to the local review job queue."
	case SubmissionGateFailed:
		return "Review submission stopped because the exact GitHub check gate did not pass."
	case SubmissionStale:
		return "Review submission stopped because its Den review round is stale."
	default:
		return "Review submission state recorded."
	}
}

func reviewRequestNotes(request SubmissionRequest) string {
	notes := request.ReviewSummary
	notes += "\n\nManaged submission source: " + request.Repository + " @ " + request.CommitSHA + " (" + request.Ref + ")."
	if request.BaseCommit != "" {
		notes += " Diff base: " + request.BaseCommit + "."
	}
	return notes
}

func emptyStringPtr() *string {
	value := ""
	return &value
}
