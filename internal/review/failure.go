package review

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// NoticeReviewFailed reports a reviewer job that ended Failed, so no verdict
// was recorded for its Den round.
const NoticeReviewFailed NoticeKind = "review_failed"

// FailureNoticeStore records a job failure and its notices together.
type FailureNoticeStore interface {
	// SubmissionForJob returns the managed submission that admitted a job;
	// found is false for a job admitted without one.
	SubmissionForJob(ctx context.Context, jobID string) (record SubmissionRecord, found bool, err error)
	// FailWithNotices ends a queued, running or finalizing job Failed and, in
	// the same transaction, records notices in the outbox. changed is false,
	// and nothing is recorded, when the job had already left those states. A
	// notice whose operation ID already exists is kept as it is.
	FailWithNotices(ctx context.Context, id, why string, notices []*Notice) (job Job, changed bool, err error)
}

const (
	failureDetailLimit  = 1200
	resubmitReruns      = "Next: call submit_task_for_review again with the same arguments; a plain resubmit re-runs this review."
	resubmitDoesNotRuns = "Next: a plain resubmit does not re-run this kind of failure. Fix the cause and submit a new commit, or ask the crew-review operator to retry job %s (POST /v1/review-jobs/%s/retry)."
)

// fail ends a job Failed and tells people through the outbox: a
// review_failed notice to the submission's notify address, when one is
// stored, and a Den task-thread note for the owner. Both are recorded only
// by the transition that failed the job, so a failure is reported once.
func (s *Service) fail(ctx context.Context, j Job, why string) error {
	store, ok := s.store.(FailureNoticeStore)
	if !ok {
		_, err := s.store.Fail(ctx, j.ID, Failed, why)
		return err
	}
	record, found, err := store.SubmissionForJob(ctx, j.ID)
	if err != nil {
		s.log("review job %s: look up its submission for the failure notice: %v", j.ID, err)
		found = false
	}
	_, _, err = store.FailWithNotices(ctx, j.ID, why, reviewFailedNotices(record, found, j, why))
	return err
}

// reviewFailedNotices returns the outbox notices for a failed job: one to the
// submission's notify address when it has one, and one Den task-thread note.
// The operation IDs name the attempt (the job's claim time), so a re-run that
// fails again is reported again.
func reviewFailedNotices(record SubmissionRecord, found bool, j Job, why string) []*Notice {
	attempt := j.ID + "@" + strconv.FormatInt(j.UpdatedAt.UnixNano(), 10)
	base, submissionID := "job/"+attempt+"/"+string(NoticeReviewFailed), ""
	if found {
		base, submissionID = "submission/"+record.ID+"/"+string(NoticeReviewFailed)+"/"+attempt, record.ID
	}
	body := reviewFailedBody(j, why)
	key := j.Admission.Key
	notices := []*Notice{{
		OperationID:  base + "/den-task",
		SubmissionID: submissionID,
		Kind:         NoticeReviewFailed,
		Recipient:    denTaskRecipientPrefix + key.ProjectID + "/" + strconv.FormatInt(key.TaskID, 10),
		Body:         body,
		State:        NoticePending,
	}}
	if address := strings.TrimSpace(record.Request.NotifyAddress); found && address != "" {
		notices = append(notices, &Notice{
			OperationID:  base,
			SubmissionID: submissionID,
			Kind:         NoticeReviewFailed,
			Recipient:    address,
			Body:         body,
			State:        NoticePending,
		})
	}
	return notices
}

func reviewFailedBody(j Job, why string) string {
	key := j.Admission.Key
	var b strings.Builder
	fmt.Fprintf(&b, "crew-review: the review of task #%d (%s), review round %d, failed, so no verdict was recorded.\n", key.TaskID, key.ProjectID, key.ReviewRoundID)
	if source := j.Admission.Source; source != nil && source.CommitSHA != "" {
		fmt.Fprintf(&b, "Commit %s", source.CommitSHA)
		if source.Ref != "" {
			fmt.Fprintf(&b, " on %s", source.Ref)
		}
		fmt.Fprintf(&b, " in %s.\n", source.Repository)
	}
	detail := strings.Join(strings.Fields(why), " ")
	if len(detail) > failureDetailLimit {
		detail = strings.ToValidUTF8(detail[:failureDetailLimit], "") + "…"
	}
	fmt.Fprintf(&b, "What failed: %s\n", detail)
	if strings.HasPrefix(why, infraFailurePrefix) {
		b.WriteString(resubmitReruns)
	} else {
		fmt.Fprintf(&b, resubmitDoesNotRuns, j.ID, j.ID)
	}
	return b.String()
}
