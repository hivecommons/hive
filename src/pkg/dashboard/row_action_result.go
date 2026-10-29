package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// RowActionResult is the one "what actually happened" contract every
// Activity Monitor row action answers with, additively, under `result`: the
// dashboard renders it as a toast that stays up until the target row moves.
// Action is a closed set (rowAction* below); Target is the item acted on,
// which may differ from the clicked row (an issue row decides its nested
// PR); Outcome is one short sentence in the past tense; Next says what
// happens now or what the owner must still do. Rule is the delegate's rule
// when the hive decided; AppliedAt is set on a no-op to say when the same
// decision was first recorded.
type RowActionResult struct {
	Action    string          `json:"action"`
	Target    RowActionTarget `json:"target"`
	Outcome   string          `json:"outcome"`
	Next      string          `json:"next"`
	Rule      string          `json:"rule,omitempty"`
	AppliedAt *time.Time      `json:"applied_at,omitempty"`
	// Why is the glossary slug for why a recorded decision has not taken
	// effect (Action no-effect); Options are the concrete next actions in
	// the popover's shape (hivecommons/hive#14822). Both additive.
	Why     string                       `json:"why,omitempty"`
	Options []StageTrackerDecisionOption `json:"options,omitempty"`
	// Posted lists what the action wrote on GitHub (`/lgtm`, `/approve`,
	// a label removed) for the toast's muted secondary line; Outcome stays
	// the plain-words headline. SentinelCleared says the action took the
	// sentinel alert off, so the toast drops its ⚠ paragraph; SentinelOpen
	// says the PR still wears the alert after the action and a person must
	// clear it. Both additive.
	Posted          []string `json:"posted,omitempty"`
	SentinelCleared bool     `json:"sentinel_cleared,omitempty"`
	SentinelOpen    bool     `json:"sentinel_open,omitempty"`
}

// RowActionTarget names the issue or PR a row action acted on.
type RowActionTarget struct {
	Repo   string `json:"repo"`
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
}

// The closed set of RowActionResult.Action values.
const (
	rowActionAdopted     = "adopted"
	rowActionApproved    = "approved"
	rowActionRecommended = "recommended"
	rowActionClosed      = "closed"
	rowActionDirected    = "directed"
	rowActionRefused     = "refused"
	rowActionHeld        = "held"
	rowActionReleased    = "released"
	rowActionRescanned   = "rescanned"
	rowActionTriaged     = "triage-accepted"
	rowActionNeedsHuman  = "needs-human"
	rowActionMerged      = "merged"
	rowActionQueued      = "queued-for-merge"
	rowActionNoop        = "noop-already-applied"
	// rowActionDecidedEach is an issue job that decided each nested PR on
	// its own; the per-PR results ride delegatedDecisionJob.Items.
	rowActionDecidedEach = "decided-each"
	rowActionNoEffect    = "no-effect"
	rowActionTargetIssue = "issue"
	rowActionTargetPR    = "pr"
)

func rowActionTarget(repo, kind string, number int) RowActionTarget {
	if kind != rowActionTargetPR {
		kind = rowActionTargetIssue
	}
	return RowActionTarget{Repo: repo, Kind: kind, Number: number, URL: githubItemURL(repo, kind, strconv.Itoa(number))}
}

// rowActionNoopResult is the plain "nothing new happened" answer: when the
// same decision was first applied and what still blocks the item.
func rowActionNoopResult(target RowActionTarget, what string, appliedAt time.Time, blockedBy string) RowActionResult {
	out := RowActionResult{Action: rowActionNoop, Target: target, Outcome: "No change — " + what}
	if !appliedAt.IsZero() {
		at := appliedAt.UTC().Truncate(time.Second)
		out.AppliedAt = &at
		out.Outcome += " at " + at.Format(time.RFC3339)
	}
	if b := strings.TrimSpace(blockedBy); b != "" {
		out.Next = "Still blocked by " + b + "."
	} else {
		out.Next = "Nothing further to do."
	}
	return out
}

// delegatedRowActionResult reads the delegate's decision and the execution
// notes into the shared result shape.
func delegatedRowActionResult(in delegatedecision.Input, d delegatedecision.Decision, notes []string) RowActionResult {
	kind := rowActionTargetIssue
	if in.Kind == delegatedecision.KindPR {
		kind = rowActionTargetPR
	}
	out := RowActionResult{Target: rowActionTarget(in.Repo, kind, in.Number), Rule: d.Rule}
	ruleTag := ""
	if d.Rule != "" {
		ruleTag = " (" + delegatedecision.RulePlain(d.Rule) + ")"
	}
	actionTarget := 0
	for _, a := range d.Actions {
		if a.Target > 0 {
			actionTarget = a.Target
		}
	}
	note := func(sub string) bool {
		for _, n := range notes {
			if strings.Contains(n, sub) {
				return true
			}
		}
		return false
	}
	parkedAt := "085"
	if in.Kind == delegatedecision.KindIssue {
		parkedAt = "045"
	}
	// The ladder's own outcome line names the facts (hivecommons/hive#15289).
	ladderOutcome, hasLadderOutcome := delegatedLadderOutcome(in, d)
	switch {
	case d.Verdict == delegatedecision.VerdictRefused:
		out.Action = rowActionRefused
		out.Outcome = "Refused: " + strings.TrimSuffix(d.Rationale, ".") + "."
		out.Next = "Stays at " + parkedAt + " — release the hold or decide it yourself."
	case d.Verdict == delegatedecision.VerdictAdoptSupersede:
		out.Action = rowActionAdopted
		// Outcome first, in one line; the head already names the rule, so
		// it is not repeated here (hivecommons/hive#15126).
		ref := "#" + strconv.Itoa(in.Number)
		if actionTarget > 0 {
			out.Outcome = fmt.Sprintf("Adopted: hive opened #%d with the same commits and will merge it; %s will be closed as superseded.", actionTarget, ref)
			out.Next = "The hive's copy rebases, soaks and merges unattended; " + ref + " closes when it lands."
		} else {
			out.Outcome = "Adopted — hive will re-author " + ref + " as a hive PR with the same commits and merge it next pass; " + ref + " will be closed as superseded."
			out.Next = "Until the copy opens the row reads Adopted at the merge gate; the copy then carries the chevron and " + ref + " is listed under it as superseded."
		}
		if hasLadderOutcome {
			out.Outcome += " " + ladderOutcome
		}
	case d.Verdict == delegatedecision.VerdictNotNow:
		out.Action = rowActionClosed
		out.Outcome = "Declined for now" + ruleTag + ": " + strings.TrimSuffix(strings.TrimSpace(d.Text), ".") + "."
		out.Next = "Closed as not planned; re-propose it with the incident or recurrence it would prevent."
	case d.Verdict == delegatedecision.VerdictOperatorAction:
		out.Action = rowActionRecommended
		out.Outcome = "Operator action" + ruleTag + ": " + strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(d.Text), "."), "operator action — ") + "."
		out.Next = "Stays at " + parkedAt + " — waiting on the spoke owner; no agent is sent. Closes itself once the report stops recurring."
	case d.Verdict == delegatedecision.VerdictCloseDuplicate || d.Verdict == delegatedecision.VerdictCloseSuperseded:
		out.Action = rowActionClosed
		switch {
		case hasLadderOutcome:
			out.Outcome = ladderOutcome
		case actionTarget > 0:
			out.Outcome = fmt.Sprintf("Closed in favour of #%d%s.", actionTarget, ruleTag)
		default:
			out.Outcome = "Closed" + ruleTag + "."
		}
		out.Next = "Nothing further to do; the row leaves the tracker on its next refresh."
	case d.Verdict == delegatedecision.VerdictOwnerOnly:
		out.Action = rowActionRecommended
		out.Outcome = "Owner-only" + ruleTag + ": `" + delegatedecision.LabelNeedsOwner + "` and `" + delegatedecision.LabelHold + "` applied."
		out.Next = "Stays at " + parkedAt + " — only the owner can grant this; release the hold once done."
	case d.Verdict == delegatedecision.VerdictRecommendOnly || d.KeepsPark:
		out.Action = rowActionRecommended
		out.Outcome = "Recommendation only" + ruleTag + ": " + strings.TrimSuffix(strings.TrimSpace(d.Text), ".") + "."
		out.Next = "Stays at " + parkedAt + " — owner to confirm."
	case d.Verdict == delegatedecision.VerdictDirection:
		out.Action = rowActionDirected
		out.Outcome = "Direction recorded" + ruleTag + ": " + strings.TrimSuffix(strings.TrimSpace(d.Text), ".") + "."
		if in.Kind == delegatedecision.KindPR {
			out.Next = "The author agent pushes a new head; the row moves when it lands."
		} else {
			out.Next = "The hive carries on with that direction; nothing is merged."
		}
	default:
		out.Action = rowActionApproved
		out.Outcome = "`/hive decision` recorded" + ruleTag + ": " + strings.TrimSuffix(strings.TrimSpace(d.Text), ".") + "."
		switch {
		case in.Kind == delegatedecision.KindIssue:
			out.Next = "Un-parked; the hive carries on. Nothing is merged."
		case note("deferred"):
			out.Next = "Approval follows once a person clears the sentinel label; the hive never clears it."
		case note("queued for auto-merge"):
			out.Next = "Queued: the auto-merge engine merges per repository settings."
		case note("/lgtm"):
			out.Next = "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked."
			out.Posted = append(out.Posted, "`/lgtm` posted", "`/approve` posted")
		default:
			out.Next = "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked."
		}
	}
	return out
}
