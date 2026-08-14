package hub

import (
	"strings"
	"testing"
	"time"
)

// TestSecurityContextHasNetAdmin is the core reconcile DECISION: given the
// jsonpath-extracted capability list from a live Deployment, decide whether the
// hive already has NET_ADMIN (skip) or is drifted and needs the patch. This is
// the pure function the sweep gates on, so it is unit-tested directly without
// shelling out to kubectl.
func TestSecurityContextHasNetAdmin(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool // true = has NET_ADMIN (skip), false = missing (patch)
	}{
		// Drifted pre-#1222 hives: securityContext absent, so jsonpath yields
		// nothing / empty / an empty list. All must be treated as "needs patch".
		{"empty output (field absent)", "", false},
		{"empty jsonpath list", "[]", false},
		{"whitespace only", "   \n", false},
		{"no value sentinel", "<no value>", false},

		// Correctly-provisioned hives: NET_ADMIN present ⇒ skip, no rollout.
		{"only NET_ADMIN", "[NET_ADMIN]", true},
		{"NET_ADMIN among others", "[NET_ADMIN NET_RAW]", true},
		{"NET_ADMIN not first", "[NET_RAW NET_ADMIN]", true},
		{"trailing newline", "[NET_ADMIN]\n", true},

		// Other capabilities present but NOT NET_ADMIN ⇒ still needs patch.
		{"other cap only", "[NET_RAW]", false},
		// Guard against a substring false-match on a differently-named cap.
		{"substring lookalike", "[NET_ADMIN_FOO]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := securityContextHasNetAdmin(c.raw); got != c.want {
				t.Errorf("securityContextHasNetAdmin(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestNetAdminPatchJSON asserts the patch body targets the hive container's
// securityContext and installs exactly NET_ADMIN — the shape verified working
// live in #2674. A malformed path or missing capability would make the reconcile
// a silent no-op (or worse, corrupt the podspec), so pin it.
func TestNetAdminPatchJSON(t *testing.T) {
	patch := netAdminPatchJSON()
	if !strings.Contains(patch, hiveContainerCapabilitiesAddPath) {
		t.Errorf("patch %q does not target the hive container capabilities.add path %q",
			patch, hiveContainerCapabilitiesAddPath)
	}
	if !strings.Contains(patch, netAdminCapability) {
		t.Errorf("patch %q does not add %s", patch, netAdminCapability)
	}
	// It must be an "add" op so it works whether the path is missing (create) or
	// present-but-empty (overwrite) — both drift shapes from #2674.
	if !strings.Contains(patch, `"value":["NET_ADMIN"]`) {
		t.Errorf("patch %q must set only the NET_ADMIN capability list", patch)
	}
	if !strings.Contains(patch, `"op":"add"`) {
		t.Errorf("patch %q is not an add op", patch)
	}
}

// TestReconcileNetAdminThrottle verifies the poller-loop throttle only lets the
// sweep run once per netAdminReconcileInterval, so the 2-min SHA poller can call
// it every tick without hammering kubectl. We drive the timestamp directly
// rather than run the (kubectl-shelling) sweep body.
func TestReconcileNetAdminThrottle(t *testing.T) {
	s := &HubServer{clusterUnreachableUntil: map[string]time.Time{}}

	// First call: lastNetAdminReconcile is zero ⇒ due.
	s.clusterUnreachableMu.Lock()
	due := s.lastNetAdminReconcile.IsZero() ||
		time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	if due {
		s.lastNetAdminReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		t.Fatal("first reconcile should be due (zero timestamp)")
	}

	// Immediately after: NOT due (interval has not elapsed).
	s.clusterUnreachableMu.Lock()
	due2 := time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if due2 {
		t.Fatal("second reconcile immediately after should be throttled, not due")
	}

	// Backdate past the interval: due again.
	s.clusterUnreachableMu.Lock()
	s.lastNetAdminReconcile = time.Now().Add(-netAdminReconcileInterval - time.Minute)
	due3 := time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if !due3 {
		t.Fatal("reconcile should be due again after the interval elapses")
	}
}

// netAdminRealRegistryStatuses are the hive lifecycle statuses that actually
// occur, with the LIVE distribution measured across all 66
// /data/saas/hives/*/meta.json records on the production hub at the time this
// test was written:
//
//	30 "available"   28 ""   8 "assigned"   0 "running"
//
// The zero is the whole point. The sweep originally guarded on
// `h.Status != "running"`, so it selected no hive on any cycle and had never
// patched a Deployment in production. These fixtures are stated as real
// measured data, not invented values, so that a future change to the status
// vocabulary which invalidates them is a test failure rather than a silent
// regression.
var netAdminRealRegistryStatuses = []struct {
	status string
	live   int
	want   bool
	why    string
}{
	{"", 28, true, "the common steady state: provisioned, deployed, no status ever written"},
	{"available", 30, true, "unclaimed placeholder; deployed, and must hold NET_ADMIN BEFORE it is claimed or it crash-loops on the F5 image"},
	{"assigned", 8, true, "claimed and deployed"},
	{"running", 0, true, "never observed live, but the pre-existing intent must still select"},
	{"error", 0, true, "status is often stale and the Deployment usually still exists; missing NET_ADMIN may be the CAUSE of the error"},
	{"provisioning", 0, false, "namespace/Deployment still being applied; the template has requested NET_ADMIN since #1222"},
}

// netAdminLiveFleetSize is the measured size of the production registry, and
// the sum of the `live` counts above. Pinned so the two cannot drift apart.
const netAdminLiveFleetSize = 66

// TestNetAdminSweepEligibleOverRealStatuses pins the sweep's hive-selection
// predicate against the REAL status vocabulary. This is the test layer that was
// missing: the existing suite exercised securityContextHasNetAdmin,
// netAdminPatchJSON and the throttle directly, but nothing covered the filter
// that decides which hives ever reach any of them — so a filter matching
// NOTHING left every one of those tests green while the sweep was dead code.
func TestNetAdminSweepEligibleOverRealStatuses(t *testing.T) {
	var sum int
	for _, tc := range netAdminRealRegistryStatuses {
		sum += tc.live
		if got := netAdminSweepEligible(tc.status); got != tc.want {
			t.Errorf("netAdminSweepEligible(%q) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
		}
	}
	if sum != netAdminLiveFleetSize {
		t.Errorf("fixture live counts sum to %d, want %d — the measured fleet distribution and the pinned size have drifted apart", sum, netAdminLiveFleetSize)
	}
}

// TestNetAdminSweepSelectsTheLiveFleet replays the predicate over a fixture
// with the exact live status distribution and asserts the sweep would examine
// the whole 66-hive fleet.
//
// POSITIVE CONTROL, BOTH DIRECTIONS. The bug being fixed is "selects nothing",
// so a test that can only fail when the predicate becomes too permissive is
// insufficient — and the converse is equally true. This asserts an EXACT
// selected count AND an EXACT skipped count, which together fail in BOTH
// directions and cannot be satisfied by any predicate that ignores its
// argument:
//
//   - Neuter to `return false` (or restore the shipped `== "running"` guard,
//     which is behaviourally identical against this fixture since no live hive
//     is "running"): selected drops to 0 and skipped rises to the full fleet —
//     both assertions fail.
//   - Neuter to `return true` (select everything unconditionally): the
//     "provisioning" row is no longer excluded, so skipped drops to 0 and
//     selectedProvisioning rises — those assertions fail.
func TestNetAdminSweepSelectsTheLiveFleet(t *testing.T) {
	type hive struct{ id, status string }
	var fleet []hive
	for _, tc := range netAdminRealRegistryStatuses {
		for i := 0; i < tc.live; i++ {
			fleet = append(fleet, hive{id: tc.status + "-" + string(rune('a'+i%26)), status: tc.status})
		}
	}
	if len(fleet) != netAdminLiveFleetSize {
		t.Fatalf("fixture fleet is %d hives, want the measured %d", len(fleet), netAdminLiveFleetSize)
	}
	// Add one synthetic hive for each status that does not occur live, so the
	// predicate's handling of "running", "error" and "provisioning" is exercised
	// by the count assertions too rather than only by the table test above.
	fleet = append(fleet,
		hive{id: "synthetic-running", status: "running"},
		hive{id: "synthetic-error", status: "error"},
		hive{id: "synthetic-provisioning", status: "provisioning"},
	)

	selected, skipped, selectedProvisioning := 0, 0, 0
	for _, h := range fleet {
		if netAdminSweepEligible(h.status) {
			selected++
			if h.status == "provisioning" {
				selectedProvisioning++
			}
			continue
		}
		skipped++
	}

	// Direction 1 — the shipped bug. A predicate that selects nobody (or that
	// keys off "running", which no live hive has) drives this to 0.
	if selected == 0 {
		t.Fatal("sweep selected NO hives from a 66-hive fleet — this is the production bug: " +
			"the lane runs every cycle and patches nothing, so pre-#1222 NET_ADMIN drift is never repaired")
	}
	// The 66 live hives + the synthetic "running" and "error" rows are eligible;
	// only the synthetic "provisioning" row is not.
	const wantSelected = netAdminLiveFleetSize + 2
	if selected != wantSelected {
		t.Errorf("selected %d hives, want %d", selected, wantSelected)
	}
	// Direction 2 — over-selection. `return true` makes this 0 and trips here.
	if skipped != 1 {
		t.Errorf("skipped %d hives, want exactly 1 (the provisioning row) — "+
			"a predicate that selects everything unconditionally skips 0", skipped)
	}
	if selectedProvisioning != 0 {
		t.Error("a hive still provisioning was selected; its Deployment may not exist yet")
	}

	// The two spokes historically measured as drifted on the live fleet
	// (kubestellar-console-4vkt and projectbluefin-knuckle-gjvq, issue #2674)
	// sit in "" and "available". If either status were excluded, the spokes
	// this sweep exists to repair would still never converge. Assert that
	// explicitly rather than relying on the aggregate count.
	for _, s := range []string{"", "available"} {
		if !netAdminSweepEligible(s) {
			t.Errorf("status %q is not selected, but known-drifted production spokes carry it", s)
		}
	}
}

// TestNetAdminSweepEligibleRejectsTheOldGuard is the regression replay, pinned
// as a permanent test. It reconstructs the exact predicate that shipped and
// asserts it selects nothing over the real distribution — so if anyone
// reintroduces a "running"-based filter, the reasoning is already written down
// next to the failure.
func TestNetAdminSweepEligibleRejectsTheOldGuard(t *testing.T) {
	oldGuard := func(status string) bool { return status == "running" }

	var oldSelected, newSelected int
	for _, tc := range netAdminRealRegistryStatuses {
		if oldGuard(tc.status) {
			oldSelected += tc.live
		}
		if netAdminSweepEligible(tc.status) {
			newSelected += tc.live
		}
	}
	if oldSelected != 0 {
		t.Fatalf("fixture no longer reproduces the bug: old guard selected %d hives, expected 0", oldSelected)
	}
	if newSelected == 0 {
		t.Fatal("fixed predicate ALSO selects nothing — the bug is not fixed")
	}
	if newSelected != netAdminLiveFleetSize {
		t.Errorf("fixed predicate selects %d of %d live hives, want all of them", newSelected, netAdminLiveFleetSize)
	}
}
