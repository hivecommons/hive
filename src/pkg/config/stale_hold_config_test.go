package config

import (
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func staleHoldEnv(v string) func(string) string {
	return func(key string) string {
		if key == StaleHoldAfterEnv {
			return v
		}
		return ""
	}
}

func TestEffectiveStaleHoldAfter(t *testing.T) {
	for _, tc := range []struct {
		name, env, cfg string
		want           time.Duration
	}{
		{"unset", "", "", DefaultStaleHoldAfter},
		{"config", "", "90m", 90 * time.Minute},
		{"env wins", "30m", "90m", 30 * time.Minute},
		{"env off", "off", "90m", 0},
		{"config off", "", "OFF", 0},
		{"config zero", "", "0", 0},
		{"zero duration", "", "0s", 0},
		{"bad env falls back to config", "soon", "90m", 90 * time.Minute},
		{"negative falls back to default", "", "-1h", DefaultStaleHoldAfter},
		{"blank env ignored", "   ", "3h", 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (EscalationConfig{StaleHoldAfter: tc.cfg}).EffectiveStaleHoldAfter(staleHoldEnv(tc.env))
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	t.Setenv(StaleHoldAfterEnv, "45m")
	if got := (EscalationConfig{}).EffectiveStaleHoldAfter(nil); got != 45*time.Minute {
		t.Fatalf("nil getenv must read the process env: got %v", got)
	}
}

func TestEffectiveStaleHoldLabels(t *testing.T) {
	got := (EscalationConfig{}).EffectiveStaleHoldLabels()
	if !slices.Equal(got, DefaultStaleHoldLabels) {
		t.Fatalf("unset: %v", got)
	}
	got[0] = "mutated"
	if DefaultStaleHoldLabels[0] == "mutated" {
		t.Fatal("caller mutation leaked into the defaults")
	}
	if got := (EscalationConfig{StaleHoldLabels: []string{" ", ""}}).EffectiveStaleHoldLabels(); !slices.Equal(got, DefaultStaleHoldLabels) {
		t.Fatalf("blank: %v", got)
	}
	if got := (EscalationConfig{StaleHoldLabels: []string{" paused ", "", "wait"}}).EffectiveStaleHoldLabels(); !slices.Equal(got, []string{"paused", "wait"}) {
		t.Fatalf("set: %v", got)
	}
}

func TestEffectiveReleaseWhenGreenLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":             DefaultReleaseWhenGreenLabel,
		"  ":           DefaultReleaseWhenGreenLabel,
		"off":          "",
		"Off":          "",
		" release-me ": "release-me",
	} {
		if got := (EscalationConfig{ReleaseWhenGreenLabel: in}).EffectiveReleaseWhenGreenLabel(); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestStaleHoldConfigYAMLKeys(t *testing.T) {
	var cfg Config
	src := "escalation:\n  stale_hold_after: 3h\n  stale_hold_labels: [paused]\n  release_when_green_label: release-me\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	e := cfg.Escalation
	if e.StaleHoldAfter != "3h" || !slices.Equal(e.StaleHoldLabels, []string{"paused"}) || e.ReleaseWhenGreenLabel != "release-me" {
		t.Fatalf("decoded %+v", e)
	}
}
