package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	manualReviewRef      = "main"
	manualReviewReviewer = "@reviewer"
	manualReviewPreamble = "This is a server-initiated best-effort review. Review the current code on main against the Den task description and bounded task messages. Do not assume or invent an unprovided commit, packet, or source-control fact; report only findings supported by the current workspace and supplied task context."
)

// GetManualReviewCapability authorizes the browser control against the live
// Den task and then projects whether an exact durable submission can be
// resumed. A non-review task is a normal ineligible response so callers can
// hide the control without treating an expected state change as an outage.
func (s *Service) GetManualReviewCapability(ctx context.Context, projectID string, taskID int64) (ManualReviewCapability, error) {
	key, err := manualTaskKey(projectID, taskID)
	if err != nil {
		return ManualReviewCapability{}, err
	}
	client, ok := s.den.(ManualReviewDenClient)
	if !ok {
		return ManualReviewCapability{}, errors.New("manual review Den adapter is not configured")
	}
	task, err := client.GetTaskContext(ctx, key)
	if err != nil {
		return ManualReviewCapability{}, err
	}
	if !taskIdentityMatches(task, key) {
		return ManualReviewCapability{}, fmt.Errorf("manual review Den task identity does not match %s/%d", key.ProjectID, key.TaskID)
	}
	if !isReviewStatus(task.Status) {
		return ManualReviewCapability{
			Eligible: false,
			Mode:     ManualReviewBestEffort,
			Label:    "Manual review unavailable",
			Detail:   "Manual review is available only while the task status is review.",
		}, nil
	}
	capability := bestEffortCapability()
	if store, ok := s.store.(ManualReviewSubmissionStore); ok {
		record, found, err := store.LatestReusableSubmission(ctx, key.ProjectID, key.TaskID)
		if err != nil {
			return ManualReviewCapability{}, err
		}
		if found && task.CurrentReviewRoundID > 0 && record.ReviewRoundID == task.CurrentReviewRoundID {
			capability.Mode = ManualReviewExact
			capability.Label = "Send to review queue"
			capability.Detail = "Resume the latest reusable exact-source review submission in the managed review queue."
			capability.Source = &ManualReviewSource{
				Repository: record.Request.Repository,
				CommitSHA:  strings.ToLower(strings.TrimSpace(record.Request.CommitSHA)),
				Ref:        strings.TrimSpace(record.Request.Ref),
			}
		}
	}
	return capability, nil
}

// SubmitManualReview performs the same live authorization as the capability
// read. Exact submissions are advanced through the existing durable handoff;
// absent exact evidence follows a separate commitless path with a fixed
// server-owned preamble and round-scoped job idempotency.
func (s *Service) SubmitManualReview(ctx context.Context, request ManualReviewSubmissionRequest) (ManualReviewReceipt, bool, error) {
	key, err := manualTaskKey(request.ProjectID, request.TaskID)
	if err != nil {
		return ManualReviewReceipt{}, false, err
	}
	client, ok := s.den.(ManualReviewDenClient)
	if !ok {
		return ManualReviewReceipt{}, false, errors.New("manual review Den adapter is not configured")
	}
	task, err := client.GetTaskContext(ctx, key)
	if err != nil {
		return ManualReviewReceipt{}, false, err
	}
	if !taskIdentityMatches(task, key) {
		return ManualReviewReceipt{}, false, fmt.Errorf("manual review Den task identity does not match %s/%d", key.ProjectID, key.TaskID)
	}
	if !isReviewStatus(task.Status) {
		return ManualReviewReceipt{}, false, ErrTaskNotReviewable
	}

	if store, ok := s.store.(ManualReviewSubmissionStore); ok {
		record, found, err := store.LatestReusableSubmission(ctx, key.ProjectID, key.TaskID)
		if err != nil {
			return ManualReviewReceipt{}, false, err
		}
		if found && task.CurrentReviewRoundID > 0 && record.ReviewRoundID == task.CurrentReviewRoundID {
			// The manual transport key is only an admission trigger. Reuse the
			// exact ledger identity so SubmitTaskForReview sees the same durable
			// material instead of creating a second submission row.
			record.Request.IdempotencyKey = record.IdempotencyKey
			receipt, replayed, err := s.SubmitTaskForReview(ctx, record.Request)
			if err != nil {
				return ManualReviewReceipt{}, replayed, err
			}
			return manualSubmissionReceipt(receipt), replayed, nil
		}
	}

	reviewer := strings.TrimSpace(request.Reviewer)
	if reviewer == "" {
		reviewer = manualReviewReviewer
	}
	round, err := client.RequestManualReview(ctx, ManualReviewRequest{
		ProjectID: key.ProjectID,
		TaskID:    key.TaskID,
		Ref:       manualReviewRef,
		Reviewer:  reviewer,
		Preamble:  manualReviewPreamble,
	})
	if err != nil {
		return ManualReviewReceipt{}, false, err
	}
	if round.ID <= 0 {
		return ManualReviewReceipt{}, false, errors.New("Den manual review request returned no review round id")
	}
	admittedKey := Key{
		ProjectID:     key.ProjectID,
		TaskID:        key.TaskID,
		ReviewRoundID: round.ID,
		CorrelationID: manualCorrelationID(key, round.ID),
	}
	current, err := s.den.GetReviewContext(ctx, admittedKey)
	if err != nil {
		return ManualReviewReceipt{}, false, err
	}
	if !current.ReviewableFor(admittedKey) {
		if current.NextState == "gate_pending" {
			return ManualReviewReceipt{Mode: ManualReviewBestEffort, Status: "pending", Message: "Manual review is waiting for Den review setup.", RoundID: round.ID}, false, nil
		}
		return ManualReviewReceipt{}, false, ErrStaleRound
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = "crew-review-manual-round:" + key.ProjectID + ":" + strconv.FormatInt(key.TaskID, 10) + ":" + strconv.FormatInt(round.ID, 10)
	}
	job, replayed, err := s.admitRound(ctx, Admission{
		IdempotencyKey: idempotencyKey,
		Key:            admittedKey,
		Reviewer:       reviewer,
		Workspace:      current.Workspace,
		Branch:         manualReviewRef,
		ReviewPreamble: manualReviewPreamble,
	})
	if err != nil {
		return ManualReviewReceipt{}, replayed, err
	}
	message := "Best-effort review of current main was queued."
	if replayed {
		message = "Best-effort review of current main is already queued."
	}
	return ManualReviewReceipt{Mode: ManualReviewBestEffort, Status: "queued", Message: message, JobID: job.ID, RoundID: round.ID}, replayed, nil
}

func manualTaskKey(projectID string, taskID int64) (TaskKey, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return TaskKey{}, errors.New("project_id is required")
	}
	if taskID <= 0 {
		return TaskKey{}, errors.New("task_id must be positive")
	}
	return TaskKey{ProjectID: projectID, TaskID: taskID}, nil
}

func taskIdentityMatches(task TaskContext, key TaskKey) bool {
	return strings.TrimSpace(task.ProjectID) == key.ProjectID && task.TaskID == key.TaskID
}

func isReviewStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "review")
}

func bestEffortCapability() ManualReviewCapability {
	return ManualReviewCapability{
		Eligible: true,
		Mode:     ManualReviewBestEffort,
		Label:    "Request best-effort review",
		Detail:   "Ask a reviewer to compare current code on main with the Den task description and bounded task messages.",
	}
}

func manualCorrelationID(key TaskKey, roundID int64) string {
	return "crew-manual-review:" + key.ProjectID + ":" + strconv.FormatInt(key.TaskID, 10) + ":" + strconv.FormatInt(roundID, 10)
}

func manualSubmissionReceipt(receipt SubmissionReceipt) ManualReviewReceipt {
	status := "accepted"
	message := receipt.Summary
	if receipt.JobID != "" {
		status = "queued"
		message = "Exact review was queued."
	} else if receipt.Retryable {
		status = "pending"
	} else if !receipt.OK {
		status = "failed"
	}
	if strings.TrimSpace(receipt.Error) != "" {
		message = receipt.Error
	}
	return ManualReviewReceipt{
		Mode:    ManualReviewExact,
		Status:  status,
		Message: firstNonEmpty(message, "Exact review submission state recorded."),
		JobID:   receipt.JobID,
		RoundID: receipt.ReviewRoundID,
	}
}
