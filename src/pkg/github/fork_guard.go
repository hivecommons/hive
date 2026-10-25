package github

import (
	"strings"
)

// Fork guard: a failing check that refuses the PR only because its head is
// in a fork ("a fork pull request cannot publish the canary image; a
// maintainer must push this branch to the repository"). Neither the
// contributor nor an in-place adoption can turn it green — both push to the
// same fork branch — so the stage engine parks such a PR for a maintainer
// (stage/085, reason `fork-guard:<checks>`) instead of returning it to its
// author (hivecommons/hive#14273).
//
// The match is deliberately narrow: the PR must be from a fork, and EVERY
// failing check must carry at least one annotation line in CIFailureExcerpt
// matching forkGuardMessage.

// forkGuardPhrases are the lower-cased annotation fragments that mark a
// fork-publication / fork-secrets guard on their own.
var forkGuardPhrases = []string{
	"fork pull request cannot",
	"fork pull requests cannot",
	"maintainer must push this branch",
}

// forkGuardMessage reports whether one annotation message says the check
// refuses fork PRs.
func forkGuardMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, p := range forkGuardPhrases {
		if strings.Contains(m, p) {
			return true
		}
	}
	return strings.Contains(m, "cannot publish") && strings.Contains(m, "fork")
}

// ForkGuardChecksFor applies the rule to one PR: the failing checks, in
// order, when the PR is from a fork and every failing check's excerpt lines
// ("<check>: <message>", see fetchFailureExcerpt) include a fork-guard
// message; nil otherwise.
func ForkGuardChecksFor(fromFork bool, failing []string, excerpt string) []string {
	if !fromFork || len(failing) == 0 || strings.TrimSpace(excerpt) == "" {
		return nil
	}
	lines := strings.Split(excerpt, "\n")
	out := make([]string, 0, len(failing))
	for _, name := range failing {
		matched := false
		for _, line := range lines {
			if msg, ok := strings.CutPrefix(line, name+": "); ok && forkGuardMessage(msg) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		out = append(out, name)
	}
	return out
}

// ForkGuardChecks is ForkGuardChecksFor over the PR's own evidence.
func (p PullRequest) ForkGuardChecks() []string {
	return ForkGuardChecksFor(p.FromFork, p.FailingChecks, p.CIFailureExcerpt)
}
