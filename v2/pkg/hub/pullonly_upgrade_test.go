package hub

import (
	"log/slog"
	"testing"
	"time"
)

// These tests come in matched PAIRS. Every assertion that a pull-only hive must
// NOT be armed/re-armed is accompanied by a positive control on a REACHABLE
// cluster asserting the normal auto-upgrade still happens. Without that control,
// a change that simply disabled auto-upgrade everywhere would pass the whole
// file — which is the failure mode this repo has repeatedly shipped (a test that
// passes for the wrong reason). The controls are what make the pull-only
// assertions mean "refused BECAUSE undeliverable" rather than "nothing works".

// pullOnlyTestServer builds a hub with one reachable remote cluster and one
// declared pull-only cluster, plus a known latest SHA for branch v4.
func pullOnlyTestServer(t *testing.T) *HubServer {
	t.Helper()
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v4"] = branchSHAInfo{SHA: "7cd059b"}
	latestSHAMu.Unlock()

	return &HubServer{
		logger:       slog.Default(),
		hubSecret:    testHubSecret,
		hubGitBranch: "v4",
		// A real store, so the surfacing assertions read the same path
		// production writes rather than a stub that cannot fail.
		timeline:         newTimelineStore(),
		heartbeatUpgrade: make(map[string]string),
		clusters: map[string]ClusterConfig{
			// Reachable: a normal remote cluster with a kubeconfig.
			"hive-oke": {ID: "hive-oke", InCluster: true, Domain: "hive.kubestellar.io"},
			// Undeliverable BY DECLARATION — audit-8 F21. The hub has no write
			// path here and is not meant to have one.
			"vllm-d": {ID: "vllm-d", PullOnly: true, Domain: "vllmd.example.cloud"},
		},
	}
}

// TestAutoUpgradeNotArmedForPullOnlyCluster is the primary assertion: the hub
// must not ARM an upgrade it structurally cannot deliver. Arming one is what
// latches Upgrading=true with no mechanism to ever clear it.
func TestAutoUpgradeNotArmedForPullOnlyCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-1")
	saveSaaSHive(&SaaSHive{
		ID: "ph-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// Behind latest, so absent the pull-only check this WOULD be armed.
	s.registry.Hives = []RegistryEntry{{ID: "ph-1", GitBranch: "v4", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.Upgrading {
		t.Error("armed an upgrade for a hive on a pull-only cluster: the hub has no write " +
			"path to deliver it, so Upgrading=true can never be cleared and the hive " +
			"wedges as perpetually 'Upgrading' while reading offline")
	}
	if h.UpgradeTarget != "" {
		t.Errorf("UpgradeTarget = %q, want empty — nothing should be targeted", h.UpgradeTarget)
	}
	if got := s.heartbeatUpgrade["ph-1"]; got != "" {
		t.Errorf("heartbeatUpgrade[ph-1] = %q, want empty — the heartbeat fallback must not "+
			"be armed either; an unassigned placeholder has no spoke to consume it", got)
	}
}

// TestAutoUpgradeStillArmedForReachableCluster is the POSITIVE CONTROL for the
// test above. If this fails, the fix has disabled auto-upgrade generally rather
// than refusing only the undeliverable case, and every other assertion in this
// file is worthless.
func TestAutoUpgradeStillArmedForReachableCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ok-1")
	saveSaaSHive(&SaaSHive{
		ID: "ok-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "hive-oke",
	})
	s.registry.Hives = []RegistryEntry{{ID: "ok-1", GitBranch: "v4", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if !h.Upgrading {
		t.Fatal("a hive on a REACHABLE cluster must still auto-upgrade normally — " +
			"the pull-only refusal must be narrow, not a blanket disable")
	}
	if h.UpgradeTarget != "7cd059b" {
		t.Errorf("UpgradeTarget = %q, want 7cd059b (latest for v4)", h.UpgradeTarget)
	}
}

// TestStaleUpgradeNotReArmedForPullOnlyCluster covers the branch that actually
// produced the measured damage: 208 re-arms in 15 minutes. A stale upgrade on a
// cluster the hub cannot reach must be ABANDONED, not re-armed — re-arming
// reproduces the identical no-op every staleUpgradeTimeout forever.
func TestStaleUpgradeNotReArmedForPullOnlyCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-2")
	saveSaaSHive(&SaaSHive{
		ID: "ph-2", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// Already latched and long past staleUpgradeTimeout — the live shape, where
	// stale_minutes was observed between 90 and 104.
	s.registry.Hives = []RegistryEntry{{
		ID: "ph-2", GitBranch: "v4", GitHash: "old1234",
		Upgrading: true, UpgradeTarget: "7cd059b",
		UpgradeStartedAt: time.Now().Add(-99 * time.Minute),
	}}
	s.heartbeatUpgrade["ph-2"] = "7cd059b"

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.Upgrading {
		t.Error("stale undeliverable upgrade was re-armed instead of abandoned — " +
			"this is the 208-re-arms-in-15-minutes loop")
	}
	if !h.UpgradeStartedAt.IsZero() {
		t.Error("UpgradeStartedAt must be zeroed when the latch is cleared, or the " +
			"next cycle reads a stale elapsed and reports the hive stuck again")
	}
	if got := s.heartbeatUpgrade["ph-2"]; got != "" {
		t.Errorf("heartbeatUpgrade[ph-2] = %q, want empty — the armed instruction must "+
			"be dropped so no path keeps re-delivering it", got)
	}
}

// TestStaleUpgradeStillRecoveredForReachableCluster is the POSITIVE CONTROL for
// the abandonment above. Stale-upgrade recovery on a reachable cluster is a real
// and valuable behaviour (#2476) and must survive.
func TestStaleUpgradeStillRecoveredForReachableCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ok-2")
	saveSaaSHive(&SaaSHive{
		ID: "ok-2", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "hive-oke",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "ok-2", GitBranch: "v4", GitHash: "old1234",
		Upgrading: true, UpgradeTarget: "7cd059b",
		UpgradeStartedAt: time.Now().Add(-99 * time.Minute),
	}}

	s.triggerAutoUpgrades()

	if !s.registry.Hives[0].Upgrading {
		t.Fatal("a stale upgrade on a REACHABLE cluster must still be recovered/re-armed — " +
			"abandonment must apply only where delivery is impossible")
	}
	if got := s.heartbeatUpgrade["ok-2"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade[ok-2] = %q, want 7cd059b — recovery must keep the "+
			"instruction alive for a reachable hive", got)
	}
}

// TestUndeliverableUpgradeCannotAccumulateStaleness is the invariant the whole
// fix exists to establish, asserted the way the bug actually presented: over
// REPEATED poll cycles.
//
// The original loop survived every individual-cycle check — each cycle looked
// like a reasonable recovery. Only the accumulation was pathological: elapsed
// climbing past 90 minutes while the hub re-armed 208 times. So this replays
// many cycles and asserts the hive never latches at all, which is the only
// formulation that would have caught the live behaviour.
func TestUndeliverableUpgradeCannotAccumulateStaleness(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-3")
	saveSaaSHive(&SaaSHive{
		ID: "ph-3", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{ID: "ph-3", GitBranch: "v4", GitHash: "old1234"}}

	for cycle := 1; cycle <= 25; cycle++ {
		s.triggerAutoUpgrades()
		h := s.registry.Hives[0]
		if h.Upgrading {
			t.Fatalf("cycle %d: hive latched Upgrading — staleness is accumulating for an "+
				"upgrade that can never be delivered", cycle)
		}
		if !h.UpgradeStartedAt.IsZero() {
			t.Fatalf("cycle %d: UpgradeStartedAt is non-zero (%v) — the elapsed counter that "+
				"reached 104 minutes live is being started again", cycle, h.UpgradeStartedAt)
		}
		if got := s.heartbeatUpgrade["ph-3"]; got != "" {
			t.Fatalf("cycle %d: heartbeatUpgrade re-armed to %q — this is the re-arm loop",
				cycle, got)
		}
	}
}

// TestUndeliverableUpgradeIsSurfacedNotSilent guards the second half of the fix.
// Silently skipping is how the original wedge went unnoticed: a hive with
// auto_upgrade=true that never upgrades looks exactly like one already at latest.
// The refusal must leave an operator-visible record.
func TestUndeliverableUpgradeIsSurfacedNotSilent(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-4")
	saveSaaSHive(&SaaSHive{
		ID: "ph-4", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{ID: "ph-4", GitBranch: "v4", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	events := s.timeline.recent("ph-4", 100)
	var found bool
	for _, e := range events {
		if e.Kind == TimelineUpgradeStale {
			found = true
			// The reason must NAME the cluster, or an operator reading it cannot
			// act on it.
			if !contains(e.Detail, "vllm-d") {
				t.Errorf("timeline detail %q does not name the responsible cluster", e.Detail)
			}
			if !contains(e.Detail, "pull-only") {
				t.Errorf("timeline detail %q does not explain WHY it was refused", e.Detail)
			}
		}
	}
	if !found {
		t.Error("the refusal left NO timeline record — an operator cannot distinguish " +
			"'deliberately not upgraded' from 'silently broken', which is exactly how " +
			"this bug survived")
	}
}

// TestUndeliverableRefusalIsDeduplicated stops the fix from trading an unbounded
// re-arm loop for an unbounded timeline. StartLatestSHAPoller ticks every 2
// minutes, so an un-deduplicated write would append ~720 identical entries per
// hive per day and bury the genuine events.
func TestUndeliverableRefusalIsDeduplicated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-5")
	saveSaaSHive(&SaaSHive{
		ID: "ph-5", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{ID: "ph-5", GitBranch: "v4", GitHash: "old1234"}}

	for i := 0; i < 10; i++ {
		s.triggerAutoUpgrades()
	}

	var stale int
	for _, e := range s.timeline.recent("ph-5", 100) {
		if e.Kind == TimelineUpgradeStale {
			stale++
		}
	}
	if stale != 1 {
		t.Errorf("timeline recorded %d refusal entries across 10 identical cycles, want 1 — "+
			"the refusal must be reported once per target, not once per poll", stale)
	}
}

// TestUpgradeDeliverablePredicate pins the reachability predicate itself, and in
// particular that it reuses the EXISTING clusterRecentlyUnreachable notion (the
// F21 vocabulary) rather than introducing a second, divergent one.
func TestUpgradeDeliverablePredicate(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"in":   {ID: "in", InCluster: true},
			"pull": {ID: "pull", PullOnly: true, Domain: "d"},
			"rem":  {ID: "rem", KubeconfigPath: "/tmp/kc", Domain: "d"},
		},
		clusterUnreachableUntil: map[string]time.Time{},
	}

	cases := []struct {
		name    string
		cluster *ClusterConfig
		want    bool
	}{
		{"nil cluster is not deliverable", nil, false},
		{"in-cluster is deliverable", &ClusterConfig{ID: "in", InCluster: true}, true},
		{"pull-only is never deliverable", &ClusterConfig{ID: "pull", PullOnly: true}, false},
		{"reachable remote is deliverable", &ClusterConfig{ID: "rem", KubeconfigPath: "/tmp/kc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.upgradeDeliverable(tc.cluster); got != tc.want {
				t.Errorf("upgradeDeliverable = %v, want %v", got, tc.want)
			}
		})
	}

	// A cluster inside the LEARNED unreachable window must also be undeliverable:
	// an upgrade armed at a cluster the hub just failed to dial wedges exactly
	// like one aimed at a declared pull-only cluster. This is the assertion that
	// proves the predicate is the shared F21 notion, not a bare PullOnly test.
	s.markClusterUnreachable("rem")
	if s.upgradeDeliverable(&ClusterConfig{ID: "rem", KubeconfigPath: "/tmp/kc"}) {
		t.Error("a cluster inside the unreachable breaker window must not be deliverable — " +
			"upgradeDeliverable must reuse clusterRecentlyUnreachable, not re-test PullOnly")
	}
}

// TestUpgradeBranchDefaultsToHubBranchNotV2 is the 0b78dc0 regression. A
// placeholder that has never heartbeated has an empty GitBranch; the old
// hardcoded `branch = "v2"` then resolved its target against v2 while running on
// a v4 hub, which is how a v2-only commit was issued as a v4 hive's target.
func TestUpgradeBranchDefaultsToHubBranchNotV2(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v4"}

	if got := s.upgradeBranchOrDefault(""); got != "v4" {
		t.Errorf("upgradeBranchOrDefault(\"\") = %q, want v4 — an unset branch must resolve "+
			"against the HUB's branch; defaulting to v2 on a v4 hub is what armed 0b78dc0, "+
			"a commit that does not exist on v4", got)
	}
	// An explicitly reported branch always wins — the default must never override
	// a hive that has told us what it runs.
	if got := s.upgradeBranchOrDefault("v2"); got != "v2" {
		t.Errorf("upgradeBranchOrDefault(\"v2\") = %q, want v2", got)
	}
	// A hub that cannot identify its own branch falls back to the historical
	// constant rather than resolving against "" (which yields no SHA at all).
	s2 := &HubServer{logger: slog.Default()}
	if got := s2.upgradeBranchOrDefault(""); got != "v2" {
		t.Errorf("with no hub branch, upgradeBranchOrDefault(\"\") = %q, want v2", got)
	}
}

// TestPullOnlyHiveNeverTargetedWithForeignBranchSHA joins the two fixes at the
// surface where they were both observed: a placeholder on a pull-only cluster
// with no reported branch. It must be neither armed nor aimed at a v2 commit.
func TestPullOnlyHiveNeverTargetedWithForeignBranchSHA(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	forgetUndeliverableUpgrade("ph-6")
	// The v2 SHA that was actually being issued live.
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "0b78dc0"}
	latestSHAMu.Unlock()

	saveSaaSHive(&SaaSHive{
		ID: "ph-6", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// GitBranch deliberately EMPTY — the unassigned-placeholder shape.
	s.registry.Hives = []RegistryEntry{{ID: "ph-6", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.UpgradeTarget == "0b78dc0" {
		t.Error("hive was targeted at 0b78dc0, a v2-only commit, from a v4 hub")
	}
	if h.Upgrading {
		t.Error("hive on a pull-only cluster must not be latched Upgrading at all")
	}
}
