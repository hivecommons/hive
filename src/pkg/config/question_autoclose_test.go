package config

import (
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func envMap(m map[string]string) questionAutocloseLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestQuestionAutocloseDefaultsOff(t *testing.T) {
	var q QuestionAutocloseConfig
	if q.isEnabledWith(envMap(nil)) {
		t.Fatal("zero-value config must be disabled")
	}
	if got, want := q.effectiveWindowWith(envMap(nil)), DefaultQuestionAutocloseHours*time.Hour; got != want {
		t.Fatalf("window = %v, want %v", got, want)
	}
	if got := q.EffectiveLabels(); !reflect.DeepEqual(got, []string{"question", "kind/question"}) {
		t.Fatalf("labels = %v", got)
	}
	if got := q.EffectiveHumanLabel(); got != DefaultQuestionAutocloseHumanLabel {
		t.Fatalf("human label = %q", got)
	}
}

func TestQuestionAutocloseEnvOverrides(t *testing.T) {
	on := QuestionAutocloseConfig{Enabled: true, Hours: 2}
	off := QuestionAutocloseConfig{}

	if !off.isEnabledWith(envMap(map[string]string{QuestionAutocloseEnvVar: "true"})) {
		t.Fatal("env true must enable a config-off hive")
	}
	if on.isEnabledWith(envMap(map[string]string{QuestionAutocloseEnvVar: "0"})) {
		t.Fatal("env 0 must disable a config-on hive")
	}
	if !on.isEnabledWith(envMap(map[string]string{QuestionAutocloseEnvVar: "maybe"})) {
		t.Fatal("unparseable env must leave the config value in force")
	}
	if got := on.effectiveWindowWith(envMap(nil)); got != 2*time.Hour {
		t.Fatalf("config hours ignored: %v", got)
	}
	if got := on.effectiveWindowWith(envMap(map[string]string{QuestionAutocloseHoursEnvVar: "9"})); got != 9*time.Hour {
		t.Fatalf("env hours ignored: %v", got)
	}
	if got := on.effectiveWindowWith(envMap(map[string]string{QuestionAutocloseHoursEnvVar: "-3"})); got != 2*time.Hour {
		t.Fatalf("non-positive env hours must be ignored: %v", got)
	}
}

func TestQuestionAutocloseCustomLabels(t *testing.T) {
	q := QuestionAutocloseConfig{Labels: []string{" support ", ""}, HumanLabel: " triage/human "}
	if got := q.EffectiveLabels(); !reflect.DeepEqual(got, []string{"support"}) {
		t.Fatalf("labels = %v", got)
	}
	if got := q.EffectiveHumanLabel(); got != "triage/human" {
		t.Fatalf("human label = %q", got)
	}
	// The defaults are handed out as a copy.
	def := QuestionAutocloseConfig{}.EffectiveLabels()
	def[0] = "mutated"
	if (QuestionAutocloseConfig{}).EffectiveLabels()[0] != "question" {
		t.Fatal("default labels were mutated through a returned slice")
	}
}

func TestQuestionAutocloseYAML(t *testing.T) {
	var g GovernorConfig
	raw := "question_autoclose:\n  enabled: true\n  hours: 6\n  human_label: needs-maintainer\n"
	if err := yaml.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	q := g.QuestionAutoclose
	if !q.Enabled || q.Hours != 6 || q.HumanLabel != "needs-maintainer" {
		t.Fatalf("parsed %+v", q)
	}
}
