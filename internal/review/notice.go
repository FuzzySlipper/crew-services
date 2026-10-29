package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NoticeKind names the submission outcome a notice reports. Only gate
// failures are produced today; source_missing, stale and changes_requested
// are expected to reuse the same outbox.
type NoticeKind string

const NoticeGateFailed NoticeKind = "gate_failed"

// NoticeState is an outbox row's delivery state. pending rows are retried by
// the notice loop until the fabric accepts them (sent) or they can no longer
// be delivered usefully (abandoned).
type NoticeState string

const (
	NoticePending   NoticeState = "pending"
	NoticeSent      NoticeState = "sent"
	NoticeAbandoned NoticeState = "abandoned"
)

const (
	// NoticeTTL is the lifetime of a notice message in the fabric and also
	// how long an unaccepted notice keeps being retried.
	NoticeTTL = 24 * time.Hour
	// NoticeActivationPolicy lets a notice reach a busy session at its next
	// step instead of waiting for its turn to end.
	NoticeActivationPolicy = "next_step"
	maxNotifyAddressBytes  = 256
)

// Notice is one outbox row: a message to a submitter, fixed at the moment the
// submission outcome is recorded so every retry sends identical material.
type Notice struct {
	// OperationID is the fabric idempotency key and the outbox primary key,
	// for example submission/<id>/gate_failed/<gate handle>.
	OperationID  string
	SubmissionID string
	Kind         NoticeKind
	Recipient    string
	Body         string
	State        NoticeState
	Attempts     int
	LastError    string
	MessageID    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	SentAt       time.Time
}

// NoticeStore is the outbox. Notices are inserted only through
// SubmissionTransition.Notice, in the transaction that records the outcome.
type NoticeStore interface {
	ListPendingNotices(ctx context.Context, limit int) ([]Notice, error)
	MarkNoticeSent(ctx context.Context, operationID, messageID string) error
	MarkNoticeAttempt(ctx context.Context, operationID, failure string) error
	MarkNoticeAbandoned(ctx context.Context, operationID, reason string) error
	ListSubmissionNotices(ctx context.Context, submissionID string) ([]Notice, error)
}

// Notifier hands a notice to the messaging fabric. It must be idempotent per
// OperationID. An error wrapping ErrNoticeRejected means the fabric refused
// the notice for good; any other error is retried on the next pass.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (messageID string, err error)
}

var ErrNoticeRejected = errors.New("notice rejected by the messaging fabric")

// WithNotifier enables delivery of outbox notices. Without one, notices are
// still recorded and wait until a notifier is configured.
func WithNotifier(notifier Notifier) Option {
	return func(s *Service) { s.notifier = notifier }
}

const noticeBatch = 50

// SendNotices submits pending outbox notices to the fabric. It runs on its own
// loop so a slow or absent fabric never delays review admission. It stops at
// the first retryable failure: the fabric is most likely unavailable, and the
// remaining notices wait for the next pass.
func (s *Service) SendNotices(ctx context.Context) (bool, error) {
	if s.notifier == nil {
		return false, nil
	}
	store, ok := s.store.(NoticeStore)
	if !ok {
		return false, nil
	}
	notices, err := store.ListPendingNotices(ctx, noticeBatch)
	if err != nil {
		return false, err
	}
	for _, notice := range notices {
		if !s.clock.Now().Before(notice.CreatedAt.Add(NoticeTTL)) {
			if err := store.MarkNoticeAbandoned(ctx, notice.OperationID, "not accepted by the messaging fabric within "+NoticeTTL.String()); err != nil {
				return true, err
			}
			continue
		}
		messageID, err := s.notifier.Notify(ctx, notice)
		if err == nil {
			if markErr := store.MarkNoticeSent(ctx, notice.OperationID, messageID); markErr != nil {
				return true, markErr
			}
			continue
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if errors.Is(err, ErrNoticeRejected) {
			if markErr := store.MarkNoticeAbandoned(ctx, notice.OperationID, err.Error()); markErr != nil {
				return true, markErr
			}
			continue
		}
		if markErr := store.MarkNoticeAttempt(ctx, notice.OperationID, err.Error()); markErr != nil {
			return true, markErr
		}
		return true, fmt.Errorf("send notice %s: %w", notice.OperationID, err)
	}
	return len(notices) > 0, nil
}

// gateFailedNotice returns the outbox notice for a failed gate, or nil when
// the submission has no notify address or the gate has no Den handle.
func gateFailedNotice(record SubmissionRecord, gate GateEvidence, taskTitle string) *Notice {
	address := strings.TrimSpace(record.Request.NotifyAddress)
	if address == "" || gate.Handle == "" {
		return nil
	}
	return &Notice{
		OperationID:  "submission/" + record.ID + "/" + string(NoticeGateFailed) + "/" + gate.failureHandle(),
		SubmissionID: record.ID,
		Kind:         NoticeGateFailed,
		Recipient:    address,
		Body:         gateFailedBody(record.Request, gate, taskTitle),
		State:        NoticePending,
	}
}

func gateFailedBody(request SubmissionRequest, gate GateEvidence, taskTitle string) string {
	var b strings.Builder
	task := "#" + strconv.FormatInt(request.TaskID, 10)
	if title := strings.TrimSpace(taskTitle); title != "" {
		task += " \"" + title + "\""
	}
	status := gate.Status
	if status == "" {
		status = "failed"
	}
	fmt.Fprintf(&b, "crew-review: the required checks for task %s (%s) %s, so no reviewer was started.\n", task, request.ProjectID, gateStatusPhrase(status))
	fmt.Fprintf(&b, "Commit %s on %s in %s.\n", request.CommitSHA, request.Ref, request.Repository)
	if len(gate.FailedChecks) > 0 {
		b.WriteString("Checks that did not pass:\n")
		for _, check := range gate.FailedChecks {
			line := "- " + check.Name
			if check.Conclusion != "" {
				line += " (" + check.Conclusion + ")"
			}
			if check.URL != "" {
				line += ": " + check.URL
			}
			b.WriteString(line + "\n")
		}
	} else if summary := firstNonEmpty(gate.FailureSummary, gate.TerminalReason); summary != "" {
		b.WriteString(summary + "\n")
	}
	b.WriteString("Next: fix the cause, or re-run the failed check if it was flaky. Then call submit_task_for_review again with the same arguments (with the new commit_sha if you pushed a fix).")
	return b.String()
}

func gateStatusPhrase(status string) string {
	switch status {
	case "failed":
		return "failed"
	case "timed_out":
		return "timed out"
	default:
		return "did not pass (" + status + ")"
	}
}

func normalizeNotifyAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if len(address) > maxNotifyAddressBytes {
		return "", &SubmissionError{Code: "invalid_notify_address", Message: fmt.Sprintf("notify_address exceeds %d bytes", maxNotifyAddressBytes)}
	}
	if strings.ContainsAny(address, " \t\r\n") {
		return "", &SubmissionError{Code: "invalid_notify_address", Message: "notify_address must not contain whitespace"}
	}
	return address, nil
}
