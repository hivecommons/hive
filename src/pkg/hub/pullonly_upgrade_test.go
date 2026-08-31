package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests come in matched PAIRS. Every assertion that an uncollectible hive
// must NOT be armed is accompanied by a positive control asserting a hive that
// CAN collect still auto-upgrades normally. Without those controls, a change
// that simply disabled auto-upgrade would pass the whole file.
//
// The most important control here is TestPullOnlyHiveThatHeartbeatsStillUpgrades.
// An earlier revision of this fix gated on CLUSTER REACHABILITY, which would
// have silently disabled auto-upgrade for every pull-only spoke that heartbeats
// perfectly well — turning a loud wedge into a silent permanent opt-out. That
// test is what makes the reachability-based gate un-reintroducible.

func rfc3339At(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// pullOnlyTestServer builds a hub with one in-cluster and one declared pull-only
// cluster, plus a known latest SHA for branch v4.
func pullOnlyTestServer(t *testing.T) *HubServer {
	t.Helper()
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v4"] = branchSHAInfo{SHA: "7cd059b"}
	latestSHAMu.Unlock()

	return &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		hubGitBranch:     "v4",
		timeline:         newTimelineStore(),
		heartbeatUpgrade: make(map[string]string),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", InCluster: true, Domain: "hive.kubestellar.io"},
			// The hub cannot WRITE here (audit-8 F21) — but its spokes pull
			// their upgrades over their own outbound heartbeat, so this must
			// NOT affect whether an upgrade is armed.
			"vllm-d": {ID: "vllm-d", PullOnly: true, Domain: "vllmd.example.cloud"},
		},
	}
}

// TestPullOnlyHiveThatHeartbeatsStillUpgrades is THE critical control.
//
// Delivery is pull: the spoke reads UpgradeTo off its own heartbeat response
// (server.go) and patches its own Deployment with its own ServiceAccount
// (cmd/hive/main.go → self_upgrade.go). The hub's inability to kubectl into the
// cluster is irrelevant to that path. A gate keyed on cluster reachability would
// disable auto-upgrade for 40+ healthy spokes; this test fails if anyone
// reintroduces one.
func TestPullOnlyHiveThatHeartbeatsStillUpgrades(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("pull-live")
	saveSaaSHive(&SaaSHive{
		ID: "pull-live", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// On a pull-only cluster, but ALIVE and collecting instructions.
	s.registry.Hives = []RegistryEntry{{
		ID: "pull-live", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if !h.Upgrading {
		t.Fatal("a pull-only spoke that heartbeats MUST still auto-upgrade — it collects " +
			"the instruction over its own outbound heartbeat and patches its own " +
			"Deployment. Gating on cluster reachability here would silently disable " +
			"auto-upgrade for 40+ healthy spokes, which is worse than the wedge")
	}
	if h.UpgradeTarget != "7cd059b" {
		t.Errorf("UpgradeTarget = %q, want 7cd059b", h.UpgradeTarget)
	}
	if got := s.heartbeatUpgrade["pull-live"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade = %q, want 7cd059b — the pull instruction must be armed", got)
	}
}

// TestAutoUpgradeNotArmedForNeverHeartbeatedHive is the primary assertion: a hive
// that has never checked in cannot collect an instruction, so arming one only
// latches a flag nothing will ever clear.
func TestAutoUpgradeNotArmedForNeverHeartbeatedHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ph-1")
	saveSaaSHive(&SaaSHive{
		ID: "ph-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// The live wedged shape: no LastHeartbeat at all.
	s.registry.Hives = []RegistryEntry{{ID: "ph-1", GitBranch: "v4", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.Upgrading {
		t.Error("armed an upgrade for a hive that has never heartbeated: nothing will ever " +
			"collect it, so Upgrading=true latches forever and the hive wedges as " +
			"perpetually 'Upgrading' while reading offline")
	}
	if got := s.heartbeatUpgrade["ph-1"]; got != "" {
		t.Errorf("heartbeatUpgrade[ph-1] = %q, want empty", got)
	}
}

// TestAutoUpgradeNotArmedForLongDeadHive covers the other uncollectible shape: a
// hive that once heartbeated but has been silent past the point where the
// registry considers it present.
func TestAutoUpgradeNotArmedForLongDeadHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("dead-1")
	saveSaaSHive(&SaaSHive{
		ID: "dead-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "hive-oke",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "dead-1", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-48 * time.Hour)),
	}}

	s.triggerAutoUpgrades()

	if s.registry.Hives[0].Upgrading {
		t.Error("armed an upgrade for a hive silent for 48h — it cannot collect it")
	}
}

// TestBrieflyQuietHiveStillUpgrades is the counter-control to the test above, and
// guards the threshold choice. A spoke that is one beat late — or quiet BECAUSE
// it is restarting into an upgrade — must still be armed. Gating on the 5-minute
// Online threshold instead of staleRemoveAge would be a different silent opt-out.
func TestBrieflyQuietHiveStillUpgrades(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("quiet-1")
	saveSaaSHive(&SaaSHive{
		ID: "quiet-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// 20 minutes quiet: past maxHeartbeatAge (5m, the Online pill), well inside
	// staleRemoveAge (24h). This hive will come back.
	s.registry.Hives = []RegistryEntry{{
		ID: "quiet-1", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-20 * time.Minute)),
	}}

	s.triggerAutoUpgrades()

	if !s.registry.Hives[0].Upgrading {
		t.Error("a briefly-quiet spoke must still be armed — it will collect the " +
			"instruction on its next beat. Gating on the 5-minute Online threshold " +
			"would refuse to upgrade a spoke that is merely mid-restart")
	}
}

// TestStaleUpgradeNotReArmedForUncollectibleHive covers the branch that produced
// the measured damage: re-arming an instruction nothing will ever pick up.
func TestStaleUpgradeNotReArmedForUncollectibleHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ph-2")
	saveSaaSHive(&SaaSHive{
		ID: "ph-2", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "ph-2", GitBranch: "v4", GitHash: "old1234",
		Upgrading: true, UpgradeTarget: "7cd059b",
		UpgradeStartedAt: time.Now().Add(-146 * time.Minute),
	}}
	s.heartbeatUpgrade["ph-2"] = "7cd059b"

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.Upgrading {
		t.Error("stale uncollectible upgrade was re-armed instead of abandoned — " +
			"this is the unbounded re-arm loop")
	}
	if !h.UpgradeStartedAt.IsZero() {
		t.Error("UpgradeStartedAt must be zeroed, or the next cycle reads a stale elapsed")
	}
	if got := s.heartbeatUpgrade["ph-2"]; got != "" {
		t.Errorf("heartbeatUpgrade[ph-2] = %q, want empty", got)
	}
}

// TestStaleUpgradeStillRecoveredForLiveHive is the POSITIVE CONTROL for the
// abandonment above — including on a PULL-ONLY cluster, since recovery there is
// exactly what the heartbeat fallback exists for.
func TestStaleUpgradeStillRecoveredForLiveHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ok-2")
	saveSaaSHive(&SaaSHive{
		ID: "ok-2", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "ok-2", GitBranch: "v4", GitHash: "old1234",
		Upgrading: true, UpgradeTarget: "7cd059b",
		UpgradeStartedAt: time.Now().Add(-99 * time.Minute),
		LastHeartbeat:    rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	s.triggerAutoUpgrades()

	if !s.registry.Hives[0].Upgrading {
		t.Fatal("a stale upgrade on a LIVE hive must still be recovered/re-armed — " +
			"abandonment must apply only where the instruction cannot be collected")
	}
	if got := s.heartbeatUpgrade["ok-2"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade[ok-2] = %q, want 7cd059b", got)
	}
}

// TestUncollectibleUpgradeCannotAccumulateStaleness asserts the invariant the way
// the bug actually presented: over REPEATED poll cycles. The original loop
// survived every individual-cycle check; only the accumulation was pathological.
func TestUncollectibleUpgradeCannotAccumulateStaleness(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ph-3")
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
				"upgrade that will never be collected", cycle)
		}
		if !h.UpgradeStartedAt.IsZero() {
			t.Fatalf("cycle %d: UpgradeStartedAt non-zero (%v) — the elapsed counter that "+
				"reached 146 minutes live is being started again", cycle, h.UpgradeStartedAt)
		}
		if got := s.heartbeatUpgrade["ph-3"]; got != "" {
			t.Fatalf("cycle %d: heartbeatUpgrade re-armed to %q — the re-arm loop", cycle, got)
		}
	}
}

// TestUncollectibleUpgradeIsSurfacedNotSilent guards the second half of the fix.
// Silence is how the original wedge went unnoticed.
func TestUncollectibleUpgradeIsSurfacedNotSilent(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ph-4")
	saveSaaSHive(&SaaSHive{
		ID: "ph-4", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{ID: "ph-4", GitBranch: "v4", GitHash: "old1234"}}

	s.triggerAutoUpgrades()

	var found bool
	for _, e := range s.timeline.recent("ph-4", 100) {
		if e.Kind == TimelineUpgradeStale {
			found = true
			if !strings.Contains(e.Detail, "heartbeat") {
				t.Errorf("timeline detail %q does not explain WHY it was refused", e.Detail)
			}
		}
	}
	if !found {
		t.Error("the refusal left NO timeline record — an operator cannot distinguish " +
			"'deliberately not upgraded' from 'silently broken'")
	}
}

// TestUncollectibleRefusalIsDeduplicated stops the fix trading an unbounded
// re-arm loop for an unbounded timeline (the poller ticks every 2 minutes).
func TestUncollectibleRefusalIsDeduplicated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("ph-5")
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
		t.Errorf("timeline recorded %d refusal entries across 10 identical cycles, want 1", stale)
	}
}

// TestUpgradeCollectiblePredicate pins the predicate, and in particular that it
// is about HEARTBEAT HISTORY and nothing else — no cluster, no reachability.
func TestUpgradeCollectiblePredicate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		last string
		want bool
	}{
		{"never heartbeated", "", false},
		{"unparseable timestamp", "not-a-timestamp", false},
		{"beating right now", rfc3339At(now.Add(-10 * time.Second)), true},
		{"quiet 20m (mid-restart)", rfc3339At(now.Add(-20 * time.Minute)), true},
		{"quiet 23h, still present", rfc3339At(now.Add(-23 * time.Hour)), true},
		{"silent 48h", rfc3339At(now.Add(-48 * time.Hour)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradeCollectible(tc.last, now); got != tc.want {
				t.Errorf("upgradeCollectible(%q) = %v, want %v", tc.last, got, tc.want)
			}
		})
	}
}

// TestUpgradeBranchDefaultsToHubBranchNotV2 is the 0b78dc0 regression. A
// placeholder that has never heartbeated has an empty GitBranch; the old
// hardcoded `branch = "v2"` resolved its target against v2 on a v4 hub — and v2
// is no longer maintained.
func TestUpgradeBranchDefaultsToHubBranchNotV2(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v4"}

	if got := s.upgradeBranchOrDefault(""); got != "v4" {
		t.Errorf("upgradeBranchOrDefault(\"\") = %q, want v4 — defaulting to v2 on a v4 hub "+
			"is what armed 0b78dc0, a commit that does not exist on v4", got)
	}
	if got := s.upgradeBranchOrDefault("v2"); got != "v2" {
		t.Errorf("upgradeBranchOrDefault(\"v2\") = %q, want v2 — an explicit branch always wins", got)
	}
	s2 := &HubServer{logger: slog.Default()}
	if got := s2.upgradeBranchOrDefault(""); got != "v2" {
		t.Errorf("with no hub branch, upgradeBranchOrDefault(\"\") = %q, want v2", got)
	}
}

// TestLiveHiveNeverTargetedWithForeignBranchSHA proves the branch fix on a hive
// that CAN collect — which is where a foreign-branch target is actually
// dangerous, because the spoke picks it up and rolls itself onto a v2 build.
func TestLiveHiveNeverTargetedWithForeignBranchSHA(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("live-nobranch")
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "0b78dc0"}
	latestSHAMu.Unlock()

	saveSaaSHive(&SaaSHive{
		ID: "live-nobranch", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// GitBranch empty, but heartbeating — so it WILL collect whatever we arm.
	s.registry.Hives = []RegistryEntry{{
		ID: "live-nobranch", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	s.triggerAutoUpgrades()

	h := s.registry.Hives[0]
	if h.UpgradeTarget == "0b78dc0" {
		t.Error("live hive was targeted at 0b78dc0, a v2-only commit, from a v4 hub — " +
			"it would collect this and roll itself onto an unmaintained branch's build")
	}
	if h.UpgradeTarget != "7cd059b" {
		t.Errorf("UpgradeTarget = %q, want 7cd059b (the hub's own branch)", h.UpgradeTarget)
	}
}

// TestUpgradePathNeverShellsOutToCluster is the push-path retirement guard.
//
// It pins the security property the operator asked for: the hub must not need
// write-capable kubeconfigs into spoke clusters to deliver an upgrade. If anyone
// reintroduces a kubectl call on the auto-upgrade path, the fake kubectl
// installed here records it and this fails.
//
// The positive half is that the upgrade is still DELIVERED — via the heartbeat —
// so this cannot pass by simply breaking upgrades.
func TestUpgradePathNeverShellsOutToCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("nopush")
	// A REACHABLE remote cluster: under the old push model this is exactly the
	// case that would have shelled out to kubectl.
	s.clusters["remote"] = ClusterConfig{
		ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx",
		Domain: "r.example.com",
	}
	saveSaaSHive(&SaaSHive{
		ID: "nopush", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "remote",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "nopush", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	s.triggerAutoUpgrades()

	// Delivered by pull.
	if got := s.heartbeatUpgrade["nopush"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade = %q, want 7cd059b — the upgrade must still be "+
			"delivered, by arming the heartbeat", got)
	}
	if !s.registry.Hives[0].Upgrading {
		t.Error("hive must be latched Upgrading so the dashboard reflects the request")
	}
}

// TestManualUpgradeButtonWorksUnderPull is the operator's explicit constraint:
// the manual Upgrade button must keep working with the push path retired.
//
// Under pull, "working" means the request records a target and arms delivery —
// the spoke applies it on its next beat. This asserts the button's observable
// contract end-to-end through the HTTP handler.
func TestManualUpgradeButtonWorksUnderPull(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.clusters["remote"] = ClusterConfig{
		ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Domain: "r.example.com",
	}
	saveSaaSHive(&SaaSHive{
		ID: "manual-btn", Owner: "alice", Status: "running", ClusterID: "remote",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "manual-btn", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	mkUser(t, "alice")
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "manual-btn")
	rec := httptest.NewRecorder()
	s.handleUpgradeHive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("manual upgrade returned %d, body %q — the button must keep working",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mode":"heartbeat"`) {
		t.Errorf("response %q should report heartbeat delivery so the UI can say "+
			"'queued' rather than implying an immediate roll", rec.Body.String())
	}
	s.mu.RLock()
	armed := s.heartbeatUpgrade["manual-btn"]
	upgrading := s.registry.Hives[0].Upgrading
	s.mu.RUnlock()
	if armed != "7cd059b" {
		t.Errorf("heartbeatUpgrade = %q, want 7cd059b — the click must arm delivery", armed)
	}
	if !upgrading {
		t.Error("the hive must be latched Upgrading so the dashboard reflects the request")
	}
}

// TestAutoUpgradeToggleWorksUnderPull is the other operator constraint. The
// toggle only writes hub-side state, so it must be entirely unaffected — this
// pins that, and that flipping it on makes the hive eligible for arming.
func TestAutoUpgradeToggleWorksUnderPull(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	saveSaaSHive(&SaaSHive{
		ID: "toggle-1", Owner: "alice", Status: "running",
		ClusterID: "vllm-d", AutoUpgrade: false,
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "toggle-1", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	mkUser(t, "alice")
	req := setPathValue(reqWithUser("PUT", "/au", `{"auto_upgrade":true}`, "alice"), "id", "toggle-1")
	rec := httptest.NewRecorder()
	s.handleToggleAutoUpgrade(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("toggle returned %d, body %q", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive("toggle-1"); h == nil || !h.AutoUpgrade {
		t.Fatal("the toggle must persist AutoUpgrade=true")
	}

	// And the setting must actually take effect on the next poll — on a
	// pull-only cluster, which is the case that matters.
	s.forgetUncollectibleUpgrade("toggle-1")
	s.triggerAutoUpgrades()
	if !s.registry.Hives[0].Upgrading {
		t.Error("after enabling auto-upgrade, a live hive must be armed on the next cycle")
	}
}

// ---------------------------------------------------------------------------
// MANUAL "Upgrade now" — the same collectibility gate as the automatic path.
//
// These are the manual-path halves of the pairs above. The bug they pin (#5304)
// is an ASYMMETRY, not a missing feature: triggerAutoUpgrades() already refused
// to arm a hive that cannot COLLECT an upgrade, while handleUpgradeHive — the
// button — armed unconditionally. That made the click strictly worse than the
// auto path it diverged from. The auto path refuses and records the refusal on
// the timeline, so an operator can find out why; the click returned
// {"status":"upgrading"} and a success toast for a target that could never be
// collected, then latched Upgrading=true for the stale sweep to re-arm every
// staleUpgradeTimeout forever.
//
// The asymmetry is also actively misleading: the same spoke auto-upgrade had
// declined would happily accept a manual click, so the button LOOKED like it
// worked around the problem when it had only hidden it.
//
// The positive control (healthy hive → 200) is what stops a regression that
// simply refuses every manual upgrade from passing this file.
// ---------------------------------------------------------------------------

// manualUpgradeReq builds the authenticated manual-upgrade request the button
// sends, with the hive id bound the way the mux would bind it.
func manualUpgradeReq(hiveID, user string) *http.Request {
	return setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", "", user), "id", hiveID)
}

func TestManualUpgradeRefusesUncollectibleHive(t *testing.T) {
	cases := []struct {
		name string
		// lastHeartbeat is RegistryEntry.LastHeartbeat — the ONLY record that
		// carries it. SaaSHive has no such field, so this is also the source the
		// automatic path reads.
		lastHeartbeat func() string
		// wantReason is a fragment the operator-facing refusal must contain.
		wantReason string
	}{
		{
			// The measured wedge population: unassigned placeholders that have
			// never checked in. Under pull delivery nothing will EVER collect.
			name:          "never heartbeated",
			lastHeartbeat: func() string { return "" },
			wantReason:    "never heartbeated",
		},
		{
			// Silent past staleRemoveAge is the same bound the registry already
			// uses to call a hive gone for good. Deliberately NOT
			// maxHeartbeatAge — a spoke one beat late must still be upgradable.
			name:          "silent beyond staleRemoveAge",
			lastHeartbeat: func() string { return rfc3339At(time.Now().Add(-staleRemoveAge - time.Hour)) },
			wantReason:    "beyond the point where it is considered present",
		},
		{
			// An unreadable timestamp is treated as uncollectible: we must not
			// arm on evidence we cannot read (the rule evaluateOrphanedUpgrade
			// applies).
			name:          "unparseable heartbeat",
			lastHeartbeat: func() string { return "not-a-timestamp" },
			wantReason:    "cannot collect",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			mkUser(t, "alice")

			s := pullOnlyTestServer(t)
			s.forgetUncollectibleUpgrade("manual-dead")
			saveSaaSHive(&SaaSHive{
				ID: "manual-dead", Owner: "alice", Status: "running", ClusterID: "vllm-d",
			})
			s.registry.Hives = []RegistryEntry{{
				ID: "manual-dead", GitBranch: "v4", GitHash: "old1234",
				LastHeartbeat: tc.lastHeartbeat(),
			}}

			rec := httptest.NewRecorder()
			s.handleUpgradeHive(rec, manualUpgradeReq("manual-dead", "alice"))

			// 409, matching the pause-switch refusal shape in the same handler.
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 — the manual button must refuse an upgrade "+
					"this hive can never collect, exactly as auto-upgrade does (body=%s)",
					rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("refusal body is not JSON: %v (%s)", err, rec.Body.String())
			}
			// The operator must be told WHY. A bare 409 reproduces the original
			// complaint in a new shape: the click stops working with no reason.
			if !strings.Contains(body["error"], tc.wantReason) {
				t.Errorf("refusal reason %q does not contain %q", body["error"], tc.wantReason)
			}
			// The reason is returned over the wire, so it must stay free of
			// anything that is not the operator's business.
			for _, leak := range []string{"kubeconfig", "/root/", "token", "secret"} {
				if strings.Contains(strings.ToLower(body["error"]), leak) {
					t.Errorf("refusal body leaks %q: %s", leak, body["error"])
				}
			}

			// THE LATCH IS THE BUG. Arming set Upgrading=true against a target
			// nothing would collect, and the stale sweep then re-armed it every
			// staleUpgradeTimeout while beginUpgrade preserved the original start
			// clock — so the elapsed only ever grew. Refusing must leave no latch.
			s.mu.RLock()
			h := s.registry.Hives[0]
			s.mu.RUnlock()
			if h.Upgrading {
				t.Error("refused upgrade still latched Upgrading=true — this is the wedge: " +
					"the stale sweep re-arms it forever and the orphan sweep's retry budget " +
					"cannot break the loop, because a hive that never heartbeated fails " +
					"evaluateOrphanedUpgrade()'s liveness test")
			}
			if h.UpgradeTarget != "" {
				t.Errorf("refused upgrade recorded a target %q", h.UpgradeTarget)
			}
			if got, ok := s.heartbeatUpgrade["manual-dead"]; ok {
				t.Errorf("refused upgrade still armed the heartbeat with %q", got)
			}

			// Refused LOUDLY. The auto path records its refusal on the timeline;
			// the manual path recording nothing is what let this hide as success.
			var found bool
			for _, e := range s.timeline.recent("manual-dead", 100) {
				if e.Kind == TimelineUpgradeStale {
					found = true
				}
			}
			if !found {
				t.Error("the manual refusal left NO timeline record — the operator has no " +
					"durable way to learn why the click did nothing")
			}
		})
	}
}

// TestManualUpgradeHealthyHiveStillArms is the positive control for the gate
// above: without it, a change that refused EVERY manual upgrade would pass.
// A heartbeating spoke must arm exactly as before — including on a pull-only
// cluster, since the hub's inability to kubectl in is irrelevant to a pull
// delivery.
func TestManualUpgradeHealthyHiveStillArms(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("manual-live")
	saveSaaSHive(&SaaSHive{
		ID: "manual-live", Owner: "alice", Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{{
		ID: "manual-live", GitBranch: "v4", GitHash: "old1234",
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	rec := httptest.NewRecorder()
	s.handleUpgradeHive(rec, manualUpgradeReq("manual-live", "alice"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a heartbeating spoke must still upgrade on "+
			"click, on ANY cluster (body=%s)", rec.Code, rec.Body.String())
	}
	// The exact success shape the dashboard parses; "heartbeat" is what makes
	// the UI say "queued" rather than implying an immediate pod roll.
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"upgrading","mode":"heartbeat"}` {
		t.Errorf("success body = %s, want {\"status\":\"upgrading\",\"mode\":\"heartbeat\"}", got)
	}

	s.mu.RLock()
	h := s.registry.Hives[0]
	s.mu.RUnlock()
	if !h.Upgrading || h.UpgradeTarget != "7cd059b" {
		t.Errorf("healthy hive was not armed: Upgrading=%v target=%q", h.Upgrading, h.UpgradeTarget)
	}
	// Delivery is pull: this map entry is the whole mechanism — the spoke reads
	// it off its next heartbeat response and patches its own Deployment.
	if got := s.heartbeatUpgrade["manual-live"]; got != "7cd059b" {
		t.Errorf("heartbeat not armed for delivery: got %q, want 7cd059b", got)
	}
}

// TestManualUpgradeRefusalDoesNotSuppressLaterRefusal pins the de-duplication
// contract across the two paths. noteUncollectibleUpgrade dedupes per
// (hive, target) using per-server memory; a successful arm must CLEAR that
// memory, as the auto path already does on its own successful arm. Otherwise a
// hive that is refused, recovers, is armed, and later goes silent again has its
// second — genuine — refusal swallowed for the same target, putting the
// operator back in the silence this fix exists to remove.
func TestManualUpgradeRefusalDoesNotSuppressLaterRefusal(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("manual-flap")
	saveSaaSHive(&SaaSHive{
		ID: "manual-flap", Owner: "alice", Status: "running", ClusterID: "vllm-d",
	})

	// 1. Dead → refused, and recorded.
	s.registry.Hives = []RegistryEntry{{ID: "manual-flap", GitBranch: "v4", GitHash: "old1234"}}
	rec := httptest.NewRecorder()
	s.handleUpgradeHive(rec, manualUpgradeReq("manual-flap", "alice"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("first click status = %d, want 409", rec.Code)
	}

	// 2. Recovers and is armed. This must clear the de-dup memory.
	s.mu.Lock()
	s.registry.Hives[0].LastHeartbeat = rfc3339At(time.Now())
	s.mu.Unlock()
	rec = httptest.NewRecorder()
	s.handleUpgradeHive(rec, manualUpgradeReq("manual-flap", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("recovered click status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// 3. Goes silent again, same target. The refusal must be reported AFRESH.
	s.mu.Lock()
	s.registry.Hives[0].LastHeartbeat = ""
	s.registry.Hives[0].Upgrading = false
	s.registry.Hives[0].UpgradeTarget = ""
	s.mu.Unlock()
	delete(s.heartbeatUpgrade, "manual-flap")

	before := len(s.timeline.recent("manual-flap", 100))
	rec = httptest.NewRecorder()
	s.handleUpgradeHive(rec, manualUpgradeReq("manual-flap", "alice"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second refusal status = %d, want 409", rec.Code)
	}
	if after := len(s.timeline.recent("manual-flap", 100)); after <= before {
		t.Error("the second, genuine refusal was suppressed by stale de-duplication memory " +
			"from the first — a successful arm must forget it, as the auto path does")
	}
}
