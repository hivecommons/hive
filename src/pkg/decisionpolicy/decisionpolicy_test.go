package decisionpolicy

import "testing"

func TestClassifyResolvesNonRiskEngineeringQuestions(t *testing.T) {
	cases := []string{
		"which ADR gets renumbered / use a v6-only ADR block",
		"supersede into an App PR vs push to contributor branch",
		"Choose approach A vs B for config shape and scope",
	}
	for _, q := range cases {
		got := Classify(q)
		if got.Escalate || got.Category != "engineering" || got.Evidence == "" {
			t.Fatalf("Classify(%q) = %+v, want engineering resolve with evidence", q, got)
		}
		if !ResolvesWithoutHuman(q) {
			t.Fatalf("ResolvesWithoutHuman(%q) = false, want true", q)
		}
	}
}

func TestClassifyEscalatesRiskQuestions(t *testing.T) {
	cases := map[string]string{
		"rotate the deploy key":           "credentials/secrets/owner-only infrastructure",
		"delete production data":          "data loss",
		"change license":                  "governance/policy/licensing",
		"spend > $X":                      "spend",
		"needs owner's GitHub secret":     "credentials/secrets/owner-only infrastructure",
		"public-facing behavior change":   "irreversible or public-facing behavior",
		"security posture question":       "security posture",
		"human intent cannot be inferred": "human intent",
	}
	for q, wantCategory := range cases {
		got := Classify(q)
		if !got.Escalate || got.Category != wantCategory || got.Evidence == "" {
			t.Fatalf("Classify(%q) = %+v, want %q escalation with evidence", q, got, wantCategory)
		}
		if ResolvesWithoutHuman(q) {
			t.Fatalf("ResolvesWithoutHuman(%q) = true, want false", q)
		}
	}
}

func TestClassifyRiskLineUsesAliases(t *testing.T) {
	cases := map[string]string{
		"Question: rotate?\nRisk: cve exposure": "security posture",
		"Risk: destructive production change":   "data loss",
		"Risk: breaking public behavior":        "irreversible or public-facing behavior",
		"Risk: copyright/legal policy call":     "governance/policy/licensing",
		"Risk: budget increase":                 "spend",
		"Risk: owner secret required":           "credentials/secrets/owner-only infrastructure",
		"Risk: maintainer intent":               "human intent",
	}
	for body, wantCategory := range cases {
		got := Classify(body)
		if !got.Escalate || !got.RiskLine || got.Category != wantCategory || got.Evidence != "Risk: "+wantCategory {
			t.Fatalf("Classify(%q) = %+v, want risk-line %q", body, got, wantCategory)
		}
	}
}

func TestClassifyRiskLineUnknownFallsThrough(t *testing.T) {
	got := Classify("Risk: ordinary engineering\nQuestion: choose naming")
	if got.Escalate || got.Category != "engineering" {
		t.Fatalf("unknown Risk line with engineering question = %+v, want engineering", got)
	}
}

func TestClassifyEmptyAndUnknownStayUnclassified(t *testing.T) {
	for _, q := range []string{"", "   ", "what should we do next"} {
		got := Classify(q)
		if got.Escalate || got.Category != "" || got.Evidence != "" || got.RiskLine {
			t.Fatalf("Classify(%q) = %+v, want empty classification", q, got)
		}
		if ResolvesWithoutHuman(q) {
			t.Fatalf("ResolvesWithoutHuman(%q) = true, want false", q)
		}
	}
}

func TestEffectivePolicyDefaultsResolve(t *testing.T) {
	cases := map[string]string{
		"":           PolicyResolve,
		"resolve":    PolicyResolve,
		"unknown":    PolicyResolve,
		" ESCALATE ": PolicyEscalate,
	}
	for policy, want := range cases {
		if got := EffectivePolicy(policy); got != want {
			t.Fatalf("EffectivePolicy(%q) = %q, want %q", policy, got, want)
		}
	}
}

func TestClassifyUnclassifiedTextDoesNotResolveWithoutHuman(t *testing.T) {
	q := "Agents do not edit repository settings; the owner runs: gh api --method PUT repos/o/r/branches/main/protection"
	got := Classify(q)
	if got.Escalate || got.Category != "" {
		t.Fatalf("Classify(%q) = %+v, want zero classification", q, got)
	}
	if ResolvesWithoutHuman(q) {
		t.Fatalf("ResolvesWithoutHuman(%q) = true, want false for unclassified text", q)
	}
}
