package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The per-user PR ring (#7894): the same delta-of-cumulative rollup as
// per_user_done, read from TasksWithPR, so the "Your contribution" card can put
// a 24-hour figure next to the all-time PR count. These tests pin the ring's
// bucketing, its trailing-window sum, its persistence, and the one place it
// deliberately differs from the completion ring — it is tail-aligned, never
// left-padded, so its length is the number of hours actually measured.

// tick is a rollup carrying both cumulative counters for one contributor.
func tick(m *metricsStore, user string, done, withPR int) {
	m.rollup(rollupSample{
		queueDepth:   1,
		fleetSize:    1,
		userTotals:   map[string]int{user: done},
		userPRTotals: map[string]int{user: withPR},
		now:          time.Now(),
	})
}

// TestPRRingBucketsWithPRDeltas proves each hour's PR bucket is the delta of the
// cumulative TasksWithPR counter — not the completion delta, and not the
// cumulative total — and that the first tick seeds a baseline instead of booking
// the whole history as one hour's pull requests.
func TestPRRingBucketsWithPRDeltas(t *testing.T) {
	m := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	tick(m, "w", 100, 40) // seed: baseline only
	tick(m, "w", 103, 41) // 3 completions, 1 of them with a PR
	tick(m, "w", 105, 41) // 2 completions, none with a PR
	tick(m, "w", 105, 39) // counter went DOWN (profile reset): zero, not -2

	snap := m.snapshot()
	if got := snap.PerUserPR["w"]; len(got) != 4 || got[0] != 0 || got[1] != 1 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("per_user_pr[w] = %v, want [0 1 0 0]", got)
	}
	if got := snap.PerUserDone["w"]; len(got) != 4 || got[1] != 3 || got[2] != 2 {
		t.Fatalf("per_user_done[w] = %v, want completions [0 3 2 0] — the PR ring must not replace it", got)
	}
}

// TestUserRecentPRSumsTrailingWindow proves the trailing-window sum reads the PR
// ring, with the same known/covered contract as userRecent.
func TestUserRecentPRSumsTrailingWindow(t *testing.T) {
	m := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	// 30 ticks: after the seed, every hour completes 2 tasks and ships 1 PR.
	for i := 0; i < 30; i++ {
		tick(m, "w", i*2, i)
	}
	sum, covered, known := m.userRecentPR("w", recentWindowBuckets)
	if !known || covered != recentWindowBuckets || sum != recentWindowBuckets {
		t.Fatalf("userRecentPR = sum %d / covered %d / known %v, want %d/%d/true",
			sum, covered, known, recentWindowBuckets, recentWindowBuckets)
	}
	// The completion figure over the same window is twice that: the two rings
	// answer different questions.
	if done, _, _ := m.userRecent("w", recentWindowBuckets); done != 2*recentWindowBuckets {
		t.Fatalf("userRecent = %d, want %d", done, 2*recentWindowBuckets)
	}
	if _, _, known := m.userRecentPR("nobody", recentWindowBuckets); known {
		t.Fatal("known = true for a contributor with no PR series at all")
	}
	if _, _, known := m.userRecentPR("", recentWindowBuckets); known {
		t.Fatal("known = true for an empty username")
	}
	if sum, covered, _ := m.userRecentPR("w", 0); sum != 0 || covered != 0 {
		t.Fatalf("a zero-width window must sum nothing, got %d/%d", sum, covered)
	}
}

// TestPRRingSurvivesRestart proves the PR ring is persisted and restored, so a
// deploy mid-day does not reset "PRs produced (24h)" to a dash.
func TestPRRingSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())
	for i := 0; i < 6; i++ {
		tick(m, "w", i, i)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["per_user_pr"]; !ok {
		t.Fatal("persisted metrics missing key per_user_pr")
	}

	m2 := newMetricsStore(path, slog.Default())
	m2.load()
	sum, covered, known := m2.userRecentPR("w", recentWindowBuckets)
	if !known || covered != 6 || sum != 5 {
		t.Fatalf("after restart userRecentPR = %d/%d/%v, want 5/6/true", sum, covered, known)
	}
}

// TestPRRingAbsentFromLegacyFileIsUnknownNotZero proves a metrics file written
// before the PR ring existed yields NO PR series — "not measured yet" — rather
// than a full timeline of zeros that would read as "no pull requests all day".
func TestPRRingAbsentFromLegacyFileIsUnknownNotZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	legacy := map[string]any{
		"queue_depth":   []int{1, 1, 1},
		"tasks_done":    []int{0, 2, 1},
		"fleet_size":    []int{1, 1, 1},
		"per_user_done": map[string][]int{"w": {0, 2, 1}},
		"bucket":        "hour",
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newMetricsStore(path, slog.Default())
	m.load()
	if _, _, known := m.userRecent("w", recentWindowBuckets); !known {
		t.Fatal("completion series lost on load — the legacy file had one")
	}
	if _, _, known := m.userRecentPR("w", recentWindowBuckets); known {
		t.Fatal("PR series reported known from a file that never measured it")
	}
}

// TestPRRingIsTailAlignedNotPadded proves the deliberate difference from the
// completion ring: after an upgrade (a store with a day of completion history
// and no PR ring), the first PR rollup starts a ONE-bucket ring, so the
// coverage the card reports is the hour actually measured — not a full day of
// zeros nobody measured. The tail still lines up with the timeline: one bucket
// per tick, newest last.
func TestPRRingIsTailAlignedNotPadded(t *testing.T) {
	m := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	// A day of pre-upgrade history: completion buckets only.
	for i := 0; i < 25; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"w": i}, now: time.Now()})
	}
	// Upgrade: a restart re-seeds baselines, then the rollup starts sampling PRs.
	m.seededTotals = false
	tick(m, "w", 25, 10) // seed
	tick(m, "w", 26, 11) // first measured PR hour
	tick(m, "w", 27, 11)

	snap := m.snapshot()
	if got := len(snap.PerUserDone["w"]); got != 28 {
		t.Fatalf("per_user_done[w] len = %d, want 28 (index-aligned with the timeline)", got)
	}
	if got := snap.PerUserPR["w"]; len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("per_user_pr[w] = %v, want the three measured hours [0 1 0], not a padded timeline", got)
	}
	sum, covered, known := m.userRecentPR("w", recentWindowBuckets)
	if !known || covered != 3 || sum != 1 {
		t.Fatalf("userRecentPR = %d/%d/%v, want 1/3/true — coverage is the measured depth", sum, covered, known)
	}
	if _, covered, _ := m.userRecent("w", recentWindowBuckets); covered != recentWindowBuckets {
		t.Fatalf("completion coverage = %d, want a full %d", covered, recentWindowBuckets)
	}
}

// TestPRRingIdleAndDepartedContributors mirrors the completion ring's pruning
// rules: a registered contributor with no PR this hour gets an explicit zero so
// the ring keeps pace with the timeline, and a departed contributor whose ring
// is all zeros is dropped rather than kept forever.
func TestPRRingIdleAndDepartedContributors(t *testing.T) {
	m := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	both := func(a, aPR, b, bPR int) {
		m.rollup(rollupSample{
			userTotals:   map[string]int{"a": a, "b": b},
			userPRTotals: map[string]int{"a": aPR, "b": bPR},
			now:          time.Now(),
		})
	}
	both(0, 0, 0, 0) // seed
	both(1, 1, 1, 0) // a ships a PR; b completes without one
	both(2, 1, 2, 0) // neither ships a PR

	// b never shipped: still registered, so the ring is a truthful flat line.
	if got := m.snapshot().PerUserPR["b"]; len(got) != 3 || !allZero(got) {
		t.Fatalf("per_user_pr[b] = %v, want three explicit zeros", got)
	}

	// b's profile disappears: an all-zero ring carries nothing and is dropped;
	// a's ring, which holds a real PR, keeps pace with a zero.
	m.rollup(rollupSample{
		userTotals:   map[string]int{"a": 2},
		userPRTotals: map[string]int{"a": 1},
		now:          time.Now(),
	})
	snap := m.snapshot()
	if _, ok := snap.PerUserPR["b"]; ok {
		t.Fatalf("per_user_pr[b] = %v, want the empty departed ring dropped", snap.PerUserPR["b"])
	}
	if got := snap.PerUserPR["a"]; len(got) != 4 || got[1] != 1 || got[3] != 0 {
		t.Fatalf("per_user_pr[a] = %v, want [0 1 0 0]", got)
	}
}

// TestPRRingRespectsRetentionCap proves the second ring cannot grow the
// persisted file without bound any more than the first one can.
func TestPRRingRespectsRetentionCap(t *testing.T) {
	m := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())
	for i := 0; i < metricsRetentionBuckets+20; i++ {
		tick(m, "w", i, i)
	}
	if got := len(m.snapshot().PerUserPR["w"]); got != metricsRetentionBuckets {
		t.Fatalf("per_user_pr[w] len = %d, want cap %d", got, metricsRetentionBuckets)
	}
}

// TestPRRingNotServedBySparklineEndpoint pins that /api/contribute/metrics keeps
// its payload: the PR ring is persisted for the card's 24h figure, not shipped
// to every viewer on every poll for a series nothing draws.
func TestPRRingNotServedBySparklineEndpoint(t *testing.T) {
	s := meStatsServer(t)
	store := s.contributeMetricsStore()
	tick(store, "w", 0, 0)
	tick(store, "w", 1, 1)

	rec := getAs(s, "/api/contribute/metrics", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("metrics payload not JSON: %v", err)
	}
	if _, ok := raw["per_user_pr"]; ok {
		t.Fatal("per_user_pr leaked into the sparkline payload")
	}
	if _, ok := raw["per_user_done"]; !ok {
		t.Fatal("per_user_done missing from the sparkline payload")
	}
}
