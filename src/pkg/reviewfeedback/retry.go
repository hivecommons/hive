package reviewfeedback

import (
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Stranded feedback (hivecommons/hive#19320): a kick that the agent accepted
// but then did nothing with (no push, no reply) used to leave the routed
// comments recorded as handled forever. The sweep now remembers what each kick
// routed and the PR head at that moment, and on a later pass re-routes the
// comments that are still unanswered when nothing happened, up to a cap with
// a doubling backoff, then hands them to a person.

// The knobs are named in pkg/config so the dashboard can show them without
// importing this package.
const (
	// RetryMaxEnvVar overrides the retry cap (0 turns re-routing off).
	RetryMaxEnvVar = config.ReviewRetryMaxEnvVar
	// DefaultRetryMax is how many times stranded feedback is routed again.
	DefaultRetryMax = config.ReviewRetryDefaultMax
	// RetryBackoffEnvVar overrides the base backoff.
	RetryBackoffEnvVar = config.ReviewRetryBackoffEnvVar
	// DefaultRetryBackoff is how long after a kick the sweep waits before
	// judging it stranded; it doubles on each retry.
	DefaultRetryBackoff = config.ReviewRetryDefaultBackoff
	// MinRetryBackoff keeps a misconfigured backoff from re-kicking an agent
	// that is still working on the first brief.
	MinRetryBackoff = config.ReviewRetryMinBackoff
	// MaxRetryMax bounds the cap an owner may set.
	MaxRetryMax = config.ReviewRetryMaxMax
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
	maxRetries, backoff := config.ReviewRetryFromEnv()
	return RetryPolicy{MaxRetries: maxRetries, Backoff: backoff}.Normalized()
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

// StrandedHandoff is the durable note for a kick that exhausted its retries.
// Body is reused verbatim so the GitHub client can reconcile an ambiguous POST.
type StrandedHandoff struct {
	Body     string    `json:"body"`
	IDs      []string  `json:"ids"`
	KickedAt time.Time `json:"kicked_at"`
	Kicks    int       `json:"kicks"`
	// Ready sticks once a known head and the backoff allow the first POST.
	Ready bool `json:"ready,omitempty"`
}

// CheckStranded judges the last kick on a PR. head is the PR head SHA now,
// lastReply when the hive's identity last posted on it, and unanswered the
// comments the PR still shows unanswered. When the head moved or the hive
// replied after the kick, the agent acted and the kick is closed. When none of
// the routed comments is still unanswered, likewise. Otherwise, once the
// backoff has passed, a retry un-marks the still-unanswered ids so Plan routes
// them again, or an exhausted cap queues a needs-human note.
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
	restoreHandled := func() {
		if !st.RetryPending {
			return
		}
		st = s.get(key) // A reopened store may omit an empty Handled map.
		for _, id := range routed {
			st.Handled[id] = st.LastKick
		}
	}
	closeKick := func(check StrandedCheck) (StrandedCheck, error) {
		restoreHandled()
		st.StrandedClosed, st.RetryPending, st.Retries = true, false, 0
		return check, s.saveLocked(now)
	}
	if len(still) == 0 || lastReply.After(st.LastKick) || (head != "" && st.LastKickHead != "" && head != st.LastKickHead) {
		return closeKick(StrandedCheck{Verdict: StrandedNone})
	}
	due := head != "" && now.Sub(st.LastKick) >= p.wait(st.Retries)
	// A refused retry already passed a valid head check. If the cap is now
	// spent, reserve its IDs while waiting for a head or a changed backoff.
	if st.LastKickHead != "" && st.Retries >= p.MaxRetries && (due || st.RetryPending) {
		if !st.NeedsHumanNoted {
			kicks := st.Retries + 1
			st.PendingHandoffs = append(st.PendingHandoffs, StrandedHandoff{
				Body: NeedsHumanNote(StrandedReason(kicks), ItemsByID(unanswered, still), nil),
				IDs:  still, KickedAt: st.LastKick, Kicks: kicks, Ready: due,
			})
		}
		st.HandoffCheckedAt = now
		return closeKick(StrandedCheck{Verdict: StrandedExhausted, IDs: still, Retries: st.Retries})
	}
	if head == "" {
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, nil
	}
	if st.LastKickHead == "" {
		// Persist the derived ids with the baseline: resetting LastKick
		// breaks the "stamped at the kick time" derivation above.
		st.LastKickIDs = append([]string(nil), routed...)
		sort.Strings(st.LastKickIDs)
		st.LastKickHead, st.LastKick = head, now
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, s.saveLocked(now)
	}
	if !due {
		return StrandedCheck{Verdict: StrandedWaiting, IDs: still, Retries: st.Retries}, nil
	}
	for _, id := range still {
		delete(st.Handled, id)
	}
	st.RetryPending = true
	return StrandedCheck{Verdict: StrandedRetry, IDs: still, Retries: st.Retries}, s.saveLocked(now)
}

// NextStrandedHandoff returns the oldest pending note and whether it is now
// obsolete. Save before returning a body so every POST has a durable record.
func (s *Store) NextStrandedHandoff(key string, fb github.PRFeedback, now time.Time, p RetryPolicy) (*StrandedHandoff, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.PRs[key]
	if st == nil || len(st.PendingHandoffs) == 0 {
		return nil, false, nil
	}
	next := &st.PendingHandoffs[0]
	obsolete := st.handoffComplete(*next, fb)
	if !obsolete && !next.Ready && fb.HeadSHA != "" && now.Sub(next.KickedAt) >= p.Normalized().wait(next.Kicks-1) {
		next.Ready = true
	}
	st.HandoffCheckedAt = now
	if err := s.saveLocked(now); err != nil {
		return nil, false, err
	}
	note := *next
	return &note, obsolete, nil
}

// DiscardStrandedHandoff drops the oldest pending note after the caller checks
// that it is obsolete and was not posted.
func (s *Store) DiscardStrandedHandoff(key string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.PRs[key]
	if st == nil || len(st.PendingHandoffs) == 0 {
		return nil
	}
	st.PendingHandoffs[0] = StrandedHandoff{}
	st.PendingHandoffs = st.PendingHandoffs[1:]
	return s.saveLocked(now)
}

func (st *PRState) handoffComplete(note StrandedHandoff, fb github.PRFeedback) bool {
	// Head and reply signals belong to the latest kick. A later kick for
	// other feedback may have produced progress without addressing this note.
	if note.KickedAt.Equal(st.LastKick) && (fb.LastHiveReplyAt.After(note.KickedAt) ||
		(fb.HeadSHA != "" && fb.HeadSHA != st.LastKickHead)) {
		return true
	}
	// RuleCandidates includes visible feedback even after it was answered.
	// An ID outside both lists could be hidden by GitHub's bounded read.
	resolved := make(map[string]bool, len(fb.RuleCandidates))
	for _, item := range fb.RuleCandidates {
		resolved[item.ID] = true
	}
	for _, item := range fb.Items {
		delete(resolved, item.ID)
	}
	for _, id := range note.IDs {
		if !resolved[id] {
			return false
		}
	}
	return true
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
