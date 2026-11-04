package review

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/intent"
)

// DecisionQuestionClass sorts one reviewer "decision needed" ask on a
// hive-authored PR into what it needs: a person's sign-off, or the hive's own
// delegated decision. hivecommons/docs#618 parked a green, finding-free,
// hive-authored PR at stage/085 because one perspective ended with a
// scope question ("should this merge ahead of that workflow edit?") that the
// risk-class denylist (DecisionRiskClasses) read as a sensitive-path ask —
// the owner's words: "this is auto-merge material, not needs-human".
type DecisionQuestionClass string

const (
	// DecisionQuestionPolicySignOff: the ask stays a person's. The PR edits
	// workflows, touches secrets / permissions / OWNERS /
	// security-labelled paths, changes the auto-merge or human-merge
	// policy, or the reviewer names an owner-only guard outright.
	DecisionQuestionPolicySignOff DecisionQuestionClass = "policy-sign-off"
	// DecisionQuestionScope: a scope or follow-up question ("should we
	// also…", "merge ahead of…", "apply X from the PR body", "confirm Y is
	// acceptable") on a hive-authored PR with CI green and no unresolved
	// finding on its head. The hive answers it through the delegate path.
	DecisionQuestionScope DecisionQuestionClass = "scope-question"
	// DecisionQuestionIneligible: not a candidate for auto-decision at all
	// (no question, not hive-authored, CI not green, findings open). The
	// ordinary 085 park stands.
	DecisionQuestionIneligible DecisionQuestionClass = "ineligible"
)

// DecisionQuestionInput is what the classifier sees. It is a value, not a
// GitHub read: callers gather the head's files and status first.
type DecisionQuestionInput struct {
	// Question is the reviewer's decision question, verbatim.
	Question string
	// HiveAuthored: the hive opened the PR, or adopted it in place.
	HiveAuthored bool
	// FromFork: the head lives in a fork of the base repository.
	FromFork bool
	// CIGreen: every gating check on the head passed.
	CIGreen bool
	// UnresolvedFindings: the head's review verdict carries a finding or a
	// changes-requested perspective that no fix has cleared.
	UnresolvedFindings bool
	// Files are the head's changed files with their patches (patch may be
	// empty when the caller did not fetch it).
	Files []DecisionQuestionFile
}

// DecisionQuestionFile is one changed file of the PR under question.
type DecisionQuestionFile struct {
	Path  string
	Patch string
}

// decisionPolicyPaths are the paths whose edit turns a reviewer question
// into a sign-off: trust and governance files, security policy, workflow
// and ruleset config, the hive's own policy files, and secrets-adjacent
// files. Deliberately narrower than sentinel.DefaultSensitivePaths, whose
// go.mod / Makefile / package.json entries guard supply chain, not policy.
var decisionPolicyPaths = []string{
	"OWNERS", "**/OWNERS", "OWNERS_ALIASES", "**/OWNERS_ALIASES",
	"CODEOWNERS", "**/CODEOWNERS", "MAINTAINERS", "**/MAINTAINERS",
	"GOVERNANCE.md", "SECURITY.md", "**/SECURITY.md",
	".github/rulesets/**", ".github/settings.yml", ".github/CODEOWNERS",
	"policies/**", "**/policies/**",
	"hive.yaml", "**/hive.yaml", "hive.yaml.dashboard", "**/hive.yaml.dashboard",
	"**/.env*", "**/*.pem", "**/*.key", "**/id_rsa*", "**/secrets*",
}

// decisionWorkflowPaths are CI-automation paths whose edits require a
// person's sign-off, regardless of where the PR originates.
var decisionWorkflowPaths = []string{".github/workflows/**", "**/.github/workflows/**", ".github/actions/**"}

// decisionPolicyConfigKeys are hive config keys whose change in a patch is
// a merge-policy change, whatever file carries them.
var decisionPolicyConfigKeys = []string{"auto_merge", "human_merge_paths", "self_authored_decision_policy", "auto_decide_questions"}

// decisionOwnerOnlyGuards are the asks a reviewer phrases as an explicit
// owner-only guard. Each is a whole-word match on the lower-cased question.
var decisionOwnerOnlyGuards = []*regexp.Regexp{
	regexp.MustCompile(`\bsentinel\b`),
	regexp.MustCompile(`\bgithub permissions?\b`),
	regexp.MustCompile(`\bapp permissions?\b`),
	regexp.MustCompile(`\borg(?:anization)? (?:permissions?|admin|owner|membership)\b`),
	regexp.MustCompile(`\bunauthori[sz]ed\b`),
	regexp.MustCompile(`\bsecrets?\b`),
	regexp.MustCompile(`\btokens?\b`),
	regexp.MustCompile(`\bcredentials?\b`),
	regexp.MustCompile(`\bbranch protection\b`),
	regexp.MustCompile(`\bcodeowners\b`),
	regexp.MustCompile(`\bowners file\b`),
	regexp.MustCompile(`\bowner-only\b`),
}

// ClassifyDecisionQuestion is the pure classifier behind the reviewer
// question auto-decide (governor.review.auto_decide_questions). The reason
// names the first rule that settled the class, for the INFO line and the
// audit record. Policy sign-off wins over scope: a question on a PR that
// edits OWNERS is the owner's however it is phrased.
func ClassifyDecisionQuestion(in DecisionQuestionInput) (DecisionQuestionClass, string) {
	q := strings.ToLower(strings.TrimSpace(in.Question))
	if q == "" {
		return DecisionQuestionIneligible, "no reviewer question"
	}
	for _, f := range in.Files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			continue
		}
		if intent.PathMatchesAny(p, decisionWorkflowPaths) {
			return DecisionQuestionPolicySignOff, "PR edits workflow path " + p
		}
		if intent.PathMatchesAny(p, decisionPolicyPaths) {
			return DecisionQuestionPolicySignOff, "PR touches policy path " + p
		}
		if key := decisionPatchPolicyKey(p, f.Patch); key != "" {
			return DecisionQuestionPolicySignOff, "PR changes " + key + " in " + p
		}
	}
	for _, guard := range decisionOwnerOnlyGuards {
		if m := guard.FindString(q); m != "" {
			return DecisionQuestionPolicySignOff, "ask names owner-only guard " + strconv.Quote(m)
		}
	}
	switch {
	case !in.HiveAuthored:
		return DecisionQuestionIneligible, "PR is not hive-authored"
	case !in.CIGreen:
		return DecisionQuestionIneligible, "CI is not green on head"
	case in.UnresolvedFindings:
		return DecisionQuestionIneligible, "head has unresolved review findings"
	}
	return DecisionQuestionScope, "scope/follow-up question on a green hive-authored PR with no findings"
}

// decisionPatchPolicyKey reports the merge-policy config key a patch adds
// or removes in a hive config file; "" when none. Only YAML-shaped files are
// read, so a Go test naming `auto_merge` in a string is not a policy edit.
func decisionPatchPolicyKey(p, patch string) string {
	if patch == "" {
		return ""
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".yaml", ".yml", ".dashboard":
	default:
		return ""
	}
	for _, line := range strings.Split(patch, "\n") {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		body := strings.TrimSpace(line[1:])
		for _, key := range decisionPolicyConfigKeys {
			if strings.HasPrefix(body, key+":") {
				return key
			}
		}
	}
	return ""
}
