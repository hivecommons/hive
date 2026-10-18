package decisionpolicy

import "testing"

func TestClassifyResolvesNonRiskEngineeringQuestions(t *testing.T) {
	cases := []string{
		"which ADR gets renumbered / use a v6-only ADR block",
		"supersede into an App PR vs push to contributor branch",
	}
	for _, q := range cases {
		if got := Classify(q); got.Escalate || got.Category != "engineering" {
			t.Fatalf("Classify(%q) = %+v, want engineering resolve", q, got)
		}
	}
}

func TestClassifyEscalatesRiskQuestions(t *testing.T) {
	cases := []string{
		"rotate the deploy key",
		"delete production data",
		"change license",
		"spend > $X",
		"needs owner's GitHub secret",
	}
	for _, q := range cases {
		if got := Classify(q); !got.Escalate || got.Category == "" {
			t.Fatalf("Classify(%q) = %+v, want risk escalation", q, got)
		}
	}
}

func TestEffectivePolicyDefaultsResolve(t *testing.T) {
	if got := EffectivePolicy(""); got != PolicyResolve {
		t.Fatalf("empty policy = %q", got)
	}
	if got := EffectivePolicy("escalate"); got != PolicyEscalate {
		t.Fatalf("escalate policy = %q", got)
	}
}
