package github

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAPIBudgetModeHysteresisAndReset(t *testing.T) {
	now := time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	var log strings.Builder
	c := &Client{logger: slog.New(slog.NewTextHandler(&log, nil))}
	c.rateLimits.now = func() time.Time { return now }
	c.SetAPIBudgetThresholds(800, 250)
	reset := now.Add(time.Hour)
	observe := func(remaining int) APIBudgetMode {
		c.rateLimits.mu.Lock()
		if c.rateLimits.windows == nil {
			c.rateLimits.windows = map[string]rateLimitWindow{}
		}
		c.rateLimits.windows[githubCoreRateLimitResource] = rateLimitWindow{limit: 5000, remaining: remaining, reset: reset, observedAt: now}
		c.rateLimits.mu.Unlock()
		mode, _ := c.APIBudgetMode()
		return mode
	}
	if got := observe(900); got != APIBudgetNormal {
		t.Fatalf("mode at 900 = %v, want normal", got)
	}
	if got := observe(700); got != APIBudgetConserve {
		t.Fatalf("mode at 700 = %v, want conserve", got)
	}
	if got := observe(850); got != APIBudgetConserve {
		t.Fatalf("mode at 850 = %v, want conserve due hysteresis", got)
	}
	if got := observe(920); got != APIBudgetNormal {
		t.Fatalf("mode at 920 = %v, want normal after hysteresis", got)
	}
	if got := observe(200); got != APIBudgetCritical {
		t.Fatalf("mode at 200 = %v, want critical", got)
	}
	if got := observe(320); got != APIBudgetCritical {
		t.Fatalf("mode at 320 = %v, want critical due hysteresis", got)
	}
	now = reset.Add(time.Second)
	if got, snap := c.APIBudgetMode(); got != APIBudgetNormal || snap.Mode != "normal" {
		t.Fatalf("mode after reset = %v/%s, want normal", got, snap.Mode)
	}
	if count := strings.Count(log.String(), "github api budget mode changed"); count != 4 {
		t.Fatalf("transition log count = %d, want 4; log=%s", count, log.String())
	}
}

func TestAPIBudgetSnapshotCarriesSkippedSteps(t *testing.T) {
	c := &Client{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.RecordAPIBudgetSkippedSteps([]string{"review_threads"})
	_, snap := c.APIBudgetMode()
	if len(snap.SkippedSteps) != 1 || snap.SkippedSteps[0] != "review_threads" {
		t.Fatalf("skipped steps = %#v", snap.SkippedSteps)
	}
}

func TestAPIBudgetNilAndUnknownBucketDefaults(t *testing.T) {
	var c *Client
	c.SetAPIBudgetThresholds(1, 1)
	c.RecordAPIBudgetSkippedSteps([]string{"x"})
	if mode, snap := c.APIBudgetMode(); mode != APIBudgetNormal || snap.Mode != "normal" {
		t.Fatalf("nil client mode = %v/%s", mode, snap.Mode)
	}
	live := &Client{}
	live.SetAPIBudgetThresholds(0, 0)
	if mode, snap := live.APIBudgetMode(); mode != APIBudgetNormal || snap.Mode != "normal" || snap.Limit != 0 {
		t.Fatalf("unknown bucket mode = %v snapshot=%+v", mode, snap)
	}
}

func TestAPIBudgetCriticalCanRelaxToConserve(t *testing.T) {
	now := time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC)
	c := &Client{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.rateLimits.now = func() time.Time { return now }
	c.SetAPIBudgetThresholds(800, 250)
	set := func(remaining int) APIBudgetMode {
		c.rateLimits.mu.Lock()
		if c.rateLimits.windows == nil {
			c.rateLimits.windows = map[string]rateLimitWindow{}
		}
		c.rateLimits.windows[githubCoreRateLimitResource] = rateLimitWindow{limit: 5000, remaining: remaining, reset: now.Add(time.Hour), observedAt: now}
		c.rateLimits.mu.Unlock()
		mode, _ := c.APIBudgetMode()
		return mode
	}
	if got := set(100); got != APIBudgetCritical {
		t.Fatalf("mode at 100 = %v", got)
	}
	if got := set(400); got != APIBudgetConserve {
		t.Fatalf("mode at 400 = %v, want conserve", got)
	}
}
