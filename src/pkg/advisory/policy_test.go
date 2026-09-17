package advisory

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

// resetPostGate isolates each test from the package-level gate state.
func resetPostGate() {
	postGate.mu.Lock()
	defer postGate.mu.Unlock()
	postGate.lastSuccess = map[string]time.Time{}
	postGate.clampLogged = false
}

// TestPostDue_UnsetIntervalPostsEveryCycle pins invariant 1 of #4820:
// 0/unset update_interval_s is EXACTLY today's cadence — the gate is open on
// every consecutive cycle, even immediately after a success.
func TestPostDue_UnsetIntervalPostsEveryCycle(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{} // update_interval_s absent
	for i := 0; i < 3; i++ {
		if !PostDue(cfg, "org/repo", now, slog.Default()) {
			t.Fatalf("cycle %d: unset interval must post every cycle", i)
		}
		RecordPostSuccess("org/repo", now)
		now = now.Add(time.Second) // far shorter than any legal interval
	}
}

// TestPostDue_UnsetIntervalIsUnconditional pins that the 0/unset interval
// short-circuits BEFORE the per-repo window is consulted at all, rather than
// merely tending to agree with it. Under a backwards clock — an NTP
// correction, or a hive whose last success was recorded ahead of now — a
// window-based answer would be "not due" and silently suppress the digest.
// Unset means always post, unconditionally.
func TestPostDue_UnsetIntervalIsUnconditional(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{} // update_interval_s absent

	RecordPostSuccess("org/repo", now)
	if !PostDue(cfg, "org/repo", now.Add(-time.Minute), slog.Default()) {
		t.Fatal("unset interval must post even when now precedes the last success")
	}
}

// TestPostDue_ThrottlesUntilIntervalElapses pins the throttle: after a
// successful post, the gate stays closed until the configured interval has
// fully elapsed, then opens.
func TestPostDue_ThrottlesUntilIntervalElapses(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 300}

	if !PostDue(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first post must never be delayed")
	}
	RecordPostSuccess("org/repo", now)

	if PostDue(cfg, "org/repo", now.Add(299*time.Second), slog.Default()) {
		t.Fatal("gate must stay closed inside the configured interval")
	}
	if !PostDue(cfg, "org/repo", now.Add(300*time.Second), slog.Default()) {
		t.Fatal("gate must open once the interval has elapsed")
	}
}

// TestPostDue_FailedAttemptRetriesNextCycle pins the success-only
// advance: the gate moves ONLY via RecordPostSuccess, so a failed post
// attempt is retried on the very next cycle instead of waiting out the
// interval — error recovery (and the hub's staleness signal) stays as prompt
// as before #4820.
func TestPostDue_FailedAttemptRetriesNextCycle(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 3600}

	if !PostDue(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first attempt must be allowed")
	}
	// The attempt FAILED: no RecordPostSuccess. The next cycle must be
	// allowed to retry immediately.
	if !PostDue(cfg, "org/repo", now.Add(time.Minute), slog.Default()) {
		t.Fatal("a failed attempt must not consume the interval window")
	}
}

// TestPostDue_PerRepoIsolation pins that the gate is keyed per repo: a
// primary-repo change (the reinit path) starts with an open gate for the new
// repo instead of inheriting the old repo's window.
func TestPostDue_PerRepoIsolation(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 3600}
	RecordPostSuccess("org/old", now)

	if PostDue(cfg, "org/old", now.Add(time.Minute), slog.Default()) {
		t.Fatal("old repo's window must still be closed")
	}
	if !PostDue(cfg, "org/new", now.Add(time.Minute), slog.Default()) {
		t.Fatal("a different repo must not inherit another repo's window")
	}
}

// TestPostDue_ClampLogsOnce pins the clamp warning contract: an
// out-of-band value is clamped at use time (here: below the minimum) and the
// operator is told exactly once, not once per eval cycle.
func TestPostDue_ClampLogsOnce(t *testing.T) {
	resetPostGate()
	now := time.Now()
	cfg := config.AdvisoryConfig{UpdateIntervalS: 5} // below the 30s floor

	if !PostDue(cfg, "org/repo", now, slog.Default()) {
		t.Fatal("first post must be allowed")
	}
	RecordPostSuccess("org/repo", now)
	// Clamped to 30s, not the raw 5s: at +10s the gate must still be closed.
	if PostDue(cfg, "org/repo", now.Add(10*time.Second), slog.Default()) {
		t.Fatal("a 5s value must be clamped up to the 30s floor, not honored")
	}
	if !PostDue(cfg, "org/repo", now.Add(31*time.Second), slog.Default()) {
		t.Fatal("gate must open after the clamped 30s interval")
	}
	postGate.mu.Lock()
	logged := postGate.clampLogged
	postGate.mu.Unlock()
	if !logged {
		t.Fatal("clamped value must set the one-shot warning flag")
	}
}

// TestPrimaryRepo pins the repo-selection precedence shared by the boot
// ensure, the per-cycle re-ensure, and the post path.
func TestPrimaryRepo(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"no repos at all", &config.Config{}, ""},
		{
			"primary wins over the list",
			func() *config.Config {
				c := &config.Config{}
				c.Project.PrimaryRepo = "org/primary"
				c.Project.Repos = []string{"org/first", "org/second"}
				return c
			}(),
			"org/primary",
		},
		{
			"falls back to the first listed repo",
			func() *config.Config {
				c := &config.Config{}
				c.Project.Repos = []string{"org/first", "org/second"}
				return c
			}(),
			"org/first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PrimaryRepo(tc.cfg); got != tc.want {
				t.Fatalf("PrimaryRepo = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIssueUnresolvedTreatsZeroAsUnresolved pins that a recorded 0 — the zero
// value a failed ensure leaves behind — counts as "no home for the digest".
func TestIssueUnresolvedTreatsZeroAsUnresolved(t *testing.T) {
	issues := map[string]int{"org/zero": 0, "org/ok": 7}

	if !IssueUnresolved(issues, "org/missing") {
		t.Error("an absent repo must be unresolved")
	}
	if !IssueUnresolved(issues, "org/zero") {
		t.Error("a recorded 0 must be unresolved, not a postable issue number")
	}
	if IssueUnresolved(issues, "org/ok") {
		t.Error("a real issue number must be resolved")
	}
	if !IssueUnresolved(nil, "org/ok") {
		t.Error("a nil map must be unresolved")
	}
}

// TestIssueNumber is the read-side twin of the above: the same 0-is-not-a-real
// -issue rule, via the accessor runEvalCycle actually posts with.
func TestIssueNumber(t *testing.T) {
	issues := map[string]int{"org/zero": 0, "org/ok": 42}

	if num, ok := IssueNumber(issues, "org/ok"); !ok || num != 42 {
		t.Errorf("IssueNumber(org/ok) = %d, %v; want 42, true", num, ok)
	}
	if _, ok := IssueNumber(issues, "org/zero"); ok {
		t.Error("IssueNumber(org/zero) reported ok for a recorded 0")
	}
	if _, ok := IssueNumber(issues, "org/missing"); ok {
		t.Error("IssueNumber(org/missing) reported ok for an absent repo")
	}
	if _, ok := IssueNumber(nil, "org/ok"); ok {
		t.Error("IssueNumber(nil map) reported ok")
	}
}

// TestShouldBuildDigest pins that a digest is assembled whenever there are
// bead stores to read, and otherwise only when there is somewhere to post it.
func TestShouldBuildDigest(t *testing.T) {
	if !ShouldBuildDigest(nil, true, true) {
		t.Error("a client plus an existing pinned issue must build (to clear it)")
	}
	if ShouldBuildDigest(nil, true, false) {
		t.Error("no stores and no pinned issue must not build")
	}
	if ShouldBuildDigest(nil, false, true) {
		t.Error("no client must not build even with a pinned issue")
	}
	if !ShouldBuildDigest(map[string]*beads.Store{"scanner": nil}, false, false) {
		t.Error("bead stores alone must build regardless of GitHub state")
	}
}

// TestShouldPostDigest pins that a digest with content always posts, while an
// empty one posts only to clear an existing pinned issue.
func TestShouldPostDigest(t *testing.T) {
	empty := &Digest{}
	withFinding := &Digest{TotalCount: 1}
	withResolved := &Digest{RecentlyResolved: []ResolvedFinding{{Title: "fixed"}}}

	if ShouldPostDigest(nil, true, true) {
		t.Error("a nil digest must never post")
	}
	if !ShouldPostDigest(empty, true, true) {
		t.Error("an empty digest must post to clear an existing pinned issue")
	}
	if ShouldPostDigest(empty, true, false) {
		t.Error("an empty digest with no pinned issue must not post")
	}
	if ShouldPostDigest(empty, false, true) {
		t.Error("an empty digest with no client must not post")
	}
	if !ShouldPostDigest(withFinding, false, false) {
		t.Error("a digest with findings must post regardless of GitHub state")
	}
	if !ShouldPostDigest(withResolved, false, false) {
		t.Error("a digest with resolved findings must post regardless of GitHub state")
	}
}
