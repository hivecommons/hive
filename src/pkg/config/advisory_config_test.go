package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestAdvisoryConfigDefaults pins the shipped advisory behavior for a hive that
// says nothing: a ten-finding digest, a week-long staleness window, and
// PR-linked auto-close on.
func TestAdvisoryConfigDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	a := cfg.Governor.Advisory
	if a.MaxFindings != defaultAdvisoryMaxFindings {
		t.Errorf("MaxFindings = %d, want %d", a.MaxFindings, defaultAdvisoryMaxFindings)
	}
	if a.StalenessDays != defaultAdvisoryStalenessDays {
		t.Errorf("StalenessDays = %d, want %d", a.StalenessDays, defaultAdvisoryStalenessDays)
	}
	if a.PRAutoClose == nil || !*a.PRAutoClose {
		t.Errorf("PRAutoClose = %v, want an explicit true", a.PRAutoClose)
	}
	if !a.PRAutoCloseEnabled() {
		t.Error("PRAutoCloseEnabled() = false, want true by default")
	}
	if a.ShowAll {
		t.Error("ShowAll = true, want false — the cap is the default, opting out of it is not")
	}
}

// TestAdvisoryConfigDefaultsPreserveExplicitValues confirms defaulting never
// overwrites an operator's choice, including an explicit pr_autoclose: false
// (the case a plain bool would silently lose).
func TestAdvisoryConfigDefaultsPreserveExplicitValues(t *testing.T) {
	off := false
	cfg := &Config{}
	cfg.Governor.Advisory = AdvisoryConfig{
		MaxFindings:   25,
		ShowAll:       true,
		StalenessDays: 3,
		PRAutoClose:   &off,
	}
	cfg.applyDefaults()

	a := cfg.Governor.Advisory
	if a.MaxFindings != 25 {
		t.Errorf("MaxFindings = %d, want the configured 25", a.MaxFindings)
	}
	if !a.ShowAll {
		t.Error("ShowAll was reset to false")
	}
	if a.StalenessDays != 3 {
		t.Errorf("StalenessDays = %d, want the configured 3", a.StalenessDays)
	}
	if a.PRAutoClose == nil || *a.PRAutoClose {
		t.Errorf("PRAutoClose = %v, want the configured false", a.PRAutoClose)
	}
	if a.PRAutoCloseEnabled() {
		t.Error("PRAutoCloseEnabled() = true, want false when explicitly disabled")
	}
}

// TestAdvisoryUpdateInterval pins update_interval_s resolution (#4820):
// 0/absent/negative mean "post every eval cycle" — exactly the pre-#4820
// behavior, so existing installs see no change — and set values clamp into
// [MinAdvisoryUpdateIntervalS, MaxAdvisoryUpdateIntervalS] so a typo can
// neither disable the throttle silently nor slow a hive past the hub's
// wedged-digest threshold.
func TestAdvisoryUpdateInterval(t *testing.T) {
	cases := []struct {
		name string
		raw  int
		want time.Duration
	}{
		{"zero is unset — every cycle", 0, 0},
		{"negative is unset, not clamped up", -5, 0},
		{"below minimum clamps up", 10, MinAdvisoryUpdateIntervalS * time.Second},
		{"minimum passes through", MinAdvisoryUpdateIntervalS, MinAdvisoryUpdateIntervalS * time.Second},
		{"typical value passes through", 300, 300 * time.Second},
		{"maximum passes through", MaxAdvisoryUpdateIntervalS, MaxAdvisoryUpdateIntervalS * time.Second},
		{"above maximum clamps down", 86400, MaxAdvisoryUpdateIntervalS * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := AdvisoryConfig{UpdateIntervalS: tc.raw}
			if got := a.UpdateInterval(); got != tc.want {
				t.Fatalf("UpdateInterval(%d) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestAdvisoryUpdateIntervalRoundTrip pins that hive.yaml stores the field as
// written (no load-time rewrite): clamping happens at USE time via
// UpdateInterval, so an operator's file round-trips byte-for-byte and applying
// defaults never mutates the raw value.
func TestAdvisoryUpdateIntervalRoundTrip(t *testing.T) {
	cfg := &Config{}
	cfg.Governor.Advisory.UpdateIntervalS = 300
	cfg.applyDefaults()
	if cfg.Governor.Advisory.UpdateIntervalS != 300 {
		t.Fatalf("UpdateIntervalS = %d after defaults, want the configured 300", cfg.Governor.Advisory.UpdateIntervalS)
	}

	out, err := yaml.Marshal(cfg.Governor.Advisory)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "update_interval_s: 300") {
		t.Fatalf("yaml round-trip lost update_interval_s: %s", out)
	}
	var back AdvisoryConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.UpdateIntervalS != 300 {
		t.Fatalf("round-tripped UpdateIntervalS = %d, want 300", back.UpdateIntervalS)
	}

	// Unset stays absent from the yaml entirely (omitempty), so untouched
	// installs never see the key appear in their file.
	out, err = yaml.Marshal(AdvisoryConfig{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(out), "update_interval_s") {
		t.Fatalf("unset update_interval_s must be omitted from yaml, got: %s", out)
	}
}

// TestAdvisoryConfigTargetDefaultsToGitHub pins the zero-behavior-change
// contract for governor.advisory.target: an untouched hive resolves to the
// GitHub advisory issue, the Linear keys stay empty, and neither key leaks
// into a re-marshalled hive.yaml.
func TestAdvisoryConfigTargetDefaultsToGitHub(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	a := cfg.Governor.Advisory
	if a.Target != "" {
		t.Errorf("Target = %q, want empty (unset) so hive.yaml round-trips untouched", a.Target)
	}
	if got := a.ResolvedTarget(); got != AdvisoryTargetGitHub {
		t.Errorf("ResolvedTarget() = %q, want %q", got, AdvisoryTargetGitHub)
	}
	if a.LinearIssue != "" {
		t.Errorf("LinearIssue = %q, want empty", a.LinearIssue)
	}
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "target") || strings.Contains(string(out), "linear_issue") {
		t.Fatalf("unset target/linear_issue must be omitted from yaml, got: %s", out)
	}
}

// TestAdvisoryConfigTargetParsesLinear confirms the two new keys parse from
// YAML and that ResolvedTarget normalizes case/whitespace without inventing a
// value for anything it does not recognize.
func TestAdvisoryConfigTargetParsesLinear(t *testing.T) {
	var cfg Config
	src := "governor:\n  advisory:\n    target: Linear\n    linear_issue: ONB-123\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.applyDefaults()
	a := cfg.Governor.Advisory
	if a.Target != "Linear" || a.LinearIssue != "ONB-123" {
		t.Fatalf("parsed = {%q %q}, want {Linear ONB-123}", a.Target, a.LinearIssue)
	}
	if got := a.ResolvedTarget(); got != AdvisoryTargetLinear {
		t.Errorf("ResolvedTarget() = %q, want %q", got, AdvisoryTargetLinear)
	}
	if got := (AdvisoryConfig{Target: " jira "}).ResolvedTarget(); got != "jira" {
		t.Errorf("unknown target resolved to %q, want it passed through for the caller to reject", got)
	}
}

// TestAdvisoryQueueHealthRoundTrip (#9103): a tuned threshold survives the
// yaml round-trip and applyDefaults leaves it alone; an untouched install
// never sees the queue_health key appear in its file.
func TestAdvisoryQueueHealthRoundTrip(t *testing.T) {
	cfg := &Config{}
	cfg.Governor.Advisory.QueueHealth.BlockedPRPct = 60
	cfg.applyDefaults()
	out, err := yaml.Marshal(cfg.Governor.Advisory)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "queue_health:\n") || !strings.Contains(string(out), "blocked_pr_pct: 60") || strings.Contains(string(out), "lane_share_pct") {
		t.Fatalf("yaml round-trip of queue_health wrong: %s", out)
	}
	var back AdvisoryConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.QueueHealth != (AdvisoryQueueHealthConfig{BlockedPRPct: 60}) {
		t.Fatalf("round-tripped queue_health = %+v", back.QueueHealth)
	}

	empty := &Config{}
	empty.applyDefaults()
	out, err = yaml.Marshal(empty.Governor.Advisory)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(out), "queue_health") {
		t.Fatalf("unset queue_health must be omitted from yaml, got: %s", out)
	}
}
