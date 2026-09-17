package advisory

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Before #7238 stage 2 each of these tests began by reaching into a
// package-level gate under its own mutex and zeroing two fields, because every
// test in package main shared one instance. The gate is now a value, so
// isolation is just constructing one -- there is no shared state left to reset
// and no ordering hazard between tests.
//
// advisoryPostGate is per-test, shadowing nothing.

// TestAdvisoryPostDue_UnsetIntervalPostsEveryCycle pins invariant 1 of #4820:
// 0/unset update_interval_s is EXACTLY today's cadence — the gate is open on
// every consecutive cycle, even immediately after a success.
func TestAdvisoryPostDue_UnsetIntervalPostsEveryCycle(t *testing.T) {
	advisoryPostGate := NewPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{} // update_interval_s absent
	for i := 0; i < 3; i++ {
		if !advisoryPostGate.Due(cfg, "org/repo", now, slog.Default()) {
			t.Fatalf("cycle %d: unset interval must post every cycle", i)
		}
		advisoryPostGate.RecordSuccess("org/repo", now)
		now = now.Add(time.Second) // far shorter than any legal interval
	}
}

// TestAdvisoryPostDue_ThrottlesUntilIntervalElapses pins the throttle: after a
// successful post, the gate stays closed until the configured interval has
// fully elapsed, then opens.
func TestAdvisoryPostDue_ThrottlesUntilIntervalElapses(t *testing.T) {
	advisoryPostGate := NewPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 300}

	if !advisoryPostGate.Due(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first post must never be delayed")
	}
	advisoryPostGate.RecordSuccess("org/repo", now)

	if advisoryPostGate.Due(cfg, "org/repo", now.Add(299*time.Second), slog.Default()) {
		t.Fatal("gate must stay closed inside the configured interval")
	}
	if !advisoryPostGate.Due(cfg, "org/repo", now.Add(300*time.Second), slog.Default()) {
		t.Fatal("gate must open once the interval has elapsed")
	}
}

// TestAdvisoryPostDue_FailedAttemptRetriesNextCycle pins the success-only
// advance: the gate moves ONLY via recordAdvisoryPostSuccess, so a failed post
// attempt is retried on the very next cycle instead of waiting out the
// interval — error recovery (and the hub's staleness signal) stays as prompt
// as before #4820.
func TestAdvisoryPostDue_FailedAttemptRetriesNextCycle(t *testing.T) {
	advisoryPostGate := NewPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 3600}

	if !advisoryPostGate.Due(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first attempt must be allowed")
	}
	// The attempt FAILED: no recordAdvisoryPostSuccess. The next cycle must be
	// allowed to retry immediately.
	if !advisoryPostGate.Due(cfg, "org/repo", now.Add(time.Minute), slog.Default()) {
		t.Fatal("a failed attempt must not consume the interval window")
	}
}

// TestAdvisoryPostDue_PerRepoIsolation pins that the gate is keyed per repo: a
// primary-repo change (the reinit path) starts with an open gate for the new
// repo instead of inheriting the old repo's window.
func TestAdvisoryPostDue_PerRepoIsolation(t *testing.T) {
	advisoryPostGate := NewPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 3600}
	advisoryPostGate.RecordSuccess("org/old", now)

	if advisoryPostGate.Due(cfg, "org/old", now.Add(time.Minute), slog.Default()) {
		t.Fatal("old repo's window must still be closed")
	}
	if !advisoryPostGate.Due(cfg, "org/new", now.Add(time.Minute), slog.Default()) {
		t.Fatal("a different repo must not inherit another repo's window")
	}
}

// TestAdvisoryPostDue_ClampLogsOnce pins the clamp warning contract: an
// out-of-band value is clamped at use time (here: below the minimum) and the
// operator is told exactly once, not once per eval cycle.
func TestAdvisoryPostDue_ClampLogsOnce(t *testing.T) {
	advisoryPostGate := NewPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 5} // below the 30s floor

	if !advisoryPostGate.Due(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first post must be allowed")
	}
	advisoryPostGate.RecordSuccess("org/repo", now)
	// Clamped to 30s, not the raw 5s: at +10s the gate must still be closed.
	if advisoryPostGate.Due(cfg, "org/repo", now.Add(10*time.Second), slog.Default()) {
		t.Fatal("a 5s value must be clamped up to the 30s floor, not honored")
	}
	if !advisoryPostGate.Due(cfg, "org/repo", now.Add(31*time.Second), slog.Default()) {
		t.Fatal("gate must open after the clamped 30s interval")
	}
	advisoryPostGate.mu.Lock()
	logged := advisoryPostGate.clampLogged
	advisoryPostGate.mu.Unlock()
	if !logged {
		t.Fatal("clamped value must set the one-shot warning flag")
	}
}
