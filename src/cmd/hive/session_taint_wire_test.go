package main

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux/sessiontaint"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/ioscan"
)

func TestSessionTaintOffByDefaultRecordsNothing(t *testing.T) {
	b := &boot{cfg: &config.Config{}, logger: slog.Default()}
	if b.sessionTaintObserver() != nil {
		t.Fatal("session taint must be off unless session_taint.enabled is set")
	}
	// A canary hit with the feature off is a no-op.
	b.recordCanaryTaint(ioscan.CanaryLeak{Agent: "agent-a", Source: "test"})
	if got := b.sessionTaintObserver().Labels("agent-a"); got != nil {
		t.Errorf("labels = %v, want none", got)
	}
}

func TestSessionTaintCanaryHitSetsSecretLabel(t *testing.T) {
	cfg := &config.Config{}
	cfg.SessionTaint.Enabled = true
	b := &boot{cfg: cfg, logger: slog.Default()}
	obs := b.sessionTaintObserver()
	if obs == nil {
		t.Fatal("expected an observer when enabled")
	}
	if b.sessionTaintObserver() != obs {
		t.Error("the observer must be built once and shared")
	}
	// The token itself is never passed on or stored.
	b.recordCanaryTaint(ioscan.CanaryLeak{Agent: "agent-a", Token: "HIVE-CANARY-synthetic", Source: "hive-comment:o/r"})
	labels := obs.Labels("agent-a")
	if len(labels) != 1 {
		t.Fatalf("labels = %v, want one", labels)
	}
	if l := labels[0]; l.Label != sessiontaint.LabelSecret || l.Origin != canaryTaintOrigin || l.Source != sessiontaint.SourceCanaryHit {
		t.Errorf("label = %+v", l)
	}
}

func TestSessionTaintConfigDefaults(t *testing.T) {
	var c config.SessionTaintConfig
	if c.Enabled {
		t.Error("default must be off")
	}
	if c.MaxSessionsOrDefault() != config.DefaultSessionTaintMaxSessions ||
		c.MaxLabelsPerSessionOrDefault() != config.DefaultSessionTaintMaxLabelsPerSession ||
		c.MaxDecisionsOrDefault() != config.DefaultSessionTaintMaxDecisions {
		t.Error("zero values must resolve to the defaults")
	}
	c = config.SessionTaintConfig{MaxSessions: -1, MaxLabelsPerSession: 5, MaxDecisions: 7}
	if c.MaxSessionsOrDefault() != config.DefaultSessionTaintMaxSessions || c.MaxLabelsPerSessionOrDefault() != 5 || c.MaxDecisionsOrDefault() != 7 {
		t.Error("explicit positive values must be kept, non-positive ones defaulted")
	}
}

// The config defaults and the observer's own bounds are the same numbers, so
// `enabled: true` alone gives the documented limits.
func TestSessionTaintConfigDefaultsMatchObserver(t *testing.T) {
	if config.DefaultSessionTaintMaxSessions != sessiontaint.DefaultMaxSessions ||
		config.DefaultSessionTaintMaxLabelsPerSession != sessiontaint.DefaultMaxLabelsPerSession ||
		config.DefaultSessionTaintMaxDecisions != sessiontaint.DefaultMaxDecisions {
		t.Fatal("pkg/config defaults drifted from crux/sessiontaint defaults")
	}
}
