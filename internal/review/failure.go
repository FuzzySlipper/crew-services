package review

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NoticeReviewFailed reports a reviewer job that ended Failed, so no verdict
// was recorded for its Den round.
const NoticeReviewFailed NoticeKind = "review_failed"

// FailureNoticeStore records a job failure and its submitter notice together.
type FailureNoticeStore interface {
	// SubmissionForJob returns the managed submission that admitted a job;
	// found is false for a job admitted without one.
	SubmissionForJob(ctx context.Context, jobID string) (record SubmissionRecord, found bool, err error)
	// FailWithNotice ends a queued, running or finalizing job Failed and, in
	// the same transaction, records notice when one is given. changed is
	// false, and nothing is recorded, when the job had already left those
	// states. A notice whose operation ID already exists is kept as it is.
	FailWithNotice(ctx context.Context, id, why string, notice *Notice) (job Job, changed bool, err error)
}

// TaskNoter posts a short status message on a Den task thread. A Den client
// that implements it lets a failed review reach the task's owner.
type TaskNoter interface {
	PostTaskNote(ctx context.Context, key TaskKey, content string) error
}

const (
	taskNoteTimeout     = 15 * time.Second
	failureDetailLimit  = 1200
	resubmitReruns      = "Next: call submit_task_for_review again with the same arguments; a plain resubmit re-runs this review."
	resubmitDoesNotRuns = "Next: a plain resubmit does not re-run this kind of failure. Fix the cause and submit a new commit, or ask the crew-review operator to retry job %s (POST /v1/review-jobs/%s/retry)."
)

// fail ends a job Failed and tells people: a review_failed notice to the
// submission's notify address, when one is stored, and a Den task-thread
// message for the owner. Both happen only for the transition that failed the
// job, so a failure is reported once.
func (s *Service) fail(ctx context.Context, j Job, why string) error {
	store, ok := s.store.(FailureNoticeStore)
	if !ok {
		_, err := s.store.Fail(ctx, j.ID, Failed, why)
		return err
	}
	var notice *Notice
	if record, found, err := store.SubmissionForJob(ctx, j.ID); err != nil {
		s.log("review job %s: look up its submission for the failure notice: %v", j.ID, err)
	} else if found {
		notice = reviewFailedNotice(record, j, why)
	}
	failed, changed, err := store.FailWithNotice(ctx, j.ID, why, notice)
	if err != nil || !changed {
		return err
	}
	s.postFailureNote(failed)
	return nil
}

func (s *Service) postFailureNote(j Job) {
	noter, ok := s.den.(TaskNoter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskNoteTimeout)
	defer cancel()
	if err := noter.PostTaskNote(ctx, j.Admission.Key.Task(), reviewFailedBody(j, j.Failure)); err != nil {
		s.log("review job %s: post the failure on Den task %s/%d: %v", j.ID, j.Admission.Key.ProjectID, j.Admission.Key.TaskID, err)
	}
}

// reviewFailedNotice returns the outbox notice for a failed job, or nil when
// its submission has no notify address. The operation ID names the attempt
// (the job's claim time), so a re-run that fails again is reported again.
func reviewFailedNotice(record SubmissionRecord, j Job, why string) *Notice {
	address := strings.TrimSpace(record.Request.NotifyAddress)
	if address == "" {
		return nil
	}
	return &Notice{
		OperationID:  "submission/" + record.ID + "/" + string(NoticeReviewFailed) + "/" + j.ID + "@" + strconv.FormatInt(j.UpdatedAt.UnixNano(), 10),
		SubmissionID: record.ID,
		Kind:         NoticeReviewFailed,
		Recipient:    address,
		Body:         reviewFailedBody(j, why),
		State:        NoticePending,
	}
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
