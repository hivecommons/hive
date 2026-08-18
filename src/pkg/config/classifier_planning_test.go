package config

import (
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
