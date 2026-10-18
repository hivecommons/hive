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
	for _, term := range nonRiskTerms {
		if strings.Contains(clean, term) {
			return Classification{Escalate: false, Category: "engineering", Evidence: term}
		}
	}
	return Classification{}
}

func ResolvesWithoutHuman(text string) bool {
	c := Classify(text)
	return !c.Escalate && c.Category == "engineering"
}

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
