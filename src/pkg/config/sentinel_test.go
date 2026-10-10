package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/sentinel"
)

func TestSentinelConfigDefaults(t *testing.T) {
	var s SentinelConfig
	if !s.IsEnabled() {
		t.Fatal("unset sentinel must be enabled")
	}
	if s.LabelOrDefault() != DefaultSentinelLabel {
		t.Fatalf("label=%q", s.LabelOrDefault())
	}
	if s.MaxActionsOrDefault() != DefaultSentinelMaxActions {
		t.Fatalf("max=%d", s.MaxActionsOrDefault())
	}
	if !reflect.DeepEqual(s.EffectiveSensitivePaths(), sentinel.DefaultSensitivePaths) {
		t.Fatal("nil sensitive_paths should resolve to defaults")
	}
	if !s.RepoAllowed("any/repo") {
		t.Fatal("empty repos means all")
	}
	if ec := s.EvaluatorConfig(); ec.SensitivePaths != nil || len(ec.Disabled) != 0 {
		t.Fatalf("evaluator config should pass through zero values: %+v", ec)
	}
}

func TestSentinelConfigOverrides(t *testing.T) {
	off := false
	s := SentinelConfig{
		Enabled:           &off,
		Label:             "  needs-security-look ",
		SensitivePaths:    []string{},
		DisabledBehaviors: []string{sentinel.RuleTestRemoval},
		ExemptLogins:      []string{"dependabot[bot]"},
		Repos:             []string{"Org/Repo"},
		MaxActions:        3,
	}
	if s.IsEnabled() {
		t.Fatal("explicit false must disable")
	}
	if s.LabelOrDefault() != "needs-security-look" {
		t.Fatalf("label=%q", s.LabelOrDefault())
	}
	if s.MaxActionsOrDefault() != 3 {
		t.Fatalf("max=%d", s.MaxActionsOrDefault())
	}
	if got := s.EffectiveSensitivePaths(); got == nil || len(got) != 0 {
		t.Fatalf("explicit empty list must stay empty, got %v", got)
	}
	if !s.RepoAllowed(" org/repo ") || s.RepoAllowed("org/other") {
		t.Fatal("repo allow-list should be case-insensitive and exact")
	}
	ec := s.EvaluatorConfig()
	if !reflect.DeepEqual(ec.Disabled, []string{sentinel.RuleTestRemoval}) || !reflect.DeepEqual(ec.ExemptLogins, []string{"dependabot[bot]"}) {
		t.Fatalf("evaluator config mismatch: %+v", ec)
	}
}

func TestValidateSentinel(t *testing.T) {
	if err := ValidateSentinel(SentinelConfig{DisabledBehaviors: []string{" Test_Removal "}}); err != nil {
		t.Fatalf("known rule (any case/space) should validate: %v", err)
	}
	err := ValidateSentinel(SentinelConfig{DisabledBehaviors: []string{"nope"}})
	if err == nil || !strings.Contains(err.Error(), "unknown behavior") {
		t.Fatalf("unknown rule should fail: %v", err)
	}
	if err := ValidateSentinel(SentinelConfig{SensitivePaths: []string{"OWNERS", " "}}); err == nil {
		t.Fatal("blank pattern should fail")
	}
	if err := ValidateSentinel(SentinelConfig{MaxActions: -1}); err == nil {
		t.Fatal("negative max_actions should fail")
	}
}

func TestSentinelBehaviorsAndDefaultPaths(t *testing.T) {
	bs := SentinelBehaviors()
	if len(bs) != len(sentinel.AllRules) {
		t.Fatalf("behaviors = %d, want %d", len(bs), len(sentinel.AllRules))
	}
	for i, b := range bs {
		if b.Name != sentinel.AllRules[i] || b.Description == "" {
			t.Fatalf("behavior %d = %+v", i, b)
		}
	}
	paths := DefaultSentinelSensitivePaths()
	if len(paths) != len(sentinel.DefaultSensitivePaths) {
		t.Fatal("default paths length mismatch")
	}
	paths[0] = "mutated"
	if sentinel.DefaultSensitivePaths[0] == "mutated" {
		t.Fatal("DefaultSentinelSensitivePaths must return a copy")
	}
}
