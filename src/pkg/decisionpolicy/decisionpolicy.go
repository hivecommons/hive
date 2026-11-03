package decisionpolicy

import (
	"regexp"
	"strings"
)

const (
	PolicyResolve  = "resolve"
	PolicyEscalate = "escalate"
)

type Classification struct {
	Escalate bool
	Category string
	Evidence string
	RiskLine bool
}

func EffectivePolicy(policy string) string {
	if strings.EqualFold(strings.TrimSpace(policy), PolicyEscalate) {
		return PolicyEscalate
	}
	return PolicyResolve
}

func Classify(text string) Classification {
	clean := strings.ToLower(strings.TrimSpace(text))
	if clean == "" {
		return Classification{}
	}
	if category, ok := riskLineCategory(text); ok {
		return Classification{Escalate: true, Category: category, RiskLine: true, Evidence: "Risk: " + category}
	}
	for _, r := range riskRules {
		for _, term := range r.Terms {
			if strings.Contains(clean, term) {
				return Classification{Escalate: true, Category: r.Category, Evidence: term}
			}
		}
	}
	if evidence, ok := repoLocalGateEvidence(text); ok {
		return Classification{Escalate: false, Category: CategoryRepoLocalGate, Evidence: evidence}
	}
	for _, term := range nonRiskTerms {
		if strings.Contains(clean, term) {
			return Classification{Escalate: false, Category: "engineering", Evidence: term}
		}
	}
	return Classification{}
}

func ResolvesWithoutHuman(text string) bool {
	c := Classify(text)
	return !c.Escalate && (c.Category == "engineering" || c.Category == CategoryRepoLocalGate)
}

// CategoryRepoLocalGate classifies a park whose only blocker is a target
// repository's own instruction-file gate — "offer a spec and wait for
// acceptance", "confirm scope before implementing", "stop and report a
// missing registered sibling checkout" — rather than a risk a person must
// own. A hive dispatch of an accepted issue is that acceptance, so such a
// park resolves without a human.
const CategoryRepoLocalGate = "repo-local confirmation gate"

// repoLocalGatePattern matches the vocabulary of an agent deferring to a
// repository's instruction-file gate. Each alternative is anchored on a term
// that does not appear in a genuine risk park: the instruction file itself,
// a spec-workflow gate, or a registered sibling path reported missing.
var repoLocalGatePattern = regexp.MustCompile(`(?i)(?:` +
	`\b(?:agents\.md|claude\.md|copilot-instructions\.md|\.cursorrules|instruction files?)\b.{0,160}\b(?:gates?|confirm(?:ation)?|approval|accept(?:ance|ed)?|offer(?:ing)?|spec|wait(?:ing)?)\b|` +
	`\b(?:confirmation|approval|spec(?:[-\s]?(?:workflow|capture|trigger)))\s+gates?\b|` +
	`\bspec_trigger_threshold\b|` +
	`\bcaptur(?:e|ed|ing)\b.{0,40}\bas a\s+(?:\S+\s+)?spec\b|` +
	`\boffer(?:ing|ed)?\b.{0,24}\bspec\b|` +
	`\bspec[-\s]capture\b|` +
	`\bspec[-\s]workflow\b.{0,24}\bdecision\b|` +
	`\b(?:proceed|go ahead|implement(?:ation)?)\b.{0,40}\bwithout (?:a |one )?(?:spec|one)\b|` +
	`\brepo_location_missing\b|` +
	`\bmissing registered (?:path|location|checkout)s?\b|` +
	`\b(?:registered|sibling|companion)\b.{0,40}\b(?:location|path|checkout|repo(?:sitory)?)\b.{0,80}\b(?:missing|absent|not (?:present|found|available))\b` +
	`)`)

// repoLocalGateEvidence returns the first gate phrase found in text.
func repoLocalGateEvidence(text string) (string, bool) {
	m := repoLocalGatePattern.FindString(text)
	if m == "" {
		return "", false
	}
	return strings.Join(strings.Fields(m), " "), true
}

// IsRepoLocalGate reports whether text describes a park on a target
// repository's own confirmation/spec/approval gate or missing registered
// sibling checkout, with no risk category present. Such a park is needless:
// the hive dispatch already satisfied the gate.
func IsRepoLocalGate(text string) bool {
	c := Classify(text)
	return !c.Escalate && c.Category == CategoryRepoLocalGate
}

// DispatchSatisfiesRepoGates is the instruction every implementation agent
// gets alongside a target repository's own AGENTS.md / CLAUDE.md: the hive
// dispatch of an accepted issue is the acceptance those files' gates wait
// for. Five contributor parks on one repository in one day (evidence in
// hivecommons/hive#13059) were agents obeying a "recognise spec-worthy work
// and offer a spec" rule literally on issues the hive had already triaged
// and dispatched.
const DispatchSatisfiesRepoGates = "A hive dispatch of an accepted issue IS the acceptance any repo-local " +
	"instruction file (AGENTS.md, CLAUDE.md, .github/copilot-instructions.md, .cursorrules) asks for: its " +
	"\"offer a spec first\", \"confirm scope\", \"ask before implementing\", \"wait for approval\" and " +
	"similar confirmation gates are already satisfied — do not offer, do not ask, do not park on them; " +
	"implement as the issue scopes it. Park for a maintainer decision only on a real risk category " +
	"(security, data loss, irreversible or public-facing actions, governance/licensing, spend, " +
	"credentials or owner-only infrastructure) or a genuine A/B the issue body leaves open."

// MissingCompanionCheckoutRule tells an agent what to do when a repository's
// tooling reports a registered sibling path missing under the workspace root
// (a repo tool answering `repo_location_missing`): clone it and go on.
const MissingCompanionCheckoutRule = "If a repository tool or instruction reports a registered sibling path under " +
	"your workspace root as missing, and the two path segments under that root are <org>/<repo> of a " +
	"repository you can read on this host (same organisation), clone that repository there (a shallow " +
	"clone is fine) and continue — that is the intended checkout, not a substitution; never park or stop for it."

var riskLineRE = regexp.MustCompile(`(?im)^\s*Risk:\s*(.+?)\s*$`)

func riskLineCategory(text string) (string, bool) {
	m := riskLineRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", false
	}
	line := strings.ToLower(strings.TrimSpace(m[1]))
	for _, r := range riskRules {
		if strings.Contains(line, r.Category) {
			return r.Category, true
		}
		for _, alias := range r.Aliases {
			if strings.Contains(line, alias) {
				return r.Category, true
			}
		}
	}
	return "", false
}

type riskRule struct {
	Category string
	Aliases  []string
	Terms    []string
}

var riskRules = []riskRule{
	{Category: "security posture", Aliases: []string{"security", "vulnerability", "vulnerable", "cve"}, Terms: []string{"security posture", "security", "vulnerab", "cve", "exploit", "permission", "privilege", "authentication", "authorization", "oauth"}},
	{Category: "data loss", Aliases: []string{"data", "destructive"}, Terms: []string{"data loss", "production data", "delete production", "drop table", "destructive", "destroy data", "erase data", "purge data"}},
	{Category: "irreversible or public-facing behavior", Aliases: []string{"irreversible", "public-facing", "public facing", "breaking"}, Terms: []string{"irreversible", "public-facing", "public facing", "breaking change", "external behavior", "user-visible behavior", "customer-facing"}},
	{Category: "governance/policy/licensing", Aliases: []string{"governance", "policy", "licensing", "license", "legal", "copyright"}, Terms: []string{"governance", "policy", "licens", "copyright", "legal"}},
	{Category: "spend", Aliases: []string{"spend", "cost", "billing", "budget"}, Terms: []string{"spend", "cost", "billing", "budget", "paid tier", "quota purchase"}},
	{Category: "credentials/secrets/owner-only infrastructure", Aliases: []string{"credential", "secret", "deploy key", "owner", "infra", "infrastructure"}, Terms: []string{"credential", "secret", "deploy key", "private key", "api key", "token", "owner-only", "owner only", "cannot access", "infra", "infrastructure"}},
	{Category: "human intent", Aliases: []string{"human intent", "owner intent", "maintainer intent"}, Terms: []string{"human intent", "owner intent", "maintainer intent", "cannot be inferred", "unclear intent"}},
}

var nonRiskTerms = []string{
	"approach a", "approach b", "a vs b", "option a", "option b", "pick a", "pick b",
	"land before", "land now", "land later", "sequenc", "groundwork", "dead-code", "dead code",
	"loader", "consumer", "naming", "namespace", "nesting", "top-level", "config shape",
	"configuration shape", "config namespace", "fixed ledger path", "scope", "placement", "organize",
	"organisation", "organization", "numbering", "numbered", "renumber", "adr", "v6-only",
	"app pr", "contributor branch", "supersede", "simplest additive", "backward-compatible",
}
