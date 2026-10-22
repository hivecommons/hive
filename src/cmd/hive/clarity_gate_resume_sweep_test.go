package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The resume sweep is gated on runs.triage.clarity_gate.enabled
// (hivecommons/hive#13588): with the gate off it makes no GitHub call and does
// not stamp its throttle clock.
func TestClarityGateResumeSweepDisabledIsNoOp(t *testing.T) {
	srv := newUnparkGateServer(t)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	client.SetAppBotLogin("hive[bot]")
	cfg := &config.Config{}
	var lastRun time.Time

	runClarityGateResumeSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	runClarityGateResumeSweepIfDue(context.Background(), client, nil, nil, &lastRun, sweepQuietLogger())
	runClarityGateResumeSweepIfDue(context.Background(), nil, cfg, nil, &lastRun, sweepQuietLogger())

	if got := srv.count(); got != 0 {
		t.Fatalf("disabled gate made %d GitHub calls, want 0", got)
	}
	if !lastRun.IsZero() {
		t.Fatal("a sweep that did not run must not consume the throttle clock")
	}
}

// Enabled, it runs at most once per clarityGateResumeSweepInterval.
func TestClarityGateResumeSweepEnabledThrottled(t *testing.T) {
	srv := newUnparkGateServer(t)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	client.SetAppBotLogin("hive[bot]")
	cfg := &config.Config{}
	cfg.Runs.Triage.ClarityGate.Enabled = true
	var lastRun time.Time

	runClarityGateResumeSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	first := srv.count()
	if first == 0 || lastRun.IsZero() {
		t.Fatalf("enabled gate: calls=%d lastRun=%v, want one pass", first, lastRun)
	}
	runClarityGateResumeSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	if got := srv.count(); got != first {
		t.Fatalf("second call inside the interval made %d more calls, want 0", got-first)
	}
}
