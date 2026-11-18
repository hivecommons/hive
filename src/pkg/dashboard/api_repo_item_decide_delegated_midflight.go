package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// delegatedMidFlight is the server-side twin of the row's `mid_flight`
// (hivecommons/hive#18889): what the PR's CURRENT head still waits on before
// a delegated decision means anything. CI comes from the live read the
// gather just made (Input.CIPending); the review track and any activity
// recorded for an earlier head come from the last tracker body, measured
// against the live head — a push since that body was served reads as a head
// whose review has not run. nil when nothing is outstanding, and for an item
// that is not a PR.
func (s *Server) delegatedMidFlight(repoFull string, number int, in delegatedecision.Input) *StageTrackerMidFlight {
	if in.Kind != delegatedecision.KindPR {
		return nil
	}
	it, ok := s.stageTrackerCachedPR(repoFull, number)
	if !ok {
		it, ok = s.stageTrackerBuiltPR(repoFull, number)
	}
	if !ok {
		// The review track of this PR cannot be read: fail closed. An
		// approval must never rest on a head whose review state is unknown
		// (a restart before the first tracker poll, a PR the enumeration
		// has not listed yet).
		return &StageTrackerMidFlight{Kind: midFlightKindReview, Head: strings.TrimSpace(in.HeadSHA), Text: "review state not yet loaded"}
	}
	it.Kind, it.Repo, it.Number = "pr", repoFull, number
	if head := strings.TrimSpace(in.HeadSHA); head != "" {
		it.Head = head
	}
	// The gather's own read of the head's checks outranks the cached CI
	// track: it is newer, and it is the one the policy decides on.
	switch {
	case in.CIPending:
		it.CIActivity = &StageTrackerActivity{Head: it.Head, State: "running"}
	default:
		it.CIActivity = nil
	}
	return stageTrackerMidFlightFor(it)
}

// stageTrackerBuiltPR finds a PR row by building the tracker from the status
// snapshot, for when no tracker body has been served yet.
func (s *Server) stageTrackerBuiltPR(repoFull string, number int) (StageTrackerItem, bool) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return StageTrackerItem{}, false
	}
	for _, it := range s.buildStageTracker(status, s.stageStoreOrShared(), time.Now().UTC()).Items {
		if it.Kind == "pr" && it.Number == number && strings.EqualFold(it.Repo, repoFull) {
			return it, true
		}
	}
	return StageTrackerItem{}, false
}

// delegatedMidFlightOutcome answers a delegated decision on a mid-flight PR:
// no decider runs, nothing is written, the park stays.
func (s *Server) delegatedMidFlightOutcome(repoFull string, number int, itemType, user string, mf *StageTrackerMidFlight) delegatedOutcome {
	head := shortHead(mf.Head)
	d := delegatedecision.Decision{
		Verdict: delegatedecision.VerdictRefused, Rule: delegatedecision.RuleMidFlight, KeepsPark: true,
		Text:      "wait on #" + strconv.Itoa(number) + ": " + mf.Text,
		Rationale: mf.Text + "; a decision now would rest on a head nobody has finished checking",
	}
	res := RowActionResult{
		Action:  rowActionNoEffect,
		Target:  rowActionTarget(repoFull, rowActionTargetPR, number),
		Rule:    delegatedecision.RuleMidFlight,
		Why:     "mid-flight:" + mf.Kind,
		Outcome: "Not decided: " + mf.Text + ".",
		Next:    "Nothing was written. Decide again once CI and review finish" + stageTrackerMidFlightOn(head) + "; the row offers the decision again by itself.",
	}
	s.audit.LogRecord(user, auditRepoItemDecisionDelegated, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType,
		"verdict", string(d.Verdict), "rule", d.Rule, "actions", "", "noop", "no-effect:mid-flight:"+mf.Kind, "policy", delegatedecision.PolicyName, "decision", d.Text), "", repoFull, number)
	return delegatedOutcome{Decision: &d, Notes: []string{"mid-flight (" + mf.Text + "); nothing written"}, Result: &res}
}

// stageTrackerMidFlightOn is " on <sha>" for a known head, "" otherwise.
func stageTrackerMidFlightOn(head string) string {
	if head == "" {
		return ""
	}
	return " on " + head
}
