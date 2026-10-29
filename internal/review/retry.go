package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Retryable reviewer-runtime failures. A runtime marks an error with one of
// these (MarkRetryable, or %w) when the attempt failed for a reason a later
// attempt can clear. The service requeues such a job a bounded number of
// times, then fails it with a failure that a plain resubmit re-runs.
var (
	// ErrRuntimeOverloaded marks a model provider that refused the turn for
	// load: a rate limit (HTTP 429) or a model at capacity. Requeues back off.
	ErrRuntimeOverloaded = errors.New("reviewer model is rate limited or at capacity")
	// ErrNoCompletion marks a turn that ended without a structured
	// completion although the runtime itself worked.
	ErrNoCompletion = errors.New("reviewer turn ended without a completion")
	// ErrReviewDeadline is the cause of the context a reviewer turn runs
	// under once its per-review deadline passes. A runtime seeing it as
	// context.Cause should interrupt the turn before returning.
	ErrReviewDeadline = errors.New("review exceeded its deadline")
)

// DefaultReviewDeadline bounds one reviewer run when WithReviewDeadline is
// not given.
const DefaultReviewDeadline = 40 * time.Minute

// WithReviewDeadline bounds each reviewer run. On expiry the run's context is
// cancelled with ErrReviewDeadline, the runtime interrupts the turn, and the
// job is requeued once before it fails. Zero or less keeps the default.
func WithReviewDeadline(deadline time.Duration) Option {
	return func(s *Service) {
		if deadline > 0 {
			s.deadline = deadline
		}
	}
}

// WithLogger reports job claims, finishes, requeues and failures, with
// durations, through logf (for example log.Printf).
func WithLogger(logf func(format string, args ...any)) Option {
	return func(s *Service) {
		if logf != nil {
			s.logf = logf
		}
	}
}

func (s *Service) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// MarkRetryable classifies err as kind (ErrRuntimeOverloaded,
// ErrNoCompletion, ErrRuntimeInfrastructure or ErrReviewDeadline) without
// changing its text.
func MarkRetryable(err, kind error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err, kind: kind}
}

type retryableError struct{ err, kind error }

func (e retryableError) Error() string   { return e.err.Error() }
func (e retryableError) Unwrap() []error { return []error{e.err, e.kind} }

// retryPolicy bounds the attempts of one class of retryable failure. The
// first requeue waits backoff and each later one doubles it, up to
// retryBackoffMax; zero requeues at once.
type retryPolicy struct {
	class    string
	attempts int
	backoff  time.Duration
}

const retryBackoffMax = 10 * time.Minute

var (
	// The runtime has already arranged its own recovery (an app-server
	// restart), so these requeue at once.
	infraRetry = retryPolicy{class: "", attempts: maxInfraAttempts}
	// A run that hit its deadline is requeued once, then fails.
	deadlineRetry = retryPolicy{class: "review_deadline", attempts: 2}
	// The provider usually recovers in minutes; Codex has already retried
	// within the turn before reporting it.
	overloadRetry   = retryPolicy{class: "model_overloaded", attempts: 4, backoff: time.Minute}
	incompleteRetry = retryPolicy{class: "no_completion", attempts: 3, backoff: 30 * time.Second}
)

func (p retryPolicy) delay(attempt int) time.Duration {
	if p.backoff <= 0 {
		return 0
	}
	delay := p.backoff
	for i := 1; i < attempt && delay < retryBackoffMax; i++ {
		delay *= 2
	}
	return min(delay, retryBackoffMax)
}

// retryPolicyFor classifies a run error. The deadline wins over whatever the
// runtime reported while being cut off.
func retryPolicyFor(runErr error) (retryPolicy, bool) {
	switch {
	case runErr == nil:
		return retryPolicy{}, false
	case errors.Is(runErr, ErrReviewDeadline):
		return deadlineRetry, true
	case errors.Is(runErr, ErrRuntimeInfrastructure):
		return infraRetry, true
	case errors.Is(runErr, ErrRuntimeOverloaded):
		return overloadRetry, true
	case errors.Is(runErr, ErrNoCompletion):
		return incompleteRetry, true
	}
	return retryPolicy{}, false
}

func retryKey(jobID, class string) string { return jobID + "\x00" + class }

// clearRetries forgets every retry count of a job; the caller holds s.mu.
func (s *Service) clearRetriesLocked(jobID string) {
	for key := range s.infraAttempts {
		if strings.HasPrefix(key, jobID+"\x00") {
			delete(s.infraAttempts, key)
		}
	}
}

// retryOrFail requeues a job whose attempt failed retryably, after the
// policy's backoff, until the policy's attempts are used up; then it fails
// the job with a failure a plain resubmit re-runs.
func (s *Service) retryOrFail(ctx context.Context, j Job, policy retryPolicy, runErr error) error {
	s.mu.Lock()
	if s.infraAttempts == nil {
		s.infraAttempts = map[string]int{}
	}
	key := retryKey(j.ID, policy.class)
	s.infraAttempts[key]++
	attempts := s.infraAttempts[key]
	if attempts >= policy.attempts {
		s.clearRetriesLocked(j.ID)
	}
	s.mu.Unlock()
	label := policy.class
	if label == "" {
		label = "runtime_infrastructure"
	}
	if attempts < policy.attempts {
		delay := policy.delay(attempts)
		if err := s.requeueAfter(j.ID, delay); err != nil {
			return err
		}
		s.log("review job %s requeued after %s attempt %d/%d (retry in %s): %v", j.ID, label, attempts, policy.attempts, delay, runErr)
		return runErr
	}
	class := ""
	if policy.class != "" {
		class = policy.class + ": "
	}
	return s.terminal(ctx, j, Failed, fmt.Sprintf("%s%s%d attempts failed; last: %v", infraFailurePrefix, class, attempts, runErr))
}

// DelayedRequeueStore holds a requeued job from claims until a time.
type DelayedRequeueStore interface {
	RequeueAfter(ctx context.Context, id string, at time.Time) (Job, error)
}

func (s *Service) requeueAfter(id string, delay time.Duration) error {
	if delay > 0 {
		if delayed, ok := s.store.(DelayedRequeueStore); ok {
			_, err := delayed.RequeueAfter(context.Background(), id, s.clock.Now().Add(delay))
			return err
		}
	}
	_, err := s.store.Requeue(context.Background(), id)
	return err
}
