package hub

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas.go — pollLatestSHAsTick (the StartLatestSHAPoller ticker-loop body)
//
// StartLatestSHAPoller's loop blocks on a 2-minute ticker, so
// TestStartLatestSHAPollerPreLoop (sha_poller_coverage_test.go) never runs the
// tick body. pollLatestSHAsTick is the extracted body, callable directly here
// against the same fake GitHub/GHCR fixture, with no ticker and no sleeps.
// ============================================================

// newTickTestServer builds a minimal HubServer suitable for pollLatestSHAsTick:
// a registered hive (so trackedBranchList() has something to fetch) and a hub
// git identity distinct from the fake's advertised SHA (so the hub
// auto-upgrade block below has a real behind/ahead decision to make).
func newTickTestServer() *HubServer {
	s := &HubServer{
		logger:       slog.Default(),
		saveCh:       make(chan struct{}, 1),
		hubGitBranch: "v2",
		hubGitHash:   "oldhubsha",
	}
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}
	return s
}

// (a) a changed SHA is persisted; an unchanged SHA is not re-persisted.
func TestPollLatestSHAsTick_PersistsOnlyOnChange(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)

	dir := t.TempDir()
	oldPath := latestSHAsPath
	latestSHAsPath = dir + "/latest-shas.json"
	defer func() { latestSHAsPath = oldPath }()

	sha := "abcdef1234567890"
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK) // image on GHCR
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"` + sha + `","commit":{"message":"m"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	resetSHACaches(t)

	s := newTickTestServer()
	ctx := context.Background()

	// First tick: v2 has no cached SHA yet, so this fetch is a change and must
	// persist.
	beforeFirstTick := snapshotBranchSHAs()
	if info, ok := beforeFirstTick["v2"]; ok {
		t.Fatalf("v2 SHA cache was populated before first tick after resetSHACaches: %+v (full snapshot=%+v)", info, beforeFirstTick)
	}
	s.pollLatestSHAsTick(ctx, time.Now())
	afterFirstTick := snapshotBranchSHAs()
	if maps.Equal(afterFirstTick, beforeFirstTick) {
		t.Fatalf("first tick did not change SHA cache: before=%+v after=%+v", beforeFirstTick, afterFirstTick)
	}
	data, err := os.ReadFile(latestSHAsPath)
	if err != nil {
		t.Fatalf("expected persisted file after first tick (changed SHA), got error: %v", err)
	}
	firstWrite := string(data)
	if !strings.Contains(firstWrite, shortSHA(sha)) {
		t.Fatalf("persisted file missing new SHA: %s", firstWrite)
	}

	// Remove the file and tick again with the SAME fake (unchanged upstream
	// SHA). Positive control: persistLatestSHAs must NOT run this time, so the
	// file must NOT reappear.
	if err := os.Remove(latestSHAsPath); err != nil {
		t.Fatalf("remove persisted file: %v", err)
	}
	s.pollLatestSHAsTick(ctx, time.Now())
	if _, err := os.Stat(latestSHAsPath); err == nil {
		t.Fatalf("persisted file reappeared after an unchanged-SHA tick — should not have been re-persisted")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking persisted file: %v", err)
	}
}

// (b) the throttled reconciliation lanes are invoked each tick. Every *IfDue
// lane records its own s.last<Lane> timestamp (guarded by
// s.clusterUnreachableMu) before doing its throttled work, and each starts at
// its zero value on a fresh HubServer — so a single tick against a due (zero)
// timestamp is the lane's own observable proof it ran. No new test-only hook
// is needed for these.
func TestPollLatestSHAsTick_InvokesThrottledLanes(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)

	dir := t.TempDir()
	oldPath := latestSHAsPath
	latestSHAsPath = dir + "/latest-shas.json"
	defer func() { latestSHAsPath = oldPath }()

	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"abcdef1234567890","commit":{"message":"m"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})

	s := newTickTestServer()

	before := time.Now()
	s.pollLatestSHAsTick(context.Background(), before)

	s.clusterUnreachableMu.Lock()
	lastTimestamps := map[string]time.Time{
		"sweepOrphanedUpgradesIfDue (lastOrphanedUpgradeSweep)": s.lastOrphanedUpgradeSweep,
		"sweepStuckAssignmentsIfDue (lastStuckAssignmentSweep)": s.lastStuckAssignmentSweep,
		"reconcileNetAdminIfDue (lastNetAdminReconcile)":        s.lastNetAdminReconcile,
		"reconcilePerHiveEnvIfDue (lastPerHiveEnvReconcile)":    s.lastPerHiveEnvReconcile,
		"reapOrphanedPodsIfDue (lastOrphanedPodReap)":           s.lastOrphanedPodReap,
		"retireExpiredGenerationsIfDue (lastGenerationRetire)":  s.lastGenerationRetire,
		"sweepExpiredAccessIfDue (lastAccessExpirySweep)":       s.lastAccessExpirySweep,
		"replenishPoolsIfDue (lastPoolReplenish)":               s.lastPoolReplenish,
	}
	s.clusterUnreachableMu.Unlock()

	for lane, ts := range lastTimestamps {
		if ts.IsZero() {
			t.Errorf("%s: throttle timestamp still zero after a tick — lane was not invoked", lane)
			continue
		}
		if ts.Before(before) {
			t.Errorf("%s: throttle timestamp %v predates the tick (%v) — stale value, lane was not invoked this tick", lane, ts, before)
		}
	}
}

// (c) the hub auto-upgrade check runs every tick, not only when the SHA just
// changed — the regression the code comment documents. We drive this with
// TWO ticks against an UNCHANGED upstream SHA (so `changed` is false on both,
// proving the check is not gated on it) and assert the auto-upgrade decision
// is (re)made on the second tick too, not just the first.
//
// hubImageExists is stubbed (existing package-level test seam, also used by
// saas_edge_coverage_test.go and hub_upgrade_state_test.go) so the assertion
// exercises the decision logic without shelling out to kubectl. Counting its
// calls is the observable: rolloutHubToSHA only calls it once the hub
// auto-upgrade branch has decided hubBranchSHA is set, not the current hash,
// and not debounced — i.e. once the every-tick check actually ran and found
// work to do.
func TestPollLatestSHAsTick_HubAutoUpgradeCheckedEveryTick(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)

	dir := t.TempDir()
	oldPath := latestSHAsPath
	latestSHAsPath = dir + "/latest-shas.json"
	defer func() { latestSHAsPath = oldPath }()

	// Upstream v2 never moves across the two ticks below — this is the
	// "SHA did not just change" condition the regression comment describes.
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"0123456789abcdef0123456789abcdef01234567","commit":{"message":"m"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})

	setAutoUpgrade(t, true)

	imageCheckCalls := 0
	oldImgCheck := hubImageExists
	hubImageExists = func(sha string, _ *slog.Logger) bool {
		imageCheckCalls++
		return false // decline the rollout itself; only the DECISION is under test
	}
	t.Cleanup(func() { hubImageExists = oldImgCheck })

	s := newTickTestServer()
	s.hubGitBranch = "v2"
	s.hubGitHash = "oldhubsha" // behind the fake's advertised hub SHA the whole time

	// First tick: resolves and caches the hub SHA, and the hub is behind it —
	// the auto-upgrade branch reaches rolloutHubToSHA, which calls
	// hubImageExists as its first real step.
	s.pollLatestSHAsTick(context.Background(), time.Now())
	if imageCheckCalls == 0 {
		t.Fatalf("hub auto-upgrade check did not run on tick 1 (hub behind, auto-upgrade on)")
	}
	firstTickCalls := imageCheckCalls

	// Second tick: the upstream SHA is UNCHANGED from tick 1 (branch API keeps
	// returning the same commit, so fetchAllBranchSHAs finds nothing new and
	// `changed` is false). Before the fix this block lived inside `if changed`,
	// so it would be skipped here. It must still run.
	s.pollLatestSHAsTick(context.Background(), time.Now())
	if imageCheckCalls <= firstTickCalls {
		t.Fatalf("hub auto-upgrade check did not run on tick 2 (SHA unchanged from tick 1) — "+
			"regression: the check must run every tick, not only when the SHA just changed; "+
			"calls after tick1=%d, after tick2=%d", firstTickCalls, imageCheckCalls)
	}
}
