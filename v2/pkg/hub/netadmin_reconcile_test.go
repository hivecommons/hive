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
	if !strings.Contains(patch, hiveContainerSecurityContextPath) {
		t.Errorf("patch %q does not target the hive container securityContext path %q",
			patch, hiveContainerSecurityContextPath)
	}
	if !strings.Contains(patch, netAdminCapability) {
		t.Errorf("patch %q does not add %s", patch, netAdminCapability)
	}
	// It must be an "add" op so it works whether the path is missing (create) or
	// present-but-empty (overwrite) — both drift shapes from #2674.
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
