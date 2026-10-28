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
	rowActionNoop        = "noop-already-applied"
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
		ruleTag = " (" + strings.ReplaceAll(d.Rule, "-", " ") + ")"
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
	switch {
	case d.Verdict == delegatedecision.VerdictRefused:
		out.Action = rowActionRefused
		out.Outcome = "Refused: " + strings.TrimSuffix(d.Rationale, ".") + "."
		out.Next = "Stays at " + parkedAt + " — release the hold or decide it yourself."
	case d.Verdict == delegatedecision.VerdictAdoptSupersede:
		out.Action = rowActionAdopted
		out.Outcome = "Adopted in supersede mode" + ruleTag + "."
		out.Next = "The adoption engine takes it on its next pass: a hive PR supersedes this one and rebases and merges unattended."
	case d.Verdict == delegatedecision.VerdictCloseDuplicate || d.Verdict == delegatedecision.VerdictCloseSuperseded:
		out.Action = rowActionClosed
		if actionTarget > 0 {
			out.Outcome = fmt.Sprintf("Closed in favour of #%d%s.", actionTarget, ruleTag)
		} else {
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
			out.Next = "`/lgtm` + `/approve` posted; the merge gate takes it from here."
		default:
			out.Next = "The merge gate takes it from here; the dashboard merges nothing."
		}
	}
	return out
}
