package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for runSentinelSweepIfDue: the eval-cycle gate in front of the
// sentinel sweep. Pinned contract: no calls without a client or when
// `sentinel.enabled: false`, default on, and at most one pass per
// sentinelSweepInterval however often the loop calls it.

func TestSentinelSweepGate_NoClientOrDisabledNoCalls(t *testing.T) {
	var lastRun time.Time
	cfg := &config.Config{}

	runSentinelSweepIfDue(context.Background(), nil, cfg, nil, &lastRun, sweepQuietLogger())
	if !lastRun.IsZero() {
		t.Fatal("no client: must not consume the throttle clock")
	}

	srv := newUnparkGateServer(t)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	off := false
	cfg.Sentinel.Enabled = &off
	runSentinelSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	if srv.count() != 0 || !lastRun.IsZero() {
		t.Fatalf("disabled: made %d calls, lastRun=%v", srv.count(), lastRun)
	}

	runSentinelSweepIfDue(context.Background(), client, nil, nil, &lastRun, sweepQuietLogger())
	if srv.count() != 0 {
		t.Fatal("nil config must not sweep")
	}
}

func TestSentinelSweepGate_DefaultOnAndThrottled(t *testing.T) {
	srv := newUnparkGateServer(t)
	client := github.NewClientForTest(srv.URL, "o", []string{"o/r"}, sweepQuietLogger())
	cfg := &config.Config{}
	var lastRun time.Time

	runSentinelSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	first := srv.count()
	if first == 0 {
		t.Fatal("default-on first pass made no API calls, want at least the PR listing")
	}
	if lastRun.IsZero() {
		t.Fatal("first pass did not stamp the throttle clock")
	}

	runSentinelSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	if srv.count() != first {
		t.Fatalf("second pass inside the interval made %d calls, want %d", srv.count(), first)
	}

	lastRun = time.Now().Add(-2 * sentinelSweepInterval)
	runSentinelSweepIfDue(context.Background(), client, cfg, nil, &lastRun, sweepQuietLogger())
	if srv.count() <= first {
		t.Fatal("a pass after the interval elapsed should have run")
	}
}
