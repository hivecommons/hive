package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// decisionForHumanMerge: a PR parked at 090 Ready for human merge — the
// hive's lanes will not merge it unattended (untrusted author, workflow
// files, manual-merge path, branch protection, …), so the person is asked
// to merge. The lead option is "Approve and let hive merge"
// (hivecommons/hive#15078): the maintainer has the last word, and the
// default way to use it is to approve and let the hive perform the merge
// through its own App — the merge sweep picks it up on its next tick once
// CI is green, soak and the self-merge ban still apply, and the merge is
// attributed to the approver. "Merge now as maintainer" merges at once
// through the App and stays available, but second. Both need the merger
// or owner role; the SPA greys them out for anyone else.
func decisionForHumanMerge(it StageTrackerItem, held bool) *StageTrackerDecision {
	base, approver := stagelabels.HumanApprovedReason(it.Reason)
	base = strings.TrimSpace(base)
	_, _, tip := stageTrackerHumanMergeReason(it.Reason)
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: base}
	ref := "#" + strconv.Itoa(it.Number)
	// First read: one sentence of state, one of why (hivecommons/hive#15394).
	why := stageTrackerUntrustedAuthorWhy(base, it.Author)
	fix := stageTrackerUntrustedAuthorFix(base, it.Author)
	switch {
	case why != "":
		why = "Hive won't merge it alone — " + why + "."
	case tip != "":
		why = sentence(tip)
	default:
		why = "Hive's merge rules leave this one to a person."
	}
	d.Question = "Ready to merge. " + why
	details := []string{"Hive never merges this on its own; a maintainer merges here or on GitHub."}
	if fix != "" {
		details = append(details, fix)
	}
	if approver != "" {
		details = append(details, "Approved by @"+approver+".")
	}
	admitter := stagelabels.AdmissionLogin(it.AdmittedBy)
	breaking := strings.EqualFold(base, stageengineprstage.AutoMergeReasonHumanMergeRequired)
	if breaking {
		// No approval lets hive merge a breaking change (hivecommons/hive#15442).
		admitter = ""
	}
	if admitter != "" {
		// A person already took the trust call for this head (#15136):
		// the lane merges it as-is; nothing is adopted or re-authored.
		d.Question = "@" + admitter + " approved it. Hive merges it on its next pass."
		details = []string{"Admitted by @" + admitter + " (" + it.AdmittedBy + "). Hive merges this exact version once CI is green; a new push asks again."}
	}
	if dco := decisionDCOBlocker(it); dco != "" {
		details = append(details, dco)
	}
	if held {
		details = append(details, decisionHeldBlocker(it.Hold))
	}
	d.Detail = stageTrackerSayOnce(d.Question, details...)
	mergeNow := decisionMergeNowOption(it, held)
	queue := decisionOption(decisionActionQueueMerge, decisionQueueMergeLabel, !held, "Release the hold first")
	queue.Consequence = "Hive merges " + ref + " on its next tick once CI is green"
	if admitter != "" && !held {
		queue.Enabled, queue.ReasonIfDisabled = false, "@"+admitter+" already approved it; hive merges it next pass"
	}
	if breaking {
		// The lanes skip a breaking-titled / needs-owner-merge PR however
		// it is queued (hivecommons/hive#15442): the merge-now click is the
		// one path, so queueing is offered greyed with the reason.
		queue.Enabled, queue.ReasonIfDisabled = false, "Hive never merges a breaking change; merge it now as maintainer"
	}
	if it.DCOUnsigned > 0 {
		mergeNow.Consequence += "; " + decisionDCOConsequence(it)
		queue.Consequence += "; " + decisionDCOConsequence(it)
	}
	direction := decisionOption(decisionActionDirection, "Give direction", it.AuthorKind != authorKindHuman, "Hive did not open this PR, so it cannot carry a direction to its author; leave a review on GitHub")
	direction.Consequence = "Asks the author's agent for changes instead"
	closeSup := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	closeSup.Consequence = "Closes " + ref + ", naming the PR that replaces it"
	closeWontfix := decisionOption(decisionActionCloseWontfix, "Close as won't fix", true, "")
	closeWontfix.Consequence = "Closes " + ref + " without merging"
	delegate := decisionDelegateOption(held, "Hive explains and recommends; merges nothing")
	d.Options = decisionHumanMergeLeadOptions(mergeNow, queue)
	if it.DCOUnsigned > 0 {
		d.Options = append(d.Options, decisionReauthorOption(it, ref, held))
	}
	d.Options = append(d.Options, delegate, direction, closeSup, closeWontfix, decisionHoldOption(held))
	return d
}

// decisionQueueMergeLabel is the lead merge-gate answer on a 090 row: the
// maintainer approves and the hive's merge sweep merges on its next tick.
const decisionQueueMergeLabel = "Approve and let hive merge"

// decisionMergeNowLabel is the secondary answer: merge at once through the
// hive's App, attributed to the maintainer.
const decisionMergeNowLabel = "Merge now as maintainer"

// decisionHumanMergeLeadOptions orders the two merge-gate answers: "Approve
// and let hive merge" leads whenever it can be clicked; only when queueing
// is greyed out (a breaking change the lanes never merge, a head somebody
// already admitted) does "Merge now as maintainer" step forward, so the
// lead is never a button nobody can press.
func decisionHumanMergeLeadOptions(mergeNow, queue StageTrackerDecisionOption) []StageTrackerDecisionOption {
	if !queue.Enabled && mergeNow.Enabled {
		return []StageTrackerDecisionOption{mergeNow, queue}
	}
	return []StageTrackerDecisionOption{queue, mergeNow}
}

// stageTrackerSayOnce joins parts into one text, dropping any sentence
// that already appeared in lead or earlier in the parts, so a hover never
// says the same thing twice (hivecommons/hive#15394).
func stageTrackerSayOnce(lead string, parts ...string) string {
	seen := map[string]bool{}
	for _, sent := range stageTrackerSentences(lead) {
		seen[sent] = true
	}
	var out []string
	for _, part := range parts {
		var keep []string
		for _, sent := range stageTrackerSentences(part) {
			if seen[sent] {
				continue
			}
			seen[sent] = true
			keep = append(keep, sentence(sent))
		}
		if len(keep) > 0 {
			out = append(out, strings.Join(keep, " "))
		}
	}
	return strings.Join(out, " ")
}

// stageTrackerSentences splits text on sentence ends and normalises each
// sentence (trimmed, trailing punctuation dropped, lower-cased first rune)
// for say-it-once comparison.
func stageTrackerSentences(text string) []string {
	var out []string
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool { return r == '.' || r == '?' || r == '!' }) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		r := []rune(raw)
		r[0] = unicode.ToLower(r[0])
		out = append(out, string(r))
	}
	return out
}

// sentence capitalises a clause and ends it with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

// decisionDCOBlocker says, before anyone presses Approve, that the head
// cannot merge as-is where DCO is required: the stage record's commit check
// found commits without a Signed-off-by (hivecommons/hive#15136). "" when
// the check is not cached for this head or every commit is signed.
func decisionDCOBlocker(it StageTrackerItem) string {
	if it.DCOUnsigned <= 0 {
		return ""
	}
	return fmt.Sprintf("%d of %s no Signed-off-by, so a required DCO check refuses the merge. Re-author as hive PR is the separate choice for that; an Approve never re-authors by itself.",
		it.DCOUnsigned, pluralCommitsHave(it.DCOTotal))
}

// decisionDCOConsequence is the short DCO caveat under the merge options.
func decisionDCOConsequence(it StageTrackerItem) string {
	return fmt.Sprintf("%d of %s no Signed-off-by (DCO)", it.DCOUnsigned, pluralCommitsHave(it.DCOTotal))
}

// pluralCommitsHave renders "3 commits have" / "1 commit has" for the DCO
// sentences; a total the record does not know reads "its commits have".
func pluralCommitsHave(total int) string {
	switch {
	case total == 1:
		return "1 commit has"
	case total > 1:
		return strconv.Itoa(total) + " commits have"
	}
	return "its commits have"
}

// decisionReauthorOption is the operator's explicit "re-author as hive PR"
// choice (hivecommons/hive#15136): the hive re-homes the commits on a
// hive-owned, signed-off branch and opens a superseding PR. A person's PR
// is never re-authored by the hive, so for a human author the option is
// shown disabled with the step that does unblock it.
func decisionReauthorOption(it StageTrackerItem, ref string, held bool) StageTrackerDecisionOption {
	o := decisionOption(decisionActionReauthor, "Re-author as hive PR", !held && it.AuthorKind != authorKindHuman, "Release the hold first")
	if it.AuthorKind == authorKindHuman {
		who := "the author"
		if it.Author != "" {
			who = "@" + it.Author
		}
		o.ReasonIfDisabled = "Hive does not re-author a person's PR; ask " + who + " to sign off and push"
	}
	o.Consequence = "Hive reopens " + ref + " as a signed-off hive PR; nothing is merged as-is"
	return o
}

// decisionMergeNowOption is "Merge now as maintainer", or — when the row
// wears needs-rebase, so a plain merge would be a GitHub 405 — "Rebase,
// then merge as maintainer": the same click records the approval, has the
// hive rebase the PR, and merges it on green (hivecommons/hive#14822).
// Either way it is the secondary answer behind "Approve and let hive merge".
func decisionMergeNowOption(it StageTrackerItem, held bool) StageTrackerDecisionOption {
	ref := "#" + strconv.Itoa(it.Number)
	into := strings.TrimSpace(it.BaseRef)
	if into == "" {
		into = "its base branch"
	}
	if stageTrackerNeedsRebase(it) {
		opt := decisionOption(decisionActionMergeNow, rebaseMergeFollowThroughLabel, !held, "Release the hold first")
		opt.Consequence = rebaseMergeFollowThroughConsequence(it.Number, strings.TrimSpace(it.BaseRef))
		return opt
	}
	opt := decisionOption(decisionActionMergeNow, decisionMergeNowLabel, !held, "Release the hold first")
	opt.Consequence = "Merges " + ref + " into " + into + " now, credited to you"
	return opt
}

// stageTrackerNeedsRebase: the row wears needs-rebase (modifier or label),
// or a rebase-then-merge follow-through is already running on it.
func stageTrackerNeedsRebase(it StageTrackerItem) bool {
	if slices.Contains(it.Modifiers, modNeedsRebase) {
		return true
	}
	for _, l := range it.Labels {
		if strings.EqualFold(strings.TrimSpace(l), "needs-rebase") {
			return true
		}
	}
	return it.RebaseMerge != nil && it.RebaseMerge.State != "" && it.RebaseMerge.State != "merged" && it.RebaseMerge.State != "failed"
}
