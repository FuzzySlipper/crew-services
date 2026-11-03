package review

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// NoticeChangesRequested wakes the session that submitted the task, or
	// the project's lead seat when that session is gone.
	NoticeChangesRequested NoticeKind = "changes_requested"
	// NoticePassed tells the addresses a Den project lists in its
	// crew_notify on_done setting that a task passed.
	NoticePassed NoticeKind = "passed"

	// verdictFindingsShown bounds the findings listed in a notice; the rest
	// are on the task.
	verdictFindingsShown = 10
	reviewLoopCap        = "After 12 review rounds on one task, or sooner when the same finding comes back after two fixes, stop resubmitting: post the disagreement on the task thread and notify the user (_global/den-operating-contract)."
)

// CrewNotifyTarget is one crew address a Den project asks to be told about
// its tasks passing review, with the activation policy to use.
type CrewNotifyTarget struct {
	Address string `json:"address"`
	Policy  string `json:"policy,omitempty"`
}

// ProjectNotifyReader reads a project's crew_notify on_done list from Den. A
// Den without the setting answers with none and no error.
type ProjectNotifyReader interface {
	DoneNotifyTargets(ctx context.Context, projectID string) ([]CrewNotifyTarget, error)
}

// VerdictNoticeStore stores a job's receipt and its verdict notices in one
// transaction.
type VerdictNoticeStore interface {
	SubmissionForJob(ctx context.Context, jobID string) (SubmissionRecord, bool, error)
	CompleteWithNotices(ctx context.Context, id string, r Receipt, notices []*Notice) (Job, error)
}

// complete stores Den's receipt for a finalized job, with its verdict notices
// when the store can record them atomically.
func (s *Service) complete(ctx context.Context, j Job, r Receipt) (Job, error) {
	store, ok := s.store.(VerdictNoticeStore)
	if !ok {
		return s.store.Complete(ctx, j.ID, r)
	}
	record, found, err := store.SubmissionForJob(ctx, j.ID)
	if err != nil {
		s.log("review job %s: look up its submission for the verdict notice: %v", j.ID, err)
		found = false
	}
	var targets []CrewNotifyTarget
	if r.Verdict == "looks_good" {
		if reader, ok := s.den.(ProjectNotifyReader); ok {
			if targets, err = reader.DoneNotifyTargets(ctx, j.Admission.Key.ProjectID); err != nil {
				s.log("review job %s: read the project's crew_notify setting: %v", j.ID, err)
				targets = nil
			}
		}
	}
	return store.CompleteWithNotices(ctx, j.ID, r, verdictNotices(j, r, record, found, targets))
}

var seatScope = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// leadSeat is the project's lead seat, <project>.lead, or "" when the project
// ID cannot be a seat scope.
func leadSeat(projectID string) string {
	if !seatScope.MatchString(projectID) {
		return ""
	}
	return projectID + ".lead"
}

// verdictNotices returns the outbox notices for a stored verdict.
// changes_requested goes, as next_turn, to the submission's notify address
// with the lead seat as its fallback, or to the lead seat when there is no
// address. looks_good goes only to the project's crew_notify targets.
func verdictNotices(j Job, r Receipt, record SubmissionRecord, found bool, targets []CrewNotifyTarget) []*Notice {
	key := j.Admission.Key
	base := "job/" + j.ID + "/"
	submissionID := ""
	if found {
		submissionID = record.ID
	}
	switch r.Verdict {
	case "changes_requested":
		recipient, fallback := "", leadSeat(key.ProjectID)
		if found {
			recipient = strings.TrimSpace(record.Request.NotifyAddress)
		}
		if recipient == "" {
			recipient, fallback = fallback, ""
		}
		if recipient == "" {
			return nil
		}
		return []*Notice{{
			OperationID:  base + string(NoticeChangesRequested),
			SubmissionID: submissionID,
			Kind:         NoticeChangesRequested,
			Recipient:    recipient,
			Fallback:     fallback,
			Policy:       "next_turn",
			Body:         changesRequestedBody(j),
			State:        NoticePending,
		}}
	case "looks_good":
		var notices []*Notice
		seen := map[string]bool{}
		for _, target := range targets {
			address := strings.TrimSpace(target.Address)
			if address == "" || seen[address] {
				continue
			}
			seen[address] = true
			policy := "no_wake"
			if target.Policy == "next_turn" {
				policy = "next_turn"
			}
			notices = append(notices, &Notice{
				OperationID:  base + string(NoticePassed) + "/" + address,
				SubmissionID: submissionID,
				Kind:         NoticePassed,
				Recipient:    address,
				Policy:       policy,
				Body:         passedBody(key, address),
				State:        NoticePending,
			})
		}
		return notices
	}
	return nil
}

func changesRequestedBody(j Job) string {
	key := j.Admission.Key
	var findings []NewFinding
	if j.Finalization != nil {
		findings = j.Finalization.Completion.NewFindings
	}
	var b strings.Builder
	fmt.Fprintf(&b, "crew-review: changes requested on Den %s task %d (review round %d), %d new %s.\n", key.ProjectID, key.TaskID, key.ReviewRoundID, len(findings), map[bool]string{true: "finding", false: "findings"}[len(findings) == 1])
	for i, f := range findings {
		if i == verdictFindingsShown {
			fmt.Fprintf(&b, "- … and %d more on the task.\n", len(findings)-i)
			break
		}
		summary := strings.Join(strings.Fields(f.Summary), " ")
		if len(summary) > 200 {
			summary = summary[:200] + "…"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", f.Category, summary)
	}
	fmt.Fprintf(&b, "\nNext: read them with list_review_findings (task_id %d), respond on each finding, fix, commit with (#%d) in the subject, push, and submit_task_for_review again with the same arguments. It came to this session because it submitted the task, or to the project's lead seat when that session was gone.\n", key.TaskID, key.TaskID)
	b.WriteString(reviewLoopCap)
	return b.String()
}

func passedBody(key Key, address string) string {
	return "crew-review: Den " + key.ProjectID + " task " + strconv.FormatInt(key.TaskID, 10) + " passed review (round " + strconv.FormatInt(key.ReviewRoundID, 10) + ") and is done. " +
		"This came to " + address + " because the project's crew_notify on_done setting lists it."
}
