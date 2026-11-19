package reviewrules

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// BlockMarker names the hidden block a kicked agent may end its reply with.
// The block is the agent's own structured statement of the rules the reviewer
// stated; the hive validates it field by field and trusts none of it beyond
// what it can tie to a review comment it saw.
const BlockMarker = "hive:review-rules"

// MaxBlockRules bounds how many rules one reply may carry.
const MaxBlockRules = 10

const (
	maxEvidenceURLLen = 300
	maxAppliesToLen   = 100
)

var blockRe = regexp.MustCompile(`(?s)<!--\s*hive:review-rules\s+(\{.*?\})\s*-->`)

// evidenceURLRe accepts only a GitHub pull request or issue URL, optionally
// with a comment anchor.
var evidenceURLRe = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/(?:pull|issues)/[0-9]+(?:#[A-Za-z0-9_-]+)?$`)

var appliesToRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Scope says where a rule applies.
type Scope string

const (
	// ScopeAgent applies to one agent's work, across repos.
	ScopeAgent Scope = "agent"
	// ScopeWorkflow applies to one named workflow's runs.
	ScopeWorkflow Scope = "workflow"
	// ScopeRepo applies to one repository.
	ScopeRepo Scope = "repo"
	// ScopeGlobal applies to every agent.
	ScopeGlobal Scope = "global"
)

// BlockRule is one entry of the agent's structured block, as validated.
type BlockRule struct {
	Text        string `json:"text"`
	Scope       string `json:"scope"`
	EvidenceURL string `json:"evidence_url"`
	AppliesTo   string `json:"applies_to"`
}

type rawBlock struct {
	Rules []BlockRule `json:"rules"`
}

// ParseBlocks reads every hive:review-rules block in a reply body. Entries that
// fail validation are returned as errors, one per entry, and never as rules:
// one bad entry does not discard its valid siblings. The total is capped at
// MaxBlockRules; a reply with more has the excess reported as an error.
func ParseBlocks(body string) ([]BlockRule, []error) {
	var rules []BlockRule
	var errs []error
	for _, m := range blockRe.FindAllStringSubmatch(body, -1) {
		dec := json.NewDecoder(strings.NewReader(m[1]))
		dec.DisallowUnknownFields()
		var rb rawBlock
		if err := dec.Decode(&rb); err != nil {
			errs = append(errs, fmt.Errorf("block is not valid JSON of the expected shape: %w", err))
			continue
		}
		for i, r := range rb.Rules {
			if len(rules) >= MaxBlockRules {
				errs = append(errs, fmt.Errorf("more than %d rules in one reply; the rest are ignored", MaxBlockRules))
				return rules, errs
			}
			v, err := ValidateBlockRule(r)
			if err != nil {
				errs = append(errs, fmt.Errorf("rule %d: %w", i+1, err))
				continue
			}
			rules = append(rules, v)
		}
	}
	return rules, errs
}

// ValidateBlockRule checks one entry and returns it normalised.
func ValidateBlockRule(r BlockRule) (BlockRule, error) {
	r.Text = NormalizeText(r.Text)
	switch {
	case r.Text == "":
		return r, fmt.Errorf("text is empty")
	case utf8.RuneCountInString(r.Text) > MaxRuleRunes:
		return r, fmt.Errorf("text is over %d characters", MaxRuleRunes)
	}
	r.Scope = strings.ToLower(strings.TrimSpace(r.Scope))
	r.AppliesTo = strings.TrimSpace(r.AppliesTo)
	r.EvidenceURL = strings.TrimSpace(r.EvidenceURL)
	if len(r.EvidenceURL) > maxEvidenceURLLen || !evidenceURLRe.MatchString(r.EvidenceURL) {
		return r, fmt.Errorf("evidence_url must be a GitHub pull request or issue comment URL")
	}
	if r.AppliesTo != "" && (len(r.AppliesTo) > maxAppliesToLen || !appliesToRe.MatchString(r.AppliesTo)) {
		return r, fmt.Errorf("applies_to has characters a name cannot have")
	}
	switch Scope(r.Scope) {
	case "", ScopeAgent, ScopeGlobal:
	case ScopeWorkflow:
		if r.AppliesTo == "" {
			return r, fmt.Errorf("scope workflow needs applies_to, the workflow name")
		}
	case ScopeRepo:
		if !repoSlugRe.MatchString(r.AppliesTo) {
			return r, fmt.Errorf("scope repo needs applies_to as owner/name")
		}
	default:
		return r, fmt.Errorf("scope must be one of agent, workflow, repo, global")
	}
	return r, nil
}
