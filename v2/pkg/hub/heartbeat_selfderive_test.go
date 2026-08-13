package hub

import (
	"log/slog"
	"testing"
)

// F2: the fleet-wide heartbeat bearer lane in verifyHeartbeatBearer does not
// bind identity, so any spoke can beat as any victim hive. Removing it requires
// that every spoke first present the per-hive bearer — historically blocked on
// "re-provision the fleet", which the operator has ruled out.
//
// These tests pin the property that makes an IN-PLACE cutover possible: the
// per-hive bearer is HMAC(master, info || 0x00 || hiveID), a pure function of
// material the spoke ALREADY HOLDS (HIVE_HUB_SECRET + HIVE_ID). A spoke can
// therefore migrate itself by rolling code, with no hub action and no
// re-provision.

// TestSpokeHeartbeatKeySelfDerivesPerHive is the F2 enabling property: a spoke
// holding the master and its own hive ID — but NO injected HIVE_HEARTBEAT_KEY —
// presents the identity-bound bearer rather than the fleet-wide one.
//
// This is exactly the configuration of the 3 live spokes measured with
// HIVE_HEARTBEAT_KEY absent. Before this change they presented the fleet-wide
// bearer and were the sole reason the legacy lane could not be deleted.
func TestSpokeHeartbeatKeySelfDerivesPerHive(t *testing.T) {
	const master = "test-master-secret-f2"
	const hiveID = "hive-alpha"

	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHeartbeatKey, "") // not injected — the laggard configuration
	t.Setenv(EnvHiveID, hiveID)

	got := SpokeHeartbeatKey()

	perHive := derivePerHiveKey(master, infoHeartbeatKey, hiveID)
	fleetWide := deriveDomainKey(master, infoHeartbeatKey)

	if got == fleetWide {
		t.Fatal("F2: spoke still presents the FLEET-WIDE bearer despite holding both " +
			"the master and its own hive ID — the legacy lane could never be deleted " +
			"without re-provisioning")
	}
	if got != perHive {
		t.Fatalf("spoke bearer = %q, want the self-derived per-hive value %q", got, perHive)
	}

	// And the hub accepts it as identity-bound, without any provisioning step.
	s := &HubServer{logger: slog.Default(), hubSecret: master}
	if !s.verifyHeartbeatBearer(got, hiveID) {
		t.Fatal("hub rejected the self-derived per-hive bearer — the in-place migration " +
			"would 401 the spoke")
	}
	if !s.heartbeatBearerIsPerHive(got, hiveID) {
		t.Fatal("self-derived bearer not reported as per-hive — rollout telemetry would " +
			"never reach 100% and the deletion precondition could not be met")
	}
}

// TestSpokeSelfDerivedBearerCannotImpersonateAnotherHive is the finding itself,
// asserted end-to-end through the spoke-side resolver: the bearer a spoke
// self-derives must authenticate ONLY that spoke.
//
// Without this, self-derivation could "succeed" while still handing out a
// credential that works fleet-wide — migrating the telemetry but not the
// vulnerability.
func TestSpokeSelfDerivedBearerCannotImpersonateAnotherHive(t *testing.T) {
	const master = "test-master-secret-f2"

	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHeartbeatKey, "")

	t.Setenv(EnvHiveID, "hive-attacker")
	attackerBearer := SpokeHeartbeatKey()

	s := &HubServer{logger: slog.Default(), hubSecret: master}

	// Positive control: the attacker CAN still beat as itself. Without this a
	// resolver that returned garbage for everything would pass the check below.
	if !s.verifyHeartbeatBearer(attackerBearer, "hive-attacker") {
		t.Fatal("positive control failed: a spoke could not authenticate as ITSELF, so " +
			"the rejection below proves nothing")
	}

	// The finding: it must NOT beat as the victim.
	if s.verifyHeartbeatBearer(attackerBearer, "hive-victim") {
		t.Fatal("F2: a self-derived bearer authenticated a heartbeat CLAIMING to be " +
			"another hive — the victim's key material would be delivered to the attacker")
	}
}

// TestSpokeInjectedKeyStillWinsOverSelfDerivation guards the ordering. 62 of 65
// live spokes already hold a hub-injected per-hive HIVE_HEARTBEAT_KEY; if
// self-derivation silently overrode it, a self-hosted spoke whose operator
// deliberately pinned a bearer would start presenting a different value and
// break. The injected value must remain authoritative.
func TestSpokeInjectedKeyStillWinsOverSelfDerivation(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-master-secret-f2")
	t.Setenv(EnvHiveID, "hive-alpha")
	t.Setenv(EnvHeartbeatKey, "explicitly-injected-bearer")

	if got := SpokeHeartbeatKey(); got != "explicitly-injected-bearer" {
		t.Fatalf("injected bearer = %q, want it to take precedence over self-derivation", got)
	}
}

// TestSpokeSelfDerivationRequiresBothInputs pins the fail-closed edges: with no
// master there is no bearer at all, and with no identity the spoke falls back to
// the fleet-wide value rather than deriving something empty or attacker-chosen.
func TestSpokeSelfDerivationRequiresBothInputs(t *testing.T) {
	const master = "test-master-secret-f2"

	// No master at all → no bearer. The spoke must fail closed, not present "".
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv(EnvHeartbeatKey, "")
	t.Setenv(EnvHiveID, "hive-alpha")
	if got := SpokeHeartbeatKey(); got != "" {
		t.Fatalf("with no master, spoke bearer = %q, want \"\" (fail closed)", got)
	}

	// Master but no identity → fleet-wide fallback, explicitly. This lane is what
	// the final deletion PR removes support for; it must be reachable ONLY here.
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHiveID, "")
	if got, want := SpokeHeartbeatKey(), deriveDomainKey(master, infoHeartbeatKey); got != want {
		t.Fatalf("with no hive ID, spoke bearer = %q, want the fleet-wide value %q", got, want)
	}
}

// TestFleetWideBearerStillAcceptedAtThisStage asserts the CURRENT stage
// explicitly, so the cutover status is unambiguous in the test suite rather than
// inferred. This PR does NOT delete the legacy lane — a spoke that has not yet
// rolled the self-derivation code still presents the fleet-wide bearer, and
// dropping it now would 401 those spokes.
//
// The deletion PR flips this test to assert rejection. If it starts failing
// because the lane was removed, that is the signal to check the precondition in
// AuthRolloutReadiness (heartbeat_lane_ready, 65/65) before flipping it.
func TestFleetWideBearerStillAcceptedAtThisStage(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubSecret: "test-master-secret-f2"}

	if !s.verifyHeartbeatBearer(s.heartbeatKey(), "hive-alpha") {
		t.Fatal("the fleet-wide lane was removed — every spoke that has not yet rolled " +
			"the self-derivation change will 401. Confirm AuthRolloutReadiness reports " +
			"heartbeat_lane_ready with 0 legacy hives before making this change.")
	}
	// It is still correctly flagged as NOT identity-bound, which is what drives
	// the readiness signal that gates the deletion.
	if s.heartbeatBearerIsPerHive(s.heartbeatKey(), "hive-alpha") {
		t.Fatal("fleet-wide bearer misreported as per-hive — readiness would read 100% " +
			"while spokes are still unbound, and the deletion would be made on false evidence")
	}
}
