package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// Owner rule (hivecommons/hive, 2026-10-29), verbatim: "there should be no
// decisions by a human while these are in ci/review — unless the review
// found something", refined to "unless you find something fishy and
// insecure happening at the pr in progress level, then we can let things
// go and then decide in merge queue, if needed", and "i always have the
// last word when it comes to merge of untrusted".
//
// So for a PR the hive did not author, the ONLY thing that may take it out
// of hands-free CI/Review and park it for a person is a concrete
// security / trust finding — the predicate below. Everything else the
// review raises (nits, duplicates, naming, scope, sequencing, a failing
// optional check, a conflict) goes back to the clanker / adoption path or
// is recorded as advisory, and the person meets the PR at the merge gate
// (090 ready-for-human-merge), where the untrusted-author merge is theirs.

// securitySensitiveClasses are the DecisionRiskClass names that count as
// "fishy and insecure". The other classes (licensing, external side
// effects, data loss, unverifiable, merge policy) are merge-gate
// decisions, not CI/Review ones, for a contributor's PR.
var securitySensitiveClasses = map[string]bool{
	"security":                true,
	"permissions-credentials": true,
	"sensitive-paths":         true,
}

// securityFindingTerms is the wording in a finding or summary that flags
// insecure or suspicious behaviour. Deliberately narrower than
// reviewtext.RiskEscalationTerms: "breaking", "cost", "license" and the
// like are not security.
var securityFindingTerms = []string{
	"security", "vulnerab", "insecure", "secret", "credential", "token leak", "leaks", "exfiltrat",
	"authentication", "authorization", "permission", "privilege", "injection", "malicious",
	"suspicious", "backdoor", "obfuscat", "supply chain", "supply-chain", "typosquat",
	"remote code", "arbitrary code", "shell out", "curl | sh", "unpinned action", "high-risk",
}

// SecuritySensitivePaths resolves the operator's extra globs
// (`security.sensitive_paths`) on top of sentinel.DefaultSensitivePaths.
func SecuritySensitivePaths(extra []string) []string {
	out := append([]string{}, sentinel.DefaultSensitivePaths...)
	for _, g := range extra {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// IsSecuritySensitiveFinding is the single early-exit predicate for a
// non-hive-authored PR in CI/Review. It reports what the review found that
// is fishy or insecure, in the words the 085 comment and the tracker print
// after "the review found:"; an empty string means nothing security-
// relevant was found and the PR must keep moving.
//
// Triggers, in order:
//   - the reviewer's explicit `<!-- hive-decision-risk: high -->` marker;
//   - a decision question in a security-class risk class (security,
//     permissions-credentials, sensitive-paths — workflows, CODEOWNERS,
//     deploy, Dockerfiles, dependency manifests, …);
//   - an in-scope finding on a sensitive path (defaults plus
//     `security.sensitive_paths`);
//   - an in-scope finding, or the review summary, flagging insecure or
//     suspicious behaviour (securityFindingTerms).
func IsSecuritySensitiveFinding(r PerspectiveReport, sensitivePaths []string) (string, bool) {
	if r.DecisionRiskHigh {
		return "the reviewer marked this change high-risk", true
	}
	if r.Perspective == PerspectiveSecurity && r.Verdict == VerdictRequiresHuman {
		// The security perspective withholding approval is itself the
		// reviewer flagging the change, whatever words it used.
		if summary := strings.TrimSpace(r.Summary); summary != "" {
			return "the security review withheld approval: " + summary, true
		}
		return "the security review withheld approval", true
	}
	if class, risk := DecisionQuestionRiskClassFor(r.DecisionQuestion, r.PermissionsTighteningOnly); risk && securitySensitiveClasses[class] {
		return class + " question from the " + string(r.Perspective) + " review", true
	}
	paths := sensitivePaths
	if paths == nil {
		paths = SecuritySensitivePaths(nil)
	}
	for _, f := range r.Findings {
		if !InScopeFinding(f) {
			continue
		}
		if file := strings.TrimSpace(f.File); file != "" && intent.PathMatchesAny(file, paths) {
			return fmt.Sprintf("sensitive path %s: %s finding %q", file, r.Perspective, strings.TrimSpace(f.Title)), true
		}
	}
	for _, f := range r.Findings {
		if !InScopeFinding(f) {
			continue
		}
		if securityTermIn(f.Title, r.PermissionsTighteningOnly) || securityTermIn(f.Summary, r.PermissionsTighteningOnly) {
			return fmt.Sprintf("%s finding %q flags insecure behaviour", r.Perspective, strings.TrimSpace(f.Title)), true
		}
	}
	if securityTermIn(r.Summary, r.PermissionsTighteningOnly) {
		return "the " + string(r.Perspective) + " review flags insecure behaviour", true
	}
	return "", false
}

func securityTermIn(text string, permissionsTighteningOnly bool) bool {
	q := strings.ToLower(strings.TrimSpace(text))
	if q == "" {
		return false
	}
	for _, term := range securityFindingTerms {
		if permissionsTighteningOnly && permissionsTighteningSuppressedTerms[term] {
			continue
		}
		if strings.Contains(q, term) {
			return true
		}
	}
	return false
}

// securityFindingAcross runs IsSecuritySensitiveFinding over the reports
// that would hold the PR for a person (a decision marker or a
// requires_human verdict) and returns the first finding.
func securityFindingAcross(reports []PerspectiveReport, sensitivePaths []string) (string, bool) {
	for _, r := range reports {
		if !r.DecisionRequested && r.Verdict != VerdictRequiresHuman {
			continue
		}
		if finding, ok := IsSecuritySensitiveFinding(r, sensitivePaths); ok {
			return finding, true
		}
	}
	return "", false
}

// contributorHandsFree reports whether the contributor rule applies to the
// aggregate: a PR the hive did not author, with the global
// `self_authored_decision_policy: escalate` opt-out not set.
func contributorHandsFree(agg Aggregate, opts AggregateOptions) bool {
	return !agg.HiveAuthored && !strings.EqualFold(strings.TrimSpace(opts.SelfAuthoredDecisionPolicy), "escalate")
}

// reportsHaveInScopeFinding says the review left the author something to
// fix, so the hands-free resolution is the fixer lane, not approve.
func reportsHaveInScopeFinding(reports []PerspectiveReport) bool {
	for _, r := range reports {
		for _, f := range r.Findings {
			if InScopeFinding(f) {
				return true
			}
		}
	}
	return false
}

// contributorDecisionAdvisory is the audit reason for a contributor-lane
// review hold (decision question or requires_human) with no security
// finding behind it.
func contributorDecisionAdvisory(questions []string) string {
	const lead = "no security finding — the PR keeps moving; a person has the last word at the merge gate"
	var cleaned []string
	for _, q := range questions {
		if q = strings.TrimSpace(q); q != "" {
			cleaned = append(cleaned, q)
		}
	}
	sort.Strings(cleaned)
	if len(cleaned) == 0 {
		return lead
	}
	return lead + "; recorded as advisory: " + strings.Join(cleaned, " | ")
}

// contributorHandsFreeAggregate resolves a contributor-PR review hold that
// no security finding backs: to the fixer lane when the review left
// something to fix (the clanker / adoption path is the mover), otherwise
// to approve so the PR reaches the merge gate, where a person decides.
// Either way nobody is asked while the PR is in CI/Review.
func contributorHandsFreeAggregate(agg Aggregate, opts AggregateOptions, needsFix bool, maxFix int, advisory []string) Aggregate {
	if needsFix {
		if opts.FixAttempts >= maxFix {
			// The clanker ran out of attempts: there is no mover left, and
			// that — not the question — is what the person is told.
			agg.Verdict = VerdictRequiresHuman
			agg.RequiresHuman = true
			agg.DecisionRequested = false
			agg.DecisionQuestion = ""
			agg.DecisionFinding = fmt.Sprintf("fix cycle cap reached (%d/%d)", opts.FixAttempts, maxFix)
			agg.Reasons = []string{agg.DecisionFinding}
			return agg
		}
		agg.Verdict = VerdictChangesRequested
		agg.FixCycle = true
		markRequiresHumanPerspectivesAsChangesRequested(&agg)
	} else {
		agg.Verdict = VerdictApprove
		agg.MergeEligible = true
		markRequiresHumanPerspectivesAsApprove(&agg)
		agg.Reasons = nil
	}
	agg.RequiresHuman = false
	agg.DecisionRequested = false
	agg.DecisionQuestion = ""
	agg.Reasons = append(append([]string{}, agg.Reasons...), contributorDecisionAdvisory(advisory))
	sortReasons(agg.Reasons)
	return agg
}
