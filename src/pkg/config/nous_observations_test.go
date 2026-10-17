package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNousObservationsDefaultOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("nous:\n  dispatcher: inline\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Nous.Observations.Enabled {
		t.Fatal("nous.observations.enabled must default to false")
	}
	if got := cfg.Nous.Observations.ResolvedMaxPerWeek(); got != DefaultNousObservationsMaxPerWeek || DefaultNousObservationsMaxPerWeek != 5 {
		t.Fatalf("ResolvedMaxPerWeek() = %d, want 5", got)
	}
	if err := cfg.Nous.Validate(); err != nil {
		t.Fatalf("default nous config must validate: %v", err)
	}
}

func TestNousObservationsYAML(t *testing.T) {
	var cfg Config
	raw := "nous:\n  observations:\n    enabled: true\n    max_per_week: 3\n"
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.Nous.Observations.Enabled || cfg.Nous.Observations.ResolvedMaxPerWeek() != 3 {
		t.Fatalf("observations = %+v, want enabled with max_per_week 3", cfg.Nous.Observations)
	}
}

func TestNousObservationsValidateMaxPerWeek(t *testing.T) {
	intp := func(v int) *int { return &v }
	for _, tc := range []struct {
		name    string
		max     *int
		wantErr bool
	}{
		{"absent uses default", nil, false},
		{"one", intp(1), false},
		{"five", intp(5), false},
		{"zero rejected", intp(0), true},
		{"negative rejected", intp(-2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NousConfig{Observations: NousObservationsConfig{Enabled: true, MaxPerWeek: tc.max}}.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "max_per_week") {
				t.Fatalf("error does not name the key: %v", err)
			}
		})
	}
}
