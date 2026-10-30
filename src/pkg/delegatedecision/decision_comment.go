package delegatedecision

import (
	"fmt"
	"strings"
	"time"
)

// Plain-words rendering of a delegated decision for the PR / issue comment
// and the dashboard (hivecommons/hive#14822: the owner opened the PR the
// hive had decided on and could not tell what the hive had done, why it
// had not worked, or what "supersede" meant). Every comment the delegate
// writes carries four lines any operator can act on — Hive decided / Why /
// What happens next / If it doesn't happen — built from the glossary, and
// a follow-up comment states plainly when the expected effect timed out.

// DefaultEffectTimeout is how long a recorded decision is given to take
// effect before it is reported as stuck (decisions.delegate.effect_timeout).
const DefaultEffectTimeout = 30 * time.Minute

// StalledMarkerPrefix opens the HTML comment stamped on the follow-up the
// delegate posts when a decision did not take effect, so it is posted once.
const StalledMarkerPrefix = "<!-- hive:decision-stalled"

// Effect is what a decision is expected to cause, in plain words, and what
// the owner can do when it does not happen.
type Effect struct {
	// What is the expected effect ("this PR is closed and a fresh
	// hive-authored PR with the same commits is opened"); "" when the
	// decision keeps the park and nothing is expected to change.
	What string
	// IfNot names the owner's options when What does not happen.
	IfNot string
	// Waits is true when something is expected to happen (What != "").
	Waits bool
}

// ExpectedEffect says what d should cause on an item of kind.
func ExpectedEffect(d Decision, kind Kind) Effect {
	switch {
	case d.Verdict == VerdictOperatorAction:
		// Nothing the hive does moves this; the dedup answer reads "already
		// decided <when>; waiting on <who>" from this line.
		return Effect{IfNot: "Waiting on the spoke owner: " + strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(d.Text), "."), operatorActionPrefix) + ". The report clears itself once it stops recurring; `/fixed` closes it by hand."}
	case d.Verdict == VerdictRefused, d.Verdict == VerdictOwnerOnly, d.Verdict == VerdictRecommendOnly, d.KeepsPark:
		return Effect{IfNot: "The item stays parked until a person decides it: approve it, give direction, or close it from the Activity Monitor."}
	case d.Verdict == VerdictNotNow:
		return Effect{Waits: true, What: "this issue is closed as not planned; re-propose it with the incident or recurrence it would prevent",
			IfNot: "Close it by hand on GitHub, or approve it yourself if you want it built anyway."}
	case d.Verdict == VerdictAdoptSupersede:
		return Effect{Waits: true,
			What:  "the hive closes this PR and opens a fresh hive-authored PR with the same commits and sign-off, then finishes that one",
			IfNot: "A maintainer can merge this PR by hand, re-run the decision, close it as superseded, or send it back for a new commit."}
	case d.Verdict == VerdictCloseDuplicate, d.Verdict == VerdictCloseSuperseded:
		return Effect{Waits: true, What: "this item is closed", IfNot: "Close it by hand on GitHub."}
	case d.Verdict == VerdictDirection:
		return Effect{Waits: true, What: "the author agent pushes a new commit with that change",
			IfNot: "Re-run the decision, give different direction, or close it."}
	case kind == KindIssue:
		return Effect{Waits: true, What: "the issue leaves Needs decision and an agent picks it up",
			IfNot: "Re-run the decision, or remove the park label by hand."}
	default:
		return Effect{Waits: true, What: "the merge queue merges this PR once its checks are green",
			IfNot: "A maintainer can merge it by hand or send it back for a new commit; approving again does not help."}
	}
}

// ActionPlain is the plain-words name of what d does on a PR ("close this
// PR and open a fresh hive-authored PR with the same commits").
func ActionPlain(d Decision) string {
	return ActionPlainOn(d, KindPR)
}

// ActionPlainOn is ActionPlain for an item of kind: an issue's `approve`
// is "approved to proceed — an agent may claim it", never "approved for
// merge".
func ActionPlainOn(d Decision, kind Kind) string {
	switch d.Verdict {
	case VerdictApprove, VerdictQuarantineFlaky, VerdictAcceptRatchet, VerdictAcceptProposal, VerdictDirection,
		VerdictCloseDuplicate, VerdictCloseSuperseded, VerdictOwnerOnly, VerdictRecommendOnly, VerdictRefused,
		VerdictOperatorAction, VerdictNotNow:
		return VerdictPlainOn(d.Verdict, kind)
	case VerdictAdoptSupersede:
		return PlainOn(GlossarySupersede, kind)
	}
	return humanizeSlug(string(d.Verdict))
}

// CommentOptions tunes the plain-words block of a decision comment.
type CommentOptions struct {
	// Kind of the item (issue or PR); affects "what happens next".
	Kind Kind
	// EffectTimeout is printed as "expected within …"; zero omits it.
	EffectTimeout time.Duration
}

// PlainWords renders the four operator lines for d.
func PlainWords(d Decision, opts CommentOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Hive decided:** %s.", CapitalizeFirst(ActionPlainOn(d, opts.Kind)))
	if why := RulePlain(d.Rule); why != "" {
		fmt.Fprintf(&b, "\n**Why:** %s.", CapitalizeFirst(why))
	}
	eff := ExpectedEffect(d, opts.Kind)
	if eff.Waits {
		fmt.Fprintf(&b, "\n**What happens next:** %s", CapitalizeFirst(eff.What))
		if opts.EffectTimeout > 0 {
			fmt.Fprintf(&b, " — expected within %s", humanDuration(opts.EffectTimeout))
		}
		b.WriteString(".")
		fmt.Fprintf(&b, "\n**If it doesn't happen:** %s", eff.IfNot)
	} else {
		fmt.Fprintf(&b, "\n**What happens next:** Nothing by itself. %s", eff.IfNot)
	}
	return b.String()
}

// CommentBodyWith is CommentBody with the plain-words block tuned by opts.
func CommentBodyWith(d Decision, hiveID, requester string, opts CommentOptions) string {
	var b strings.Builder
	switch {
	case d.Verdict == VerdictRefused:
		fmt.Fprintf(&b, "Hive (owner-delegate) did not decide this item: %s.", d.Rationale)
	case d.Verdict == VerdictNotNow:
		// Not a `/hive decision` line: the unpark sweep must not read a
		// declined proposal as an approval.
		fmt.Fprintf(&b, "%s\n\nRationale: %s. Closed as not planned.", CapitalizeFirst(d.Text), d.Rationale)
	case d.KeepsPark:
		park := LabelNeedsDecision
		if opts.Kind == KindIssue {
			park = LabelIssueNeedsDecision
		}
		fmt.Fprintf(&b, "%s\n\nRationale: %s. `%s` stays.", d.Text, d.Rationale, park)
	default:
		fmt.Fprintf(&b, "/hive decision %s\n\nRationale: %s.", d.Text, d.Rationale)
	}
	fmt.Fprintf(&b, "\n\n%s", PlainWords(d, opts))
	by := "the hive"
	if r := strings.TrimSpace(requester); r != "" {
		by = "the hive at @" + r + "'s request"
	}
	fmt.Fprintf(&b, "\n\n_Decided by %s as the owner's delegate under the %s policy (rule `%s`)._\n%s", by, PolicyName, d.Rule, Marker(hiveID))
	return b.String()
}

// StalledMarker is the audit stamp on the follow-up comment for rule.
func StalledMarker(hiveID, rule string) string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		hiveID = "unknown"
	}
	return fmt.Sprintf("%s by=%s rule=%s -->", StalledMarkerPrefix, hiveID, strings.TrimSpace(rule))
}

// IsStalledComment reports whether body is a stalled follow-up (for rule,
// when rule is non-empty).
func IsStalledComment(body, rule string) bool {
	if !strings.Contains(body, StalledMarkerPrefix) {
		return false
	}
	return rule == "" || strings.Contains(body, " rule="+strings.TrimSpace(rule)+" ")
}

// StalledCommentBody is the follow-up the delegate posts when the decision
// recorded at decidedAt has not caused its expected effect: what was
// decided, when, what should have happened, why it has not (plain words
// from the glossary for the lane reason), and what the owner can do.
func StalledCommentBody(d Decision, kind Kind, decidedAt time.Time, whySlug string, options []string, hiveID string) string {
	eff := ExpectedEffect(d, kind)
	var b strings.Builder
	fmt.Fprintf(&b, "**Hive decided %s at %s, but it hasn't taken effect.**", ActionPlainOn(d, kind), decidedAt.UTC().Truncate(time.Second).Format(time.RFC3339))
	if eff.What != "" {
		fmt.Fprintf(&b, "\n\n**What should have happened:** %s.", CapitalizeFirst(eff.What))
	}
	why := DetailOn(whySlug, kind)
	if strings.TrimSpace(whySlug) == "" {
		why = Detail(GlossaryEffectUnknown)
	}
	fmt.Fprintf(&b, "\n**Why it hasn't:** %s", why)
	if len(options) > 0 {
		fmt.Fprintf(&b, "\n**What you can do:** %s.", strings.Join(options, " · "))
	} else if eff.IfNot != "" {
		fmt.Fprintf(&b, "\n**What you can do:** %s", eff.IfNot)
	}
	fmt.Fprintf(&b, "\n\n_Posted by the hive as the owner's delegate; the rule was: %s (`%s`)._\n%s", RulePlain(d.Rule), d.Rule, StalledMarker(hiveID, d.Rule))
	return b.String()
}

// CapitalizeFirst upper-cases the first byte of a trimmed phrase.
func CapitalizeFirst(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}
