package config

import (
	"testing"
	"time"
)

func TestRunsConfigDefaults(t *testing.T) {
	var r RunsConfig
	if r.Spektacular.Enabled {
		t.Fatal("spektacular runner must be off by default")
	}
	if got := r.MaxStageRetriesOrDefault(); got != DefaultMaxStageRetries {
		t.Fatalf("MaxStageRetriesOrDefault() = %d, want %d", got, DefaultMaxStageRetries)
	}
	if got := r.Spektacular.BinaryOrDefault(); got != DefaultSpektacularBinary {
		t.Fatalf("BinaryOrDefault() = %q, want %q", got, DefaultSpektacularBinary)
	}
	if got := r.Spektacular.PollInterval(); got != time.Duration(DefaultSpektacularPollS)*time.Second {
		t.Fatalf("PollInterval() = %s, want %ds", got, DefaultSpektacularPollS)
	}
}

func TestRunsConfigOverrides(t *testing.T) {
	r := RunsConfig{MaxStageRetries: 5, Spektacular: SpektacularConfig{Enabled: true, Binary: "  /opt/bin/spektacular ", PollIntervalS: 7}}
	if got := r.MaxStageRetriesOrDefault(); got != 5 {
		t.Fatalf("MaxStageRetriesOrDefault() = %d, want 5", got)
	}
	if got := r.Spektacular.BinaryOrDefault(); got != "/opt/bin/spektacular" {
		t.Fatalf("BinaryOrDefault() = %q", got)
	}
	if got := r.Spektacular.PollInterval(); got != 7*time.Second {
		t.Fatalf("PollInterval() = %s, want 7s", got)
	}
	neg := RunsConfig{MaxStageRetries: -1, Spektacular: SpektacularConfig{PollIntervalS: -3}}
	if neg.MaxStageRetriesOrDefault() != DefaultMaxStageRetries || neg.Spektacular.PollInterval() != time.Duration(DefaultSpektacularPollS)*time.Second {
		t.Fatal("negative values must fall back to defaults")
	}
}

func TestTriageConfigDefaultsAndOverrides(t *testing.T) {
	var cfg TriageConfig
	if cfg.Enabled {
		t.Fatal("triage must be off by default")
	}
	if got := cfg.EffectiveMinBodyChars(); got != DefaultTriageMinBodyChars {
		t.Fatalf("min body = %d, want %d", got, DefaultTriageMinBodyChars)
	}
	if !cfg.ShouldClarifyComment() {
		t.Fatal("clarify comments default on once triage is enabled")
	}
	if got := cfg.EffectiveSpecLabels(); len(got) == 0 || got[0] != "kind/feature" {
		t.Fatalf("default spec labels = %v", got)
	}
	noComment := false
	cfg = TriageConfig{SpecLabels: []string{"design"}, FixLabels: []string{"bug"}, MinBodyChars: 12, ClarifyComment: &noComment}
	if cfg.EffectiveMinBodyChars() != 12 || cfg.ShouldClarifyComment() || cfg.EffectiveSpecLabels()[0] != "design" || cfg.EffectiveFixLabels()[0] != "bug" {
		t.Fatalf("overrides not honored: %+v", cfg)
	}
}
