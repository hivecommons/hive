package config

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(b bool) *bool { return &b }

func TestPlanFromLabelEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  PlanningConfig
		acmm int
		want bool
	}{
		// Security: the label trigger is OFF by default (it pipes a raw issue
		// body into the architect's prompt with no per-kick review), so nil
		// config never enables it regardless of ACMM level. Explicit true is
		// honored, but still gated on L5+ where the decomposing architect is
		// actually scheduled — an explicit true below L5 is inert.
		{"explicit true is L5-gated: inert at L1", PlanningConfig{PlanFromLabel: boolPtr(true)}, 1, false},
		{"explicit true fires at L5", PlanningConfig{PlanFromLabel: boolPtr(true)}, 5, true},
		{"explicit true fires above L5", PlanningConfig{PlanFromLabel: boolPtr(true)}, 6, true},
		{"explicit false wins at L6", PlanningConfig{PlanFromLabel: boolPtr(false)}, 6, false},
		{"nil defaults off below L5", PlanningConfig{}, 3, false},
		{"nil defaults off at L4 (boundary)", PlanningConfig{}, 4, false},
		{"nil defaults OFF at L5 (was on; now opt-in)", PlanningConfig{}, 5, false},
		{"nil defaults OFF above L5", PlanningConfig{}, 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.PlanFromLabelEnabled(tc.acmm); got != tc.want {
				t.Errorf("PlanFromLabelEnabled(%d) = %v, want %v", tc.acmm, got, tc.want)
			}
		})
	}
}

func TestClassifierConfig_YAML(t *testing.T) {
	src := `
classifier:
  backend: jev
  mode: enforce
  jev:
    provider: openrouter
    model: typesafe/jev-1.13
    endpoint: https://example.test/v1/systemone
    api_key_env: JEV_API_KEY
    min_confidence: 0.9
    timeout: 3s
    decisions: [lane, tier]
  simple_keywords: [tweak, nit]
  complex_signals: [distributed-consensus]
planning:
  plan_from_label: true
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg.Classifier.SimpleKeywords, []string{"tweak", "nit"}) {
		t.Errorf("simple_keywords = %v", cfg.Classifier.SimpleKeywords)
	}
	if !reflect.DeepEqual(cfg.Classifier.ComplexSignals, []string{"distributed-consensus"}) {
		t.Errorf("complex_signals = %v", cfg.Classifier.ComplexSignals)
	}
	if cfg.Classifier.EffectiveBackend() != "jev" || cfg.Classifier.EffectiveMode() != "enforce" {
		t.Errorf("classifier backend/mode = %q/%q", cfg.Classifier.EffectiveBackend(), cfg.Classifier.EffectiveMode())
	}
	if got := cfg.Classifier.EffectiveJev(); got.Provider != "openrouter" || got.Model != "typesafe/jev-1.13" || got.Endpoint != "https://example.test/v1/systemone" || got.APIKeyEnv != "JEV_API_KEY" || got.MinConfidence != 0.9 || got.Timeout.String() != "3s" || !reflect.DeepEqual(got.Decisions, []string{"lane", "tier"}) {
		t.Errorf("jev config = %+v", got)
	}
	if cfg.Planning.PlanFromLabel == nil || !*cfg.Planning.PlanFromLabel {
		t.Errorf("plan_from_label = %v, want true", cfg.Planning.PlanFromLabel)
	}
}

func TestClassifierConfig_AbsentIsZero(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project: {}\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Absent blocks yield empty lists (classifier keeps built-in defaults) and a
	// nil PlanFromLabel (falls back to the ACMM gate).
	if len(cfg.Classifier.SimpleKeywords) != 0 || len(cfg.Classifier.ComplexSignals) != 0 {
		t.Errorf("expected empty classifier lists, got %+v", cfg.Classifier)
	}
	if cfg.Planning.PlanFromLabel != nil {
		t.Errorf("expected nil PlanFromLabel, got %v", *cfg.Planning.PlanFromLabel)
	}
	// Security: the label trigger is OFF by default, so an absent block never
	// enables it — at any ACMM level. It must be opted into explicitly.
	if cfg.Planning.PlanFromLabelEnabled(5) != false || cfg.Planning.PlanFromLabelEnabled(4) != false {
		t.Errorf("absent PlanFromLabel should be OFF by default at every level")
	}
}

func TestClassifierConfigValidation(t *testing.T) {
	valid := Config{Project: ProjectConfig{Org: "o"}, Agents: map[string]AgentConfig{"scanner": {}}, GitHub: GitHubConfig{Token: "x"}}
	valid.Classifier = ClassifierConfig{Backend: "jev", Mode: "shadow", Jev: JevClassifierConfig{Provider: "typesafe", MinConfidence: 0.8, Timeout: 2}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid classifier config rejected: %v", err)
	}
	cases := []ClassifierConfig{
		{Backend: "magic"},
		{Mode: "replace"},
		{Backend: "jev", Jev: JevClassifierConfig{Provider: "other"}},
		{Backend: "jev", Jev: JevClassifierConfig{MinConfidence: 1.2}},
		{Backend: "jev", Jev: JevClassifierConfig{Decisions: []string{"lane", "other"}}},
	}
	for _, cc := range cases {
		cfg := valid
		cfg.Classifier = cc
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate() for %+v = nil, want error", cc)
		}
	}
}

func TestJevClassifierEffectiveDefaults(t *testing.T) {
	j := JevClassifierConfig{}
	if j.EffectiveProvider() != "" {
		t.Fatalf("raw provider effective = %q, want empty before ClassifierConfig defaults", j.EffectiveProvider())
	}
	cfg := ClassifierConfig{Mode: "enforce"}
	got := cfg.EffectiveJev()
	if got.Provider != "openrouter" || got.Model != "typesafe/jev-1.13" || got.Endpoint != "https://openrouter.ai/api/v1/systemone" || got.MinConfidence != 0.8 || got.Timeout.String() != "2s" || got.EffectiveMode() != "enforce" || !reflect.DeepEqual(got.EffectiveDecisions(), []string{"lane", "tier", "triage"}) {
		t.Fatalf("openrouter defaults = %+v", got)
	}
	typed := ClassifierConfig{Jev: JevClassifierConfig{Provider: "typesafe"}}
	if got := typed.EffectiveJev(); got.Model != "jev-latest" || got.Endpoint != "https://api.typesafe.ai/v1/systemone" || got.EffectiveProvider() != "typesafe" {
		t.Fatalf("typesafe defaults = %+v", got)
	}
	custom := JevClassifierConfig{Model: "m", Endpoint: "https://jev.example", MinConfidence: 0.7, Timeout: 5, Mode: "shadow", Decisions: []string{" lane ", "", "tier"}}
	if custom.EffectiveModel() != "m" || custom.EffectiveEndpoint() != "https://jev.example" || custom.EffectiveMinConfidence() != 0.7 || custom.EffectiveTimeout() != 5 || custom.EffectiveMode() != "shadow" || !reflect.DeepEqual(custom.EffectiveDecisions(), []string{"lane", "tier"}) {
		t.Fatalf("custom effective methods returned unexpected values: %+v", custom)
	}
}

func TestAPIKeySHA256(t *testing.T) {
	if got := APIKeySHA256(""); got != "" {
		t.Fatalf("empty key hash = %q, want empty", got)
	}
	sum := sha256.Sum256([]byte("secret"))
	if got, want := APIKeySHA256("secret"), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("key hash = %q, want %q", got, want)
	}
}
