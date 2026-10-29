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
// failures and final gate timeouts are produced today; source_missing, stale
// and changes_requested are expected to reuse the same outbox.
type NoticeKind string

const (
	NoticeGateFailed NoticeKind = "gate_failed"
	// NoticeGateTimedOut reports a gate that timed out again after
	// crew-review re-registered it once on its own. A first timeout is
	// re-registered instead of reported.
	NoticeGateTimedOut NoticeKind = "gate_timed_out"
)

// denTaskRecipientPrefix marks an outbox notice that is posted on the Den
// task thread instead of sent through the messaging fabric. The rest of the
// recipient is "<project_id>/<task_id>".
const denTaskRecipientPrefix = "den-task:"

// TaskNotePoster posts a short crew-review status note on a Den task
// thread. A Den adapter that implements it receives den-task notices.
type TaskNotePoster interface {
	PostTaskNote(ctx context.Context, key TaskKey, content string) error
}

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
	// noticeRetryBase and noticeRetryMax bound the backoff of a notice whose
	// recipient the fabric does not know: the first retry waits
	// noticeRetryBase and each further one doubles, up to noticeRetryMax.
	noticeRetryBase = 30 * time.Second
	noticeRetryMax  = 15 * time.Minute
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
	// NextAttemptAt is when a pending notice is next due for a send.
	NextAttemptAt time.Time
}

// NoticeStore is the outbox. Notices are inserted only through
// SubmissionTransition.Notice, in the transaction that records the outcome.
type NoticeStore interface {
	ListPendingNotices(ctx context.Context, limit int) ([]Notice, error)
	MarkNoticeSent(ctx context.Context, operationID, messageID string) error
	MarkNoticeAttempt(ctx context.Context, operationID, failure string, nextAttemptAt time.Time) error
	MarkNoticeAbandoned(ctx context.Context, operationID, reason string) error
	ListSubmissionNotices(ctx context.Context, submissionID string) ([]Notice, error)
}

// Notifier hands a notice to the messaging fabric. It must be idempotent per
// OperationID. An error wrapping ErrNoticeRejected means the fabric refused
// the notice for good. An error wrapping ErrRecipientUnavailable concerns
// only this notice's recipient: it is retried with backoff while other
// notices proceed. Any other error means the fabric itself is unavailable.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (messageID string, err error)
}

var (
	ErrNoticeRejected       = errors.New("notice rejected by the messaging fabric")
	ErrRecipientUnavailable = errors.New("notice recipient is not registered with the messaging fabric")
)

// WithNotifier enables delivery of outbox notices. Without one, notices are
// still recorded and wait until a notifier is configured.
func WithNotifier(notifier Notifier) Option {
	return func(s *Service) { s.notifier = notifier }
}

const noticeBatch = 50

// SendNotices submits due outbox notices to the fabric. It runs on its own
// loop so a slow or absent fabric never delays review admission. A notice
// whose recipient is not registered is rescheduled with backoff and the pass
// continues, so one dead recipient never holds back other notices. A failure
// of the fabric itself stops the pass: the remaining notices wait for the
// next one, and the failed notice is requeued behind them.
func (s *Service) SendNotices(ctx context.Context) (bool, error) {
	poster, _ := s.den.(TaskNotePoster)
	if s.notifier == nil && poster == nil {
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
	var deferred []error
	for _, notice := range notices {
		expiry := notice.CreatedAt.Add(NoticeTTL)
		if !s.clock.Now().Before(expiry) {
			if err := store.MarkNoticeAbandoned(ctx, notice.OperationID, "not accepted by the messaging fabric within "+NoticeTTL.String()); err != nil {
				return true, err
			}
			continue
		}
		if key, isTaskNote := denTaskNoteKey(notice.Recipient); isTaskNote {
			if poster == nil || key.TaskID <= 0 {
				if err := store.MarkNoticeAbandoned(ctx, notice.OperationID, "the Den adapter cannot post this task note"); err != nil {
					return true, err
				}
				continue
			}
			// A Den failure concerns only task notes: they back off like an
			// unregistered recipient while fabric notices proceed.
			if err := poster.PostTaskNote(ctx, key, notice.Body); err != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				retryAt := s.clock.Now().Add(noticeRetryDelay(notice.Attempts))
				if retryAt.After(expiry) {
					retryAt = expiry
				}
				if markErr := store.MarkNoticeAttempt(ctx, notice.OperationID, err.Error(), retryAt); markErr != nil {
					return true, markErr
				}
				deferred = append(deferred, fmt.Errorf("defer Den task note %s until %s: %w", notice.OperationID, retryAt.UTC().Format(time.RFC3339), err))
				continue
			}
			if err := store.MarkNoticeSent(ctx, notice.OperationID, ""); err != nil {
				return true, err
			}
			continue
		}
		if s.notifier == nil {
			// Fabric delivery is not configured; the notice waits for it.
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
		if errors.Is(err, ErrRecipientUnavailable) {
			// Never schedule past the expiry, so the notice is still listed
			// and abandoned on time.
			retryAt := s.clock.Now().Add(noticeRetryDelay(notice.Attempts))
			if retryAt.After(expiry) {
				retryAt = expiry
			}
			if markErr := store.MarkNoticeAttempt(ctx, notice.OperationID, err.Error(), retryAt); markErr != nil {
				return true, markErr
			}
			deferred = append(deferred, fmt.Errorf("defer notice %s until %s: %w", notice.OperationID, retryAt.UTC().Format(time.RFC3339), err))
			continue
		}
		if markErr := store.MarkNoticeAttempt(ctx, notice.OperationID, err.Error(), s.clock.Now()); markErr != nil {
			return true, markErr
		}
		return true, fmt.Errorf("send notice %s: %w", notice.OperationID, err)
	}
	// Deferred notices are reported for the log; they did not stop the pass.
	return len(notices) > 0, errors.Join(deferred...)
}

// noticeRetryDelay is the backoff after the given number of earlier failed
// attempts to reach an unregistered recipient.
func noticeRetryDelay(attempts int) time.Duration {
	delay := noticeRetryBase
	for i := 0; i < attempts && delay < noticeRetryMax; i++ {
		delay *= 2
	}
	return min(delay, noticeRetryMax)
}

// gateFailedNotice returns the outbox notice for a failed gate, or nil when
// the submission has no notify address or the gate has no Den handle. A
// timed-out gate is reported as gate_timed_out.
func gateFailedNotice(record SubmissionRecord, gate GateEvidence, taskTitle string) *Notice {
	address := strings.TrimSpace(record.Request.NotifyAddress)
	if address == "" || gate.Handle == "" {
		return nil
	}
	kind := NoticeGateFailed
	if gate.Status == "timed_out" {
		kind = NoticeGateTimedOut
	}
	return &Notice{
		OperationID:  "submission/" + record.ID + "/" + string(kind) + "/" + gate.failureHandle(),
		SubmissionID: record.ID,
		Kind:         kind,
		Recipient:    address,
		Body:         gateFailedBody(record, gate, taskTitle),
		State:        NoticePending,
	}
}

// gateTimedOutTaskNote returns the Den task-thread note for a final gate
// timeout, so the task owner learns why no reviewer started even when the
// submitter left no notify address. Other gate failures get none: Den's own
// gate evidence on the thread already names the failed checks, and only a
// timeout leaves the submission waiting on nobody.
func gateTimedOutTaskNote(record SubmissionRecord, gate GateEvidence, taskTitle string) *Notice {
	if gate.Status != "timed_out" || gate.Handle == "" {
		return nil
	}
	return &Notice{
		OperationID:  "submission/" + record.ID + "/" + string(NoticeGateTimedOut) + "/" + gate.failureHandle() + "/den-task",
		SubmissionID: record.ID,
		Kind:         NoticeGateTimedOut,
		Recipient:    denTaskRecipientPrefix + record.Request.ProjectID + "/" + strconv.FormatInt(record.Request.TaskID, 10),
		Body:         gateFailedBody(record, gate, taskTitle),
		State:        NoticePending,
	}
}

// denTaskNoteKey parses a den-task notice recipient. ok reports the prefix;
// a malformed remainder returns a zero key.
func denTaskNoteKey(recipient string) (key TaskKey, ok bool) {
	rest, ok := strings.CutPrefix(recipient, denTaskRecipientPrefix)
	if !ok {
		return TaskKey{}, false
	}
	slash := strings.LastIndex(rest, "/")
	if slash <= 0 {
		return TaskKey{}, true
	}
	taskID, err := strconv.ParseInt(rest[slash+1:], 10, 64)
	if err != nil || taskID <= 0 {
		return TaskKey{}, true
	}
	return TaskKey{ProjectID: rest[:slash], TaskID: taskID}, true
}

func gateFailedBody(record SubmissionRecord, gate GateEvidence, taskTitle string) string {
	request := record.Request
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
	if status == "timed_out" && record.GateAutoRechecks > 0 {
		b.WriteString("crew-review already re-registered this gate once after its first timeout, and it timed out again.\n")
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
	if strings.HasPrefix(address, denTaskRecipientPrefix) {
		return "", &SubmissionError{Code: "invalid_notify_address", Message: "notify_address must be a messaging fabric address"}
	}
	return address, nil
}
