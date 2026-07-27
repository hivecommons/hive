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
		{"explicit true wins at L1", PlanningConfig{PlanFromLabel: boolPtr(true)}, 1, true},
		{"explicit false wins at L6", PlanningConfig{PlanFromLabel: boolPtr(false)}, 6, false},
		{"nil defaults off below L5", PlanningConfig{}, 3, false},
		{"nil defaults off at L4 (boundary)", PlanningConfig{}, 4, false},
		{"nil defaults on at L5 (boundary)", PlanningConfig{}, 5, true},
		{"nil defaults on above L5", PlanningConfig{}, 6, true},
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
	// The ACMM gate then applies: on at L5+ (architect scheduled), off below.
	if cfg.Planning.PlanFromLabelEnabled(5) != true || cfg.Planning.PlanFromLabelEnabled(4) != false {
		t.Errorf("ACMM gate wrong for nil PlanFromLabel")
	}
}
