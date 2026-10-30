package delegatedecision

import (
	"fmt"
	"strings"
)

// The contributor-PR decision ladder (hivecommons/hive#15289).
//
// On hive's own spoke an operator pressed "Let hive decide" on PR #15219
// (author via a clanker relay, conflicting, author/clanker). The policy
// fell straight through to the contributor-offline rule and adopted it in
// supersede mode — but its issue #15202 had been fixed and merged 2.7h
// earlier by #15240. The right answer was "superseded by a merged PR;
// close with a courteous comment". The ladder below evaluates the cheap,
// decisive facts first and only reaches adopt-supersede as the last
// resort. Each rung that does not apply is recorded in Ladder.RuledOut so
// the operator (toast, API, log) can see why the lower rung was reached.

// Ladder rung slugs. Each is also a Decision.Rule with a glossary entry.
const (
	// RuleAlreadyFixed: the PR's issue is closed and another merged PR
	// closed it (or a merged PR declares it supersedes this one). The PR
	// is closed as superseded; it is never adopted.
	RuleAlreadyFixed = "already-fixed"
	// RuleMergeableAsIs: the head is mergeable and green; the merge queue
	// takes it with no adoption.
	RuleMergeableAsIs = "mergeable-as-is"
	// RuleRebaseThenMerge: conflicts are the only blocker. The unattended
	// rebase-then-merge path (hivecommons/hive#15104) is not merged yet, so
	// the rung is recorded as ruled out and the ladder continues.
	RuleRebaseThenMerge = "rebase-then-merge"
	// RuleReturnToContributor: the author's clanker or hive is connected
	// (or has picked the rework up), so the PR goes back to them with the
	// fix list instead of being adopted.
	RuleReturnToContributor = "return-to-contributor"
	// RuleContributorOffline: the last rung — adopt in supersede mode.
	RuleContributorOffline = "contributor-offline"
)

// ladderRungs is the evaluation order.
var ladderRungs = []string{RuleAlreadyFixed, RuleMergeableAsIs, RuleRebaseThenMerge, RuleReturnToContributor, RuleContributorOffline}

// AuthorTooling says what the PR's author drives: a hive, a clanker (a
// relay), a person pushing by hand, or unknown. It picks the words a
// decision uses for them (AuthorWord).
type AuthorTooling string

// Author tooling values.
const (
	AuthorToolingUnknown AuthorTooling = ""
	AuthorToolingHive    AuthorTooling = "hive"
	AuthorToolingClanker AuthorTooling = "clanker"
	AuthorToolingPerson  AuthorTooling = "person"
)

// MergedCloser names the merged PR that already delivered this PR's work.
type MergedCloser struct {
	// PR is the merged pull request; Issue is the issue it closed (0 when
	// the closer declared `Supersedes #N` rather than closing an issue).
	PR    int    `json:"pr"`
	Issue int    `json:"issue,omitempty"`
	SHA   string `json:"sha,omitempty"`
}

// Ladder is the rung the decision landed on and why the rungs above it did
// not apply. It rides on Decision (json `ladder`) into the decide API.
type Ladder struct {
	Decision string   `json:"decision"`
	Reason   string   `json:"reason"`
	RuledOut []string `json:"ruled_out,omitempty"`
}

// AuthorWord is how a decision refers to the author's tooling: "the
// author's clanker" for a clanker relay, "the author's hive" only when a
// hive is known, otherwise "the author" (hivecommons/hive#15289: #15219's
// author has a clanker, and the decision called it their hive).
func (in Input) AuthorWord() string {
	switch in.AuthorTooling {
	case AuthorToolingClanker:
		return "the author's clanker"
	case AuthorToolingHive:
		return "the author's hive"
	}
	return "the author"
}

// OfflineWording is the contributor-offline rationale in the author's own
// terms: a clanker or hive "hasn't been connected"; a bare author "hasn't
// been reachable".
func (in Input) OfflineWording() string {
	who := in.AuthorWord()
	if who == "the author" {
		return who + " hasn't been reachable since the nudge"
	}
	return who + " hasn't been connected since the nudge"
}

// AuthorToolingFromLabels reads author/<kind> labels (hive beats clanker
// beats human, as in pkg/authorkind). An owner is a person whatever the
// labels say. hiveLabel/clankerLabel/humanLabel are the configured label
// names.
func AuthorToolingFromLabels(labels []string, hiveLabel, clankerLabel, humanLabel string, owner bool) AuthorTooling {
	if owner {
		return AuthorToolingPerson
	}
	out := AuthorToolingUnknown
	for _, l := range labels {
		l = strings.TrimSpace(l)
		switch {
		case strings.EqualFold(l, hiveLabel):
			return AuthorToolingHive
		case strings.EqualFold(l, clankerLabel):
			out = AuthorToolingClanker
		case strings.EqualFold(l, humanLabel) && out == AuthorToolingUnknown:
			out = AuthorToolingPerson
		}
	}
	return out
}

// shortSHA is the seven-character form used in comments and toasts.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// SupersededText is the one-line close note for an already-fixed PR:
// "superseded: #15202 was already fixed by #15240 (25de3ba)".
func SupersededText(c MergedCloser) string {
	ref := "#" + itoa(c.PR)
	if c.SHA != "" {
		ref += " (" + shortSHA(c.SHA) + ")"
	}
	if c.Issue > 0 {
		return fmt.Sprintf("superseded: #%d was already fixed by merged PR %s", c.Issue, ref)
	}
	return "superseded by merged PR " + ref
}

// ladderCandidate: the ladder governs an open PR that is not the hive's
// own (hive PRs have their own direction rules).
func ladderCandidate(in Input) bool {
	return in.Kind == KindPR && !in.HiveAuthored
}

// decideLadder walks the rungs for a contributor PR. ok is false when no
// rung applies (the ordinary fix-then-merge / release rules follow).
func decideLadder(in Input, ref string) (Decision, bool) {
	if !ladderCandidate(in) {
		return Decision{}, false
	}
	var ruledOut []string
	out := func(d Decision, reason string) (Decision, bool) {
		d.Ladder = &Ladder{Decision: d.Rule, Reason: reason, RuledOut: ruledOut}
		return d, true
	}
	skip := func(rung, why string) { ruledOut = append(ruledOut, rung+": "+why) }

	// Rung 1 — already fixed. Cheap and decisive: nothing is left to
	// deliver, so neither adoption nor a nudge makes sense.
	if c := in.SupersededBy; c != nil && c.PR > 0 {
		return out(decideAlreadyFixed(in, ref, *c))
	}
	skip(RuleAlreadyFixed, "no merged PR has closed this PR's issue")

	// A person's PR never reaches the contributor rungs: decideReturnToAuthor
	// and the adoption rail own it (hivecommons/hive#15202).
	if in.PersonAuthored() {
		return Decision{}, false
	}

	// Rung 2 — mergeable as-is: the merge queue takes it; no adoption. An
	// adoption that already stalled on a lane blocker (changes requested,
	// attribution) is not one the gate would take on a green head alone.
	if !in.Conflicting && in.CIGreen && !in.DCOMissing && !in.ReporterTrustHold && !in.AdoptionStalled {
		d := releasePR(RuleMergeableAsIs, "approve "+ref+": the head is mergeable and green, so it goes to the merge queue as it is", "a mergeable, green head needs no adoption; the merge queue takes it (hivecommons/hive#15289)")
		return out(d, "mergeable and green; handed to the merge queue")
	}
	skip(RuleMergeableAsIs, mergeBlockers(in))

	// Rung 3 — rebase-then-merge: conflicts are the only blocker. The
	// unattended rebase path is hivecommons/hive#15104 (still open); until
	// it lands the rung only records itself.
	// TODO(hivecommons/hive#15104): hand a conflicts-only head to the
	// rebase-then-merge hook once it is merged.
	if in.Conflicting && in.CIGreen && !in.DCOMissing {
		skip(RuleRebaseThenMerge, "conflicts are the only blocker, but the unattended rebase path (hivecommons/hive#15104) is not available yet")
	} else {
		skip(RuleRebaseThenMerge, "not a conflicts-only head")
	}

	// The reporter-trust gate is decided by the rules after the ladder; a
	// workflow-path PR is the capability-gap rule's; a reachable author
	// adds their own sign-off (fix-then-merge).
	if in.ReporterTrustHold || in.WorkflowPath || (in.DCOMissing && in.ContributorOnline) {
		return Decision{}, false
	}

	// Rung 4 — return to contributor: their clanker or hive is reachable
	// (presence, or the rework-pickup signal of hivecommons/hive#15280).
	if in.ContributorOnline || in.ReworkPickedUp {
		fix := fixList(in)
		if len(fix) == 0 {
			return Decision{}, false
		}
		who := in.AuthorWord()
		why := who + " is connected"
		if in.ReworkPickedUp {
			why = who + " has picked the rework up"
		}
		text := why + ": " + strings.Join(fix, ", ") + " on " + ref + "; then it merges"
		d := Decision{
			Verdict: VerdictDirection, Rule: RuleReturnToContributor, Text: text,
			Rationale: why + ", so the PR goes back to them with the fix list instead of being adopted (hivecommons/hive#15289)",
			Actions:   []Action{decisionComment(text)},
		}
		return out(d, why+"; returned with the fix list")
	}
	skip(RuleReturnToContributor, in.OfflineWording())

	// Rung 5 — adopt-supersede, the last resort.
	if in.AdoptionGraceElapsed {
		text := "adopt now: the offline grace on " + ref + " has already elapsed; adopt in supersede mode at once, preserving the author's commits and sign-off"
		d := Decision{
			Verdict: VerdictAdoptSupersede, Rule: RuleAdoptNow, Text: text,
			Rationale: in.OfflineWording() + " and the grace window has run out; waiting longer helps no one (hivecommons/hive#15038)",
			Actions:   []Action{decisionComment(text), {Kind: ActionAdoptSupersede}},
		}
		return out(d, in.OfflineWording()+" and the grace has elapsed; adopted now")
	}
	if in.AdoptionStalled || in.Conflicting || !in.CIGreen || in.DCOMissing {
		text := "supersede now: adopt " + ref + " in supersede mode, preserving the author's commits and sign-off; do not wait for the grace deadline"
		d := Decision{
			Verdict: VerdictAdoptSupersede, Rule: RuleContributorOffline, Text: text,
			Rationale: in.OfflineWording() + "; adoption in supersede mode keeps the work moving (hivecommons/hive#13975)",
			Actions:   []Action{decisionComment(text), {Kind: ActionAdoptSupersede}},
		}
		return out(d, in.OfflineWording()+"; adopted in supersede mode")
	}
	return Decision{}, false
}

// decideAlreadyFixed is rung 1: close as superseded by the merged closer,
// with a courteous note naming it. A paused repo or a person's PR gets a
// recommendation instead of a write.
func decideAlreadyFixed(in Input, ref string, c MergedCloser) (Decision, string) {
	text := SupersededText(c)
	closer := "#" + itoa(c.PR)
	if c.SHA != "" {
		closer += " (" + shortSHA(c.SHA) + ")"
	}
	var rationale string
	if c.Issue > 0 {
		rationale = fmt.Sprintf("issue #%d is closed and merged PR %s closed it, so %s has nothing left to deliver; it is closed as superseded, not adopted (hivecommons/hive#15289)", c.Issue, closer, ref)
	} else {
		rationale = fmt.Sprintf("merged PR %s declares that it supersedes %s, so nothing is left to deliver; it is closed as superseded, not adopted (hivecommons/hive#15289)", closer, ref)
	}
	switch {
	case in.RepoPaused:
		return recommendOnly(RuleAlreadyFixed, rationale+"; the repo is paused, so the hive only recommends", "close "+ref+" as "+text), text + " (repo paused: recommend only)"
	case in.PersonAuthored():
		return recommendOnly(RuleAlreadyFixed, rationale+"; a person pushed it themselves, so the hive asks them to close it", "close "+ref+" as "+text), text + " (person-authored: recommend only)"
	}
	note := fmt.Sprintf("Thank you for this PR. %s, so there is nothing left for it to deliver and the hive is closing it as superseded. Your work is credited on the issue.", strings.ToUpper(text[:1])+text[1:])
	return Decision{
		Verdict: VerdictCloseSuperseded, Rule: RuleAlreadyFixed, Text: text,
		Rationale: rationale,
		Actions:   []Action{{Kind: ActionCloseDuplicate, Target: c.PR, Text: note}},
	}, text
}

// mergeBlockers names what keeps a head from the merge queue.
func mergeBlockers(in Input) string {
	var b []string
	if in.Conflicting {
		b = append(b, "conflicting")
	}
	if !in.CIGreen {
		b = append(b, "CI not green")
	}
	if in.DCOMissing {
		b = append(b, "DCO missing")
	}
	if in.ReporterTrustHold {
		b = append(b, "reporter-trust hold")
	}
	if in.AdoptionStalled {
		b = append(b, "adoption stalled on a lane blocker")
	}
	if len(b) == 0 {
		return "not mergeable"
	}
	return strings.Join(b, ", ")
}

// fixList is the author's to-do for a head that is not mergeable.
func fixList(in Input) []string {
	var fix []string
	if in.Conflicting {
		fix = append(fix, "rebase onto the base branch")
	}
	if in.DCOMissing {
		fix = append(fix, "add the DCO sign-off")
	}
	if !in.CIGreen {
		fix = append(fix, "make CI green without widening scope")
	}
	return fix
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// LadderRungs returns the rung slugs in evaluation order (for docs/tests).
func LadderRungs() []string { return append([]string(nil), ladderRungs...) }
