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
	if r.Spektacular.HubExecutorEnabled() {
		t.Fatal("hub executor must be off when Spektacular is off")
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
	if !r.Spektacular.HubExecutorEnabled() {
		t.Fatal("hub executor defaults on when Spektacular is enabled")
	}
	if got := r.Spektacular.HubExecutor.MaxConcurrentOrDefault(); got != DefaultSpektacularHubExecutorMaxConcurrent {
		t.Fatalf("MaxConcurrentOrDefault() = %d", got)
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

func TestSpektacularHubExecutorDefaults(t *testing.T) {
	var s SpektacularConfig
	if s.HubExecutorEnabled() {
		t.Fatal("hub executor must be off when Spektacular is disabled")
	}
	s.Enabled = true
	if !s.HubExecutorEnabled() {
		t.Fatal("hub executor should default on when Spektacular is enabled")
	}
	off := false
	s.HubExecutor.Enabled = &off
	if s.HubExecutorEnabled() {
		t.Fatal("explicit enabled=false must disable the hub executor")
	}

	var h SpektacularHubExecutorConfig
	if got := h.BackendOrDefault(""); got != DefaultSpektacularHubExecutorBackend {
		t.Errorf("BackendOrDefault(\"\") = %q, want %q", got, DefaultSpektacularHubExecutorBackend)
	}
	if got := h.BackendOrDefault(" claude "); got != "claude" {
		t.Errorf("BackendOrDefault(fallback) = %q, want claude", got)
	}
	if got := h.IdentityOrDefault(); got != DefaultSpektacularHubExecutorIdentity {
		t.Errorf("IdentityOrDefault() = %q, want %q", got, DefaultSpektacularHubExecutorIdentity)
	}
	if got, want := h.Timeout(), time.Duration(DefaultSpektacularHubExecutorTimeoutSeconds)*time.Second; got != want {
		t.Errorf("Timeout() = %v, want %v", got, want)
	}
	if got := h.MaxConcurrentOrDefault(); got != DefaultSpektacularHubExecutorMaxConcurrent {
		t.Errorf("MaxConcurrentOrDefault() = %d, want %d", got, DefaultSpektacularHubExecutorMaxConcurrent)
	}
}

func TestSpektacularHubExecutorOverrides(t *testing.T) {
	h := SpektacularHubExecutorConfig{
		Backend:        " codex ",
		Identity:       " spek-bot ",
		TimeoutSeconds: 90,
		MaxConcurrent:  3,
	}
	if got := h.BackendOrDefault("claude"); got != "codex" {
		t.Errorf("BackendOrDefault = %q, want codex", got)
	}
	if got := h.IdentityOrDefault(); got != "spek-bot" {
		t.Errorf("IdentityOrDefault = %q, want spek-bot", got)
	}
	if got := h.Timeout(); got != 90*time.Second {
		t.Errorf("Timeout = %v, want 90s", got)
	}
	if got := h.MaxConcurrentOrDefault(); got != 3 {
		t.Errorf("MaxConcurrentOrDefault = %d, want 3", got)
	}
}

func TestSpektacularInterviewModeDefaultsToHuman(t *testing.T) {
	if got := (SpektacularConfig{}).InterviewMode(); got != "human" {
		t.Fatalf("empty InterviewMode() = %q, want human", got)
	}
	if got := (SpektacularConfig{Interview: " auto "}).InterviewMode(); got != "auto" {
		t.Fatalf("auto InterviewMode() = %q, want auto", got)
	}
	if got := (SpektacularConfig{Interview: "surprise"}).InterviewMode(); got != "human" {
		t.Fatalf("unknown InterviewMode() = %q, want human", got)
	}
}
