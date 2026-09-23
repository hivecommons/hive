package classify

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/issueshape"
)

type TriageVerdict string

const (
	TriageFix     TriageVerdict = "fix"
	TriageSpec    TriageVerdict = "spec"
	TriageClarify TriageVerdict = "clarify"
)

type TriageDecision struct {
	Verdict   TriageVerdict
	Rationale string
	Signals   []string
}

func Triage(issue github.Issue, c Classification, cfg config.TriageConfig) TriageDecision {
	labels := normalizedLabels(issue.Labels)
	if hasLabel(labels, "run/spec") {
		return triageDecision(TriageSpec, "run/spec override label", "label:run/spec")
	}
	if hasLabel(labels, "run/fix") {
		return triageDecision(TriageFix, "run/fix override label", "label:run/fix")
	}
	body := strings.TrimSpace(issue.Body)
	if min := cfg.EffectiveMinBodyChars(); len(body) < min {
		return triageDecision(TriageClarify, fmt.Sprintf("issue body is shorter than %d characters", min), "body:short")
	}
	if token, ok := issueshape.UnchosenOptionList(body); ok {
		return triageDecision(TriageClarify, "issue body still contains an unchosen option list", "body:unchosen_option:"+token)
	}
	if token, ok := issueshape.UnsubstitutedTemplatePlaceholder(body); ok {
		return triageDecision(TriageClarify, "issue body still contains an unsubstituted template placeholder", "body:placeholder:"+token)
	}
	if label, ok := firstMatchingLabel(labels, cfg.EffectiveSpecLabels()); ok {
		return triageDecision(TriageSpec, "spec label requires a spec run", "label:"+label)
	}
	if c.Tier == TierComplex {
		return triageDecision(TriageSpec, "complex issue requires a spec run", "tier:Complex")
	}
	if label, ok := firstMatchingLabel(labels, cfg.EffectiveFixLabels()); ok {
		return triageDecision(TriageFix, "fix label routes directly to implementation", "label:"+label)
	}
	if c.Tier == TierSimple {
		return triageDecision(TriageFix, "simple issue routes directly to implementation", "tier:Simple")
	}
	return triageDecision(TriageFix, "medium issue routes directly to implementation", "tier:Medium")
}

func triageDecision(verdict TriageVerdict, rationale string, signals ...string) TriageDecision {
	return TriageDecision{Verdict: verdict, Rationale: rationale, Signals: signals}
}

func normalizedLabels(labels []string) map[string]string {
	out := make(map[string]string, len(labels))
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed != "" {
			out[strings.ToLower(trimmed)] = trimmed
		}
	}
	return out
}

func hasLabel(labels map[string]string, want string) bool {
	_, ok := labels[strings.ToLower(strings.TrimSpace(want))]
	return ok
}

func firstMatchingLabel(labels map[string]string, wants []string) (string, bool) {
	for _, want := range wants {
		key := strings.ToLower(strings.TrimSpace(want))
		if key == "" {
			continue
		}
		if got, ok := labels[key]; ok {
			return got, true
		}
	}
	return "", false
}
