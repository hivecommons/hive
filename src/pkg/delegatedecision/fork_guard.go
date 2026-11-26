package delegatedecision

import (
	"slices"
	"strings"
)

// Fork guard (hivecommons/hive#14273): a red check that refuses a PR only
// because its head is in a fork ("a fork pull request cannot publish …; a
// maintainer must …"). Neither the author nor an in-place adoption can turn
// it green, since both push to the same fork branch, so the check is a
// maintainer's step. The stage engine parks such a PR at 085 with reason
// `fork-guard:<checks>` and never nudges or adopts it; the delegate keeps the
// check out of every fix list for the same reason. With nothing else owed it
// recommends the maintainer's step and keeps the park; with other items owed
// they go to their owner as usual and the direction names the guarded check
// as the maintainer's step after them.

// ReasonForkGuardPrefix opens the stage engine's fork-guard park reason, the
// checks following comma-separated (stagelabels.LaneReasonForkGuardPrefix).
const ReasonForkGuardPrefix = "fork-guard:"

// RuleForkGuard: every red check on the head is fork-guarded and nothing
// else is owed, so the one step left is a maintainer's. The hive recommends
// it and keeps the park; it never hands the PR to a fixer or the author.
const RuleForkGuard = "fork-guard-maintainer"

// forkGuardFromReasons is the checks a `fork-guard:` park reason names.
func forkGuardFromReasons(reasons []string) []string {
	for _, r := range reasons {
		list, ok := strings.CutPrefix(strings.TrimSpace(r), ReasonForkGuardPrefix)
		if !ok {
			continue
		}
		var out []string
		for _, name := range strings.Split(list, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// forkGuard splits a red head's checks: guarded are the red checks that are
// fork-guarded (ForkGuardChecks, or a `fork-guard:` park reason), others the
// red checks that are not. The park reasons are not tied to a head, so only
// a check RedChecks names as red on this head counts: with no red check
// named there is no guard. Both are nil when CI is not red or no red check
// is guarded.
func (in Input) forkGuard() (guarded, others []string) {
	if !in.CIRed() || len(in.RedChecks) == 0 {
		return nil, nil
	}
	guard := append(append([]string(nil), in.ForkGuardChecks...), forkGuardFromReasons(in.Reasons)...)
	if len(guard) == 0 {
		return nil, nil
	}
	for _, name := range in.RedChecks {
		if slices.Contains(guard, name) {
			guarded = append(guarded, name)
		} else {
			others = append(others, name)
		}
	}
	if len(guarded) == 0 {
		return nil, nil
	}
	return guarded, others
}

// ForkGuarded is the head's red checks that only a maintainer can turn
// green; nil when there are none.
func (in Input) ForkGuarded() []string {
	guarded, _ := in.forkGuard()
	return guarded
}

// CIRedOwed reports a red head with a check the author or a fixer can turn
// green: CIRed, unless every red check is fork-guarded.
func (in Input) CIRedOwed() bool {
	if !in.CIRed() {
		return false
	}
	guarded, others := in.forkGuard()
	return len(guarded) == 0 || len(others) > 0
}

// checkNames is "`a`" or "`a`, `b`".
func checkNames(checks []string) string {
	return "`" + strings.Join(checks, "`, `") + "`"
}

// forkGuardCheckWords names the guarded checks with their own message:
// "the fork-guarded check `a` as its own message asks".
func forkGuardCheckWords(checks []string) string {
	if len(checks) > 1 {
		return "the fork-guarded checks " + checkNames(checks) + " as their own messages ask"
	}
	return "the fork-guarded check " + checkNames(checks) + " as its own message asks"
}

// thenMerges ends a direction: "then it merges", or, when a fork-guarded
// check is red too, the maintainer's step the PR still waits on after the
// listed items.
func (in Input) thenMerges() string {
	guarded := in.ForkGuarded()
	if len(guarded) == 0 {
		return "then it merges"
	}
	return "then a maintainer acts on " + forkGuardCheckWords(guarded) + ", and it merges"
}

// forkGuardOnly reports a PR whose only blocker left is a fork guard: no
// conflict, sign-off, change request or other red check is owed, and on an
// adopted PR no reviewer finding either.
func forkGuardOnly(in Input) bool {
	return in.Kind == KindPR && len(in.ForkGuarded()) > 0 && len(fixList(in)) == 0 && !(in.AdoptedByHive && in.ReviewerRequiresHuman)
}

// decideForkGuard answers a PR whose only blocker left is a fork guard
// (forkGuardOnly). It leaves to their own rules a PR a merged PR already
// delivered (the already-fixed rung), a person's PR with a reviewer
// question for them (decideReturnToAuthor names both), and a contributor PR
// the hive has not adopted whose unreachable author is covered by a
// person's or the hive's PR or claim (the covered-by-other-pr rung). An
// adopted-in-place PR stays parked with the maintainer's step.
func decideForkGuard(in Input, ref string) (Decision, bool) {
	if !forkGuardOnly(in) {
		return Decision{}, false
	}
	if c := in.SupersededBy; c != nil && c.PR > 0 {
		return Decision{}, false
	}
	if in.PersonAuthored() && strings.TrimSpace(in.Question) != "" {
		return Decision{}, false
	}
	if !in.HiveAuthored && !in.PersonAuthored() && !in.AdoptedByHive && !in.ContributorReachable() {
		if _, covered := coveringCompetitor(in); covered || strings.TrimSpace(in.IssueClaimHolder) != "" {
			return Decision{}, false
		}
	}
	return forkGuardDecision(in, ref), true
}

// forkGuardDecision is the recommendation for a head whose only red checks
// are fork-guarded: a maintainer reviews the change and acts on the check
// as its own message asks, or closes the PR; the park is kept. A supersede
// adoption is not offered: the adoption engine refuses to supersede a fork
// PR (fork-rehome-refused), so it would only park the PR again.
func forkGuardDecision(in Input, ref string) Decision {
	guarded := in.ForkGuarded()
	subject, them := "check", "it"
	if len(guarded) > 1 {
		subject, them = "checks", "them"
	}
	head := "this head"
	if h := shortSHA(in.HeadSHA); h != "" {
		head = "head `" + h + "`"
	}
	rationale := "the only red " + subject + " on " + ref + ", " + checkNames(guarded) + ", refuses the PR because its head is in a fork; neither the author nor the hive's fixer can turn " + them + " green, since both push to the fork's branch, so the one step left is a maintainer's (hivecommons/hive#14273)"
	return recommendOnly(RuleForkGuard, rationale,
		"review the change on "+ref+", then act on "+forkGuardCheckWords(guarded)+" (run or admit "+them+" for "+head+", or push the branch to the base repository), or close "+ref)
}
