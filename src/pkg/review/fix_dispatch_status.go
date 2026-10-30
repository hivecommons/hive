package review

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/escalation"
)

// FixDispatchStatus is the fixer's standing on one PR head, read from the
// persisted dispatch state: is a fix job in flight, and if not, why the
// dispatcher refuses to start one (hivecommons/hive#15744). The stage engine
// and the tracker consult it so a 070 row never claims "the hive is fixing
// it" when the fixer has in fact given up or is otherwise excluded.
type FixDispatchStatus struct {
	// Active is true when a pending fix for this head is on the books.
	Active bool
	// Agent is the fixer holding the pending fix (Active only).
	Agent string
	// Reason is the FixDispatchExcluded* reason when no fix is active and the
	// dispatcher would refuse to start one; "" when a fix could be kicked.
	Reason string
	// Attempts is the highest attempt number recorded for the PR.
	Attempts int
}

// Exhausted reports whether the fixer has hit the re-engagement cap.
func (s FixDispatchStatus) Exhausted() bool {
	return !s.Active && s.Reason == FixDispatchExcludedAttemptsExhausted
}

// Excluded reports whether no fix is active and a reason bars dispatch.
func (s FixDispatchStatus) Excluded() bool {
	return !s.Active && s.Reason != ""
}

// FixDispatchStatusFor derives the fixer's standing on repo#number at head
// from state. Repo names are matched loosely (bare vs owner/name) because the
// dispatch state records the org-qualified form while callers may hold
// either. A head of "" matches any head of the PR.
func FixDispatchStatusFor(state DispatchState, repo string, number int, head string) FixDispatchStatus {
	var out FixDispatchStatus
	if number <= 0 {
		return out
	}
	headMatches := func(sha string) bool {
		return strings.TrimSpace(head) == "" || SameHeadSHA(sha, head)
	}
	for _, f := range state.Fixes {
		if f.Number != number || !sameRepoLoose(f.Repo, repo) {
			continue
		}
		if f.Attempts > out.Attempts {
			out.Attempts = f.Attempts
		}
		if headMatches(f.HeadSHA) {
			out.Active, out.Agent = true, f.Agent
		}
	}
	for _, f := range state.FixAttempts {
		if f.Number == number && sameRepoLoose(f.Repo, repo) && f.Attempts > out.Attempts {
			out.Attempts = f.Attempts
		}
	}
	if out.Active {
		return out
	}
	if out.Attempts >= escalation.MaxReEngagements {
		out.Reason = FixDispatchExcludedAttemptsExhausted
		return out
	}
	for _, h := range state.Human {
		if h.Number != number || !sameRepoLoose(h.Repo, repo) || !headMatches(h.HeadSHA) {
			continue
		}
		switch {
		case h.Reason == FixAttemptsExhaustedHumanDecisionReason:
			out.Reason = FixDispatchExcludedAttemptsExhausted
			if out.Attempts < escalation.MaxReEngagements {
				out.Attempts = escalation.MaxReEngagements
			}
			return out
		case strings.Contains(h.Reason, "no enabled kick-capable fixer"):
			out.Reason = FixDispatchExcludedNoEnabledFixer
			return out
		}
	}
	for _, w := range state.Withheld {
		if w.Number == number && sameRepoLoose(w.Repo, repo) && headMatches(w.HeadSHA) {
			out.Reason = FixDispatchExcludedNotHiveAuthored
			return out
		}
	}
	return out
}

// FixDispatchExclusionText renders a dispatch exclusion reason as the short
// phrase the tracker caption and the 085 decision comment use.
func FixDispatchExclusionText(s FixDispatchStatus) string {
	switch s.Reason {
	case FixDispatchExcludedAttemptsExhausted:
		return "fixer gave up after " + strconv.Itoa(s.Attempts) + " attempts"
	case FixDispatchExcludedNoEnabledFixer:
		return "no enabled fixer agent"
	case FixDispatchExcludedNotHiveAuthored, FixDispatchExcludedNotHiveAuthoredDCO:
		return "not hive-authored — the hive does not push to it"
	case FixDispatchExcludedPending:
		return "fix already dispatched"
	case "":
		return ""
	}
	return s.Reason
}
