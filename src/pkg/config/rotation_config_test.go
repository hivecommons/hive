package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRotationConfig_YAMLRoundTrip(t *testing.T) {
	in := RotationConfig{
		Enabled:            true,
		ThresholdPct:       90,
		HighVolumeCadenceS: 900,
		Providers: map[string]ProviderRotationConfig{
			"anthropic": {Class: "subscription", Backends: []string{"claude", "pi"}},
			"deepseek":  {Class: "metered", Backends: []string{"litellm"}},
		},
		AgentTiers: map[string]string{"worker": "T1", "triage": "T3"},
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out RotationConfig
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestRotationConfig_Defaults(t *testing.T) {
	var r RotationConfig
	if r.Enabled {
		t.Error("Enabled default = true, want false (opt-in)")
	}
	if got := r.EffectiveThreshold(); got != 85 {
		t.Errorf("EffectiveThreshold = %d, want 85", got)
	}
	if got := r.EffectiveHighVolumeCadenceS(); got != 1800 {
		t.Errorf("EffectiveHighVolumeCadenceS = %d, want 1800", got)
	}
	r.ThresholdPct = 70
	r.HighVolumeCadenceS = 600
	if got := r.EffectiveThreshold(); got != 70 {
		t.Errorf("EffectiveThreshold = %d, want 70", got)
	}
	if got := r.EffectiveHighVolumeCadenceS(); got != 600 {
		t.Errorf("EffectiveHighVolumeCadenceS = %d, want 600", got)
	}
}

func TestRotationConfig_YAMLParse(t *testing.T) {
	src := `
enabled: true
threshold_pct: 85
high_volume_cadence_s: 1800
providers:
  anthropic:
    class: subscription
    backends: [claude, pi]
  deepseek:
    class: metered
    backends: [litellm]
agents:
  worker: T1
`
	var r RotationConfig
	if err := yaml.Unmarshal([]byte(src), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Enabled || r.ThresholdPct != 85 || r.HighVolumeCadenceS != 1800 {
		t.Errorf("parsed = %+v", r)
	}
	if got := r.Providers["anthropic"].Class; got != "subscription" {
		t.Errorf("anthropic class = %q", got)
	}
	if got := r.AgentTiers["worker"]; got != "T1" {
		t.Errorf("worker tier = %q", got)
	}
}
