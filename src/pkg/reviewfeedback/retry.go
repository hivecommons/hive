package reviewfeedback

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Stranded feedback (hivecommons/hive#19320): a kick that the agent accepted
// but then did nothing with (no push, no reply) used to leave the routed
// comments recorded as handled forever. The sweep now remembers what each kick
// routed and the PR head at that moment, and on a later pass re-routes the
// comments that are still unanswered when nothing happened, up to a cap with
// a doubling backoff, then hands them to a person.

const (
	// RetryMaxEnvVar overrides the retry cap (0 turns re-routing off).
	RetryMaxEnvVar = "HIVE_REVIEW_FEEDBACK_RETRY_MAX"
	// DefaultRetryMax is how many times stranded feedback is routed again.
	DefaultRetryMax = 2
	// RetryBackoffEnvVar overrides the base backoff.
	RetryBackoffEnvVar = "HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF"
	// DefaultRetryBackoff is how long after a kick the sweep waits before
	// judging it stranded; it doubles on each retry.
	DefaultRetryBackoff = time.Hour
	// MinRetryBackoff keeps a misconfigured backoff from re-kicking an agent
	// that is still working on the first brief.
	MinRetryBackoff = 10 * time.Minute
	// MaxRetryMax bounds the cap an owner may set.
	MaxRetryMax = 10
)

// Verdicts of CheckStranded.
const (
	StrandedNone      = "none"
	StrandedWaiting   = "waiting"
	StrandedRetry     = "retry"
	StrandedExhausted = "exhausted"
)

// RetryPolicy is the stranded-feedback knobs.
type RetryPolicy struct {
	// MaxRetries is the number of re-routes before a person is asked; 0
	// hands stranded feedback to a person on the first check.
	MaxRetries int
	// Backoff is the wait after the first kick; retry n waits Backoff*2^n.
	Backoff time.Duration
}

// RetryPolicyFromEnv reads the knobs, falling back to the defaults.
func RetryPolicyFromEnv() RetryPolicy {
	p := RetryPolicy{MaxRetries: DefaultRetryMax, Backoff: DefaultRetryBackoff}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(RetryMaxEnvVar))); err == nil && n >= 0 {
		p.MaxRetries = n
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(RetryBackoffEnvVar))); err == nil && d > 0 {
		p.Backoff = d
	}
	return p.Normalized()
}

// Normalized clamps the knobs into their valid ranges.
func (p RetryPolicy) Normalized() RetryPolicy {
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRetries > MaxRetryMax {
		p.MaxRetries = MaxRetryMax
	}
	if p.Backoff <= 0 {
		p.Backoff = DefaultRetryBackoff
	}
	if p.Backoff < MinRetryBackoff {
		p.Backoff = MinRetryBackoff
	}
	return p
}

// wait is the backoff before judging the kick after retries re-routes.
func (p RetryPolicy) wait(retries int) time.Duration {
	d := p.Backoff
	for i := 0; i < retries && d < 30*24*time.Hour; i++ {
		d *= 2
	}
	return d
}

// StrandedCheck is CheckStranded's answer.
type StrandedCheck struct {
	Verdict string
	// IDs are the routed comment ids still unanswered (retry, exhausted).
	IDs []string
	// Retries is how many re-routes were already spent.
	Retries int
}

// CheckStranded judges the last kick on a PR. head is the PR head SHA now,
// lastReply when the hive's identity last posted on it, and unanswered the
// comments the PR still shows unanswered. When the head moved or the hive
// replied after the kick, the agent acted and the kick is closed. When none of
// the routed comments is still unanswered, likewise. Otherwise, once the
// backoff has passed, a retry un-marks the still-unanswered ids so Plan routes
// them again, and an exhausted cap returns them for a needs-human note.
//
// A kick recorded before this check existed has no head; the current head is
// taken as its baseline and the backoff restarts, so feedback stranded before
// an upgrade is still recovered.
func (s *Store) CheckStranded(key, head string, lastReply time.Time, unanswered []github.FeedbackItem, now time.Time, p RetryPolicy) (StrandedCheck, error) {
	p = p.Normalized()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.PRs[key]
	if st == nil || st.LastKick.IsZero() || st.StrandedClosed {
		return StrandedCheck{Verdict: StrandedNone}, nil
	}
	routed := st.LastKickIDs
	if len(routed) == 0 {
		// Legacy record: the ids routed by the last kick carry its time.
		for id, at := range st.Handled {
			if at.Equal(st.LastKick) {
				routed = append(routed, id)
			}
		}
	}
	open := map[string]bool{}
	for _, it := range unanswered {
		open[it.ID] = true
	}
	var still []string
	for _, id := range routed {
		if open[id] {
			still = append(still, id)
		}
	}
	sort.Strings(still)
	closeKick := func() (StrandedCheck, error) {
		st.StrandedClosed, st.RetryPending, st.Retries = true, false, 0
		return StrandedCheck{Verdict: StrandedNone}, s.saveLocked(now)
	}
	if len(still) == 0 || lastReply.After(st.LastKick) {
		return closeKick()
	}
	if head == "" {
		// The read did not report a head: nothing can be judged this pass.
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, nil
	}
	if st.LastKickHead == "" {
		st.LastKickHead, st.LastKick = head, now
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, s.saveLocked(now)
	}
	if head != st.LastKickHead {
		return closeKick()
	}
	if now.Sub(st.LastKick) < p.wait(st.Retries) {
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, nil
	}
	if st.Retries >= p.MaxRetries {
		retries := st.Retries
		st.StrandedClosed, st.RetryPending = true, false
		return StrandedCheck{Verdict: StrandedExhausted, IDs: still, Retries: retries}, s.saveLocked(now)
	}
	for _, id := range still {
		delete(st.Handled, id)
	}
	st.RetryPending = true
	return StrandedCheck{Verdict: StrandedRetry, IDs: still, Retries: st.Retries}, s.saveLocked(now)
}

// StrandedReason is the needs-human reason for feedback that stayed stranded
// after every retry.
func StrandedReason(kicks int) string {
	return "it was routed to its agent " + strconv.Itoa(kicks) + " time(s) and each run ended with no push and no reply on this PR"
}

// ItemsByID keeps the items whose id is in ids, in their order.
func ItemsByID(items []github.FeedbackItem, ids []string) []github.FeedbackItem {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []github.FeedbackItem
	for _, it := range items {
		if want[it.ID] {
			out = append(out, it)
		}
	}
	return out
}
