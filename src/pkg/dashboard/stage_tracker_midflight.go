package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Mid-flight PRs (hivecommons/hive#18889).
//
// A decision on a PR (Decide…, Approve as-is, Let hive decide) is a call on
// the head as it stands. While the CURRENT head's CI is still running, or
// its review has not finished, the call is premature: the verdict the person
// would be deciding against does not exist yet, and an approval recorded now
// is an approval of a head nobody has looked at. The row therefore carries
// `mid_flight`, the one fact the SPA's guards and the server's delegated
// decision both read: what the current head still waits on. A park recorded
// for an EARLIER head does not count either — a push restarts both tracks,
// so a CI or review activity measured on another head says the current one
// has not been evaluated yet.

// Mid-flight kinds.
const (
	midFlightKindCI     = "ci-running"
	midFlightKindReview = "review-in-progress"
	midFlightKindHead   = "head-moved"
)

// StageTrackerMidFlight is what the PR's current head still waits on before
// a decision is meaningful. nil on a PR whose CI and review are settled.
type StageTrackerMidFlight struct {
	// Kind is one of the midFlightKind* constants (the first that applies:
	// a moved head, then CI, then review).
	Kind string `json:"kind"`
	// Text is the reason the row prints on the disabled verbs: "CI running
	// 2/5", "review queued on abc1234 (0/5)".
	Text string `json:"text"`
	// Head is the current head the wait is about.
	Head string `json:"head,omitempty"`
	// Park is, when a person was asked for a decision on this PR, the plain
	// reason the hive asked, so the row says why it is parked at all
	// instead of a bare "decision".
	Park string `json:"park,omitempty"`
}

// stageTrackerReviewInFlightStates are the review activity states of a head
// whose review has been requested and has not produced its verdict:
// queued (unassigned / assigned) or being worked (reviewing / adjudicating).
var stageTrackerReviewInFlightStates = map[string]string{
	"unassigned":   "queued",
	"assigned":     "queued",
	"reviewing":    "in progress",
	"adjudicating": "in progress",
}

// stageTrackerMidFlightFor derives the mid-flight state of a PR row from its
// CI and review activity on the CURRENT head. Rows that are not open PRs,
// and PRs whose CI and review are settled, answer nil. Checks that have run
// far longer than a normal run (stuck) do not make a PR mid-flight: they
// will not finish on their own, and the person may have to act.
func stageTrackerMidFlightFor(it StageTrackerItem) *StageTrackerMidFlight {
	if it.Kind != "pr" || stageTrackerHumanGateInert(it) {
		return nil
	}
	head := shortHead(it.Head)
	out := &StageTrackerMidFlight{Head: strings.TrimSpace(it.Head)}
	ci, rv := it.CIActivity, it.ReviewActivity
	var waits []string
	var moved []string
	if ci != nil && !stageTrackerHumanGateOnHead(ci, it.Head) {
		moved = append(moved, "CI")
	}
	if rv != nil && !stageTrackerHumanGateOnHead(rv, it.Head) {
		moved = append(moved, "review")
	}
	if len(moved) > 0 {
		// The activity on file is for an earlier head: a push restarted
		// the tracks, and whatever parked the PR parked the old head.
		out.Kind = midFlightKindHead
		on := ""
		if head != "" {
			on = " on " + head
		}
		waits = append(waits, strings.Join(moved, " and ")+" not yet run"+on)
	}
	if ci != nil && stageTrackerHumanGateOnHead(ci, it.Head) && stageTrackerCIMidFlight(ci) {
		tally := ""
		if ci.ChecksTotal > 0 {
			tally = fmt.Sprintf(" %d/%d", ci.ChecksPassed, ci.ChecksTotal)
		}
		if out.Kind == "" {
			out.Kind = midFlightKindCI
		}
		waits = append(waits, "CI running"+tally)
	}
	if rv != nil && stageTrackerHumanGateOnHead(rv, it.Head) {
		if word, ok := stageTrackerReviewInFlightStates[rv.State]; ok {
			tally := ""
			if rv.PerspectivesTotal > 0 {
				tally = fmt.Sprintf(" (%d/%d)", rv.PerspectivesDone, rv.PerspectivesTotal)
			}
			on := ""
			if head != "" {
				on = " on " + head
			}
			if out.Kind == "" {
				out.Kind = midFlightKindReview
			}
			waits = append(waits, "review "+word+on+tally)
		}
	}
	if len(waits) == 0 {
		return nil
	}
	out.Text = strings.Join(waits, " · ")
	if stageTrackerHumanGatePRParked(it.Stage, it.Modifiers, it.Superseded != nil, it.ManualMerge, it.HumanMergeReason) {
		out.Park = stageTrackerMidFlightPark(it)
	}
	return out
}

// stageTrackerCIMidFlight reports a CI activity that still has checks to
// report on its head: running, queued, or required checks not yet reported
// — unless every pending check is stuck.
func stageTrackerCIMidFlight(ci *StageTrackerActivity) bool {
	if ci == nil {
		return false
	}
	if len(ci.ChecksStuck) > 0 && len(ci.ChecksPending) <= len(ci.ChecksStuck) {
		return false
	}
	switch ci.State {
	case "running", "queued":
		return true
	case "missing":
		return len(ci.ChecksPending) > 0
	}
	return false
}

// stageTrackerMidFlightPark is the plain-words reason the hive parked the PR
// for a person: the lane reason's own words, "" when the row names none.
func stageTrackerMidFlightPark(it StageTrackerItem) string {
	base := strings.TrimSpace(it.Reason)
	if base == "" {
		return ""
	}
	if plain := decisionPlainReason(base); plain != "" && plain != decisionPlainReasonFallback {
		return plain
	}
	return ""
}

// stageTrackerResolveMidFlight sets every PR row's mid_flight and each
// nested PR's, from the same tracks (a nested PR carries its own CI and
// review activity). It runs before the human gates so the gate and the
// verbs read the finished field.
func stageTrackerResolveMidFlight(items []StageTrackerItem) {
	// A nested PR carries no lane reason of its own; when the PR is listed
	// as a row, its park reason is the row's.
	parks := map[string]string{}
	for i := range items {
		it := &items[i]
		it.MidFlight = stageTrackerMidFlightFor(*it)
		stageTrackerMidFlightGuardOptions(it.DecisionNeeded, it.MidFlight)
		if it.Kind == "pr" && it.MidFlight != nil && it.MidFlight.Park != "" {
			parks[strings.ToLower(it.Repo)+"#"+strconv.Itoa(it.Number)] = it.MidFlight.Park
		}
	}
	for i := range items {
		it := &items[i]
		for j := range it.LinkedPRs {
			lp := &it.LinkedPRs[j]
			lp.MidFlight = nil
			if !stageTrackerHumanGateOpenLinkedPR(*lp) {
				continue
			}
			lp.MidFlight = stageTrackerMidFlightFor(StageTrackerItem{
				Repo: it.Repo, Number: lp.Number, Kind: "pr", Stage: lp.Stage, Head: lp.Head,
				Modifiers: lp.Modifiers, ManualMerge: lp.ManualMerge, HumanMergeReason: lp.HumanMergeReason,
				CIActivity: lp.CIActivity, ReviewActivity: lp.ReviewActivity,
			})
			if lp.MidFlight != nil && lp.MidFlight.Park == "" {
				lp.MidFlight.Park = parks[strings.ToLower(it.Repo)+"#"+strconv.Itoa(lp.Number)]
			}
			stageTrackerMidFlightGuardOptions(lp.DecisionNeeded, lp.MidFlight)
		}
	}
}

// stageTrackerMidFlightVerbs are the decision options that rest on the head
// being checked: the delegate, the approval and the answer to the park's
// question. Closing, holding and the rest stay as they were — a person may
// close a PR whose CI is still running.
var stageTrackerMidFlightVerbs = []string{decisionActionDelegate, decisionActionApprove, decisionActionDirection}

// stageTrackerMidFlightWhy is the reason printed on a disabled verb.
func stageTrackerMidFlightWhy(mf *StageTrackerMidFlight) string {
	why := "Waiting on " + mf.Text + ": the decision opens when CI and review finish on the current head"
	if mf.Park != "" {
		why += " (parked because " + mf.Park + ")"
	}
	return why
}

// stageTrackerMidFlightGuardOptions disables, in place, the options of a
// decision that rest on the head being checked while the PR is mid-flight,
// with the reason, so every surface that draws the server's options (the
// popover, the lead actions, the pill) shows them disabled instead of
// offering a decision on a head nobody has finished checking.
func stageTrackerMidFlightGuardOptions(d *StageTrackerDecision, mf *StageTrackerMidFlight) {
	if d == nil || mf == nil {
		return
	}
	for i := range d.Options {
		if o := &d.Options[i]; slices.Contains(stageTrackerMidFlightVerbs, o.Action) {
			o.Enabled, o.ReasonIfDisabled = false, stageTrackerMidFlightWhy(mf)
		}
	}
}
