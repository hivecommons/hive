package hub

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// commit_order.go — ancestry cache + the vllmd-13 stale-target wedge
// ============================================================

// resetCommitOrderState clears the ancestry cache/in-flight maps and restores
// the suite-default compare fetcher (the offline TestMain stub — the real
// network fetcher is never active during tests) after the test. All fetcher
// writes happen under
// commitOrderMu — the mutex commitAtOrAheadOfTarget captures the var under —
// so a background resolve leaked from another test can never race them.
func resetCommitOrderState(t *testing.T) {
	t.Helper()
	commitOrderMu.Lock()
	origFetch := fetchCommitCompareStatus
	commitOrderCache = map[commitOrderKey]bool{}
	commitOrderInFlight = map[commitOrderKey]bool{}
	commitOrderMu.Unlock()
	t.Cleanup(func() {
		commitOrderMu.Lock()
		fetchCommitCompareStatus = origFetch
		commitOrderCache = map[commitOrderKey]bool{}
		commitOrderInFlight = map[commitOrderKey]bool{}
		commitOrderMu.Unlock()
	})
}

// stubCommitCompare swaps the compare fetcher under commitOrderMu.
func stubCommitCompare(fn func(base, head string, logger *slog.Logger) (string, error)) {
	commitOrderMu.Lock()
	fetchCommitCompareStatus = fn
	commitOrderMu.Unlock()
}

// seedCommitOrder marks (reported at-or-ahead-of target) = v in the cache, as
// a completed background resolve would.
func seedCommitOrder(reported, target string, v bool) {
	commitOrderMu.Lock()
	commitOrderCache[commitOrderKey{target: shortSHA(target), reported: shortSHA(reported)}] = v
	commitOrderMu.Unlock()
}

func TestCommitAtOrAheadOfTargetSameCommitNoFetch(t *testing.T) {
	resetCommitOrderState(t)
	fetchRan := false
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		fetchRan = true
		return "", errors.New("unexpected")
	})
	// Short/full mismatch is still the same commit.
	if !commitAtOrAheadOfTarget("be8c36eb1234567", "be8c36e", nil) {
		t.Error("same commit (short/full) should be at-or-ahead without any fetch")
	}
	// Empty inputs never read as complete.
	if commitAtOrAheadOfTarget("", "aaaaaaa", nil) || commitAtOrAheadOfTarget("bbbbbbb", "", nil) {
		t.Error("empty SHA must never be at-or-ahead")
	}
	if fetchRan {
		t.Error("fetch must not run for same-commit or empty pairs")
	}
}

func TestResolveCommitOrderStatuses(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"ahead", true},
		{"identical", true},
		{"behind", false},
		{"diverged", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			resetCommitOrderState(t)
			key := commitOrderKey{target: "aaaaaaa", reported: "bbbbbbb"}
			resolveCommitOrder(key, func(base, head string, logger *slog.Logger) (string, error) {
				return tc.status, nil
			}, nil)
			if got := commitAtOrAheadOfTarget("bbbbbbb", "aaaaaaa", nil); got != tc.want {
				t.Errorf("status %q: at-or-ahead = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestResolveCommitOrderErrorNotCached(t *testing.T) {
	resetCommitOrderState(t)
	key := commitOrderKey{target: "aaaaaaa", reported: "bbbbbbb"}
	resolveCommitOrder(key, func(base, head string, logger *slog.Logger) (string, error) {
		return "", errors.New("rate limited")
	}, nil)
	commitOrderMu.Lock()
	_, cached := commitOrderCache[key]
	inFlight := commitOrderInFlight[key]
	commitOrderMu.Unlock()
	if cached {
		t.Error("a failed resolve must not be cached (it must retry later)")
	}
	if inFlight {
		t.Error("a failed resolve must clear the in-flight marker so retries can start")
	}
}

func TestResolveCommitOrderCacheBounded(t *testing.T) {
	resetCommitOrderState(t)
	commitOrderMu.Lock()
	for i := 0; i < commitOrderCacheMax; i++ {
		commitOrderCache[commitOrderKey{target: "t", reported: string(rune(i))}] = false
	}
	commitOrderMu.Unlock()
	key := commitOrderKey{target: "aaaaaaa", reported: "bbbbbbb"}
	resolveCommitOrder(key, func(base, head string, logger *slog.Logger) (string, error) {
		return "ahead", nil
	}, nil)
	commitOrderMu.Lock()
	size := len(commitOrderCache)
	v := commitOrderCache[key]
	commitOrderMu.Unlock()
	if size != 1 || !v {
		t.Errorf("overflowing cache should reset and keep only the new answer; size=%d v=%v", size, v)
	}
}

// The exact vllmd-13 scenario, step 1 (regression for the already-merged
// clears): Upgrading=true toward shaA; the spoke re-pulled its FLOATING tag
// and reports shaB where shaB == registry latest != shaA. Both the row latch
// and the armed heartbeat fallback must drain.
func TestHandleHeartbeatFloatingTagLandedOnLatestNotTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	resetSHACaches(t)
	s := newHeartbeatHub()

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "bbbbbbb"}
	latestSHAMu.Unlock()

	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "aaaaaaa", GitHash: "old0000",
		UpgradeStartedAt: time.Now().Add(-10 * time.Minute),
	}}
	s.heartbeatUpgrade["h1"] = "aaaaaaa"

	rec := postHeartbeat(t, s,
		`{"hive_id":"h1","git_hash":"bbbbbbb","git_branch":"v2","upgrading":false,"image_ref":"ghcr.io/hivecommons/hive:v2-latest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	_, armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" || !entry.UpgradeStartedAt.IsZero() {
		t.Errorf("expected latch fully cleared, got %+v", entry)
	}
	if armed {
		t.Error("expected armed heartbeat fallback drained once the spoke is at latest")
	}
	if strings.Contains(rec.Body.String(), "upgrade_to") {
		t.Errorf("hub must not re-instruct a completed upgrade, got %s", rec.Body.String())
	}
}

// The exact vllmd-13 WEDGE (the condition that blocked every merged clear):
// the branch advanced AGAIN after the spoke pulled, so the reported shaB
// equals NEITHER the armed target shaA NOR the current registry latest shaC,
// and the stored row already carries shaB (so the hash-changed clear can
// never fire either). Only ancestry can prove shaB surpassed shaA. With the
// pair resolved as "ahead", the badge must clear and the stale UpgradeTo
// re-instruct loop must stop.
func TestHandleHeartbeatClearsWhenReportedAheadOfStaleTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	resetSHACaches(t)
	s := newHeartbeatHub()

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "ccccccc"} // advanced again
	latestSHAMu.Unlock()

	// Stored row already saw the spoke on shaB (steady wedge state) and no
	// image_ref is on the wire, so pre-fix NOTHING cleared this.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "aaaaaaa", GitHash: "bbbbbbb",
		UpgradeStartedAt: time.Now().Add(-time.Hour),
	}}
	s.heartbeatUpgrade["h1"] = "aaaaaaa"
	seedCommitOrder("bbbbbbb", "aaaaaaa", true) // resolved: shaB is ahead of shaA

	rec := postHeartbeat(t, s,
		`{"hive_id":"h1","git_hash":"bbbbbbb","git_branch":"v2","upgrading":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	_, armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" || !entry.UpgradeStartedAt.IsZero() {
		t.Errorf("expected latch cleared for a spoke AHEAD of its stale target, got %+v", entry)
	}
	if armed {
		t.Error("expected stale heartbeat fallback drained — re-instructing it re-rolls the spoke forever")
	}
	if strings.Contains(rec.Body.String(), "upgrade_to") {
		t.Errorf("hub must not re-instruct a surpassed target, got %s", rec.Body.String())
	}
}

// Same wedge but the ancestry is UNRESOLVED: the hub must keep its previous
// (latched + instructing) behaviour rather than guess — clearing on an
// unknown pair would cancel genuinely undelivered upgrades.
func TestHandleHeartbeatKeepsLatchWhileAncestryUnresolved(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	resetSHACaches(t)
	// The unknown pair kicks a background resolve; stub it so the test never
	// touches the network.
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "", errors.New("stubbed offline")
	})
	s := newHeartbeatHub()

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "ccccccc"}
	latestSHAMu.Unlock()

	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "aaaaaaa", GitHash: "bbbbbbb",
		UpgradeStartedAt: time.Now().Add(-time.Hour),
	}}
	s.heartbeatUpgrade["h1"] = "aaaaaaa"

	rec := postHeartbeat(t, s,
		`{"hive_id":"h1","git_hash":"bbbbbbb","git_branch":"v2","upgrading":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if !entry.Upgrading || entry.UpgradeTarget != "aaaaaaa" {
		t.Errorf("unresolved ancestry must not clear the latch, got %+v", entry)
	}
	if !strings.Contains(rec.Body.String(), "upgrade_to") {
		t.Errorf("unresolved ancestry keeps the instruction on the wire, got %s", rec.Body.String())
	}
}

// The orphan sweep must treat a spoke at-or-ahead of its target as CONVERGED:
// the latch is cleared, but as a completed upgrade — never re-armed, never
// charged against maxOrphanedUpgradeSweeps, and never escalated to a false
// permanent UpgradeFailed (the fault this test originally guarded).
//
// It formerly asserted orphaned=false, i.e. "leave it to the heartbeat path".
// That deferral wedged z-mlz-manager: once the entry records the target SHA,
// later beats carry no transition for the completion chain to act on, so the
// latch survived forever. Clearing is now correct; `converged` is what keeps
// the abandoned-attempt handling from firing on a success.
func TestEvaluateOrphanedUpgradeAheadOfTargetIsConverged(t *testing.T) {
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "", errors.New("stubbed offline")
	})
	now := time.Now()
	entry := &RegistryEntry{
		ID: "h1", Upgrading: true, UpgradeTarget: "aaaaaaa", GitHash: "bbbbbbb",
		UpgradeStartedAt: now.Add(-3 * orphanedUpgradeClearAfter),
		LastHeartbeat:    now.Add(-time.Minute).UTC().Format(time.RFC3339),
	}

	// Unresolved ancestry: liveness + wrong SHA reads as an ABANDONED orphan —
	// we cannot yet prove the spoke surpassed the target.
	ev := evaluateOrphanedUpgrade(entry, now, "ccccccc")
	if !ev.orphaned {
		t.Fatal("sanity: unresolved ancestry should still evaluate as orphaned")
	}
	if ev.converged {
		t.Errorf("unresolved ancestry cannot prove convergence: %+v", ev)
	}

	// Resolved ahead: cleared, and marked converged so the sweep records a
	// completion rather than re-arming a stale pin.
	seedCommitOrder("bbbbbbb", "aaaaaaa", true)
	ev = evaluateOrphanedUpgrade(entry, now, "ccccccc")
	if !ev.orphaned {
		t.Errorf("a spoke ahead of its target must have its stale latch cleared: %+v", ev)
	}
	if !ev.converged {
		t.Errorf("a spoke ahead of its target is a COMPLETED upgrade: %+v", ev)
	}
}

// triggerAutoUpgrades' stale-recovery must CLEAR a manual upgrade whose spoke
// surpassed the armed target instead of re-arming the original stale pin
// forever (manual upgrades never advance their target, so pre-fix this
// re-stamped Upgrading/UpgradeTarget and re-instructed the same stale SHA
// every cycle).
func TestTriggerAutoUpgradesClearsSurpassedManualTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	resetSHACaches(t)

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: false, Status: "running", ClusterID: "remote"})
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "ccccccc"}
	latestSHAMu.Unlock()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret,
		heartbeatUpgrade: map[string]string{"h1": "aaaaaaa"},
		clusters:         map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "bbbbbbb", Upgrading: true,
		UpgradeTarget: "aaaaaaa", UpgradeStartedAt: time.Now().Add(-2 * time.Hour),
	}}
	seedCommitOrder("bbbbbbb", "aaaaaaa", true)

	s.triggerAutoUpgrades()

	s.mu.RLock()
	entry := s.registry.Hives[0]
	_, armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" {
		t.Errorf("expected surpassed-target latch cleared, got %+v", entry)
	}
	if armed {
		t.Error("expected stale heartbeat fallback drained, not re-armed")
	}
}

// The production compare fetcher's HTTP behaviour, exercised directly against
// a local server. TestMain replaces the fetchCommitCompareStatus var with an
// offline stub for the whole suite (background resolves must never reach the
// real GitHub API from unit tests), so this is the one place the real
// implementation runs — via the realFetchCommitCompareStatus handle captured
// before the swap.
func TestFetchCommitCompareStatusHTTP(t *testing.T) {
	if realFetchCommitCompareStatus == nil {
		t.Fatal("TestMain did not capture the real compare fetcher")
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
		wantErr bool
	}{
		{"ahead", func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/repos/kubestellar/hive/compare/aaaaaaa...bbbbbbb") {
				t.Errorf("unexpected compare path %s", r.URL.Path)
			}
			w.Write([]byte(`{"status":"ahead"}`))
		}, "ahead", false},
		{"non-200", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, "", true},
		{"empty status", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}, "", true},
		{"bad json", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{`))
		}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			oldBase := githubAPIBase
			githubAPIBase = srv.URL
			defer func() { githubAPIBase = oldBase }()

			status, err := realFetchCommitCompareStatus("aaaaaaa", "bbbbbbb", slog.Default())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got status %q", status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}
}
