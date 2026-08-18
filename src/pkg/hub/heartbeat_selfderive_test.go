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

	// Master but no identity → the spoke still computes the fleet-wide value.
	// Post-F2 the HUB no longer accepts that value, so such a spoke fails closed at
	// verification rather than authenticating unbound. This asserts the spoke-side
	// resolver is unchanged and does not, say, emit "" or an attacker-chosen value.
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHiveID, "")
	if got, want := SpokeHeartbeatKey(), deriveDomainKey(master, infoHeartbeatKey); got != want {
		t.Fatalf("with no hive ID, spoke bearer = %q, want the fleet-wide value %q", got, want)
	}
}

// TestFleetWideBearerNowRejected is the flip the previous stage's
// TestFleetWideBearerStillAcceptedAtThisStage explicitly called for. That test
// asserted the fleet-wide lane MUST be accepted — an assertion that encoded the
// F2 vulnerability itself, so it is INVERTED rather than deleted: the file still
// documents the lane's status, and re-adding the lane now reddens CI.
//
// The gating precondition was measured across all three clusters before the
// deletion: 67 spokes hold an injected per-hive bearer, 0 hold the fleet-wide
// one, and the 3 with no injected key hold both HIVE_HUB_SECRET and HIVE_ID so
// they self-derive (see TestSpokeHeartbeatKeySelfDerivesPerHive).
func TestFleetWideBearerNowRejected(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubSecret: "test-master-secret-f2"}

	// Positive control first: a legitimate heartbeat still authenticates. A
	// verifier that rejected everything would otherwise "pass" the check below
	// while 401ing the entire fleet.
	if !s.verifyHeartbeatBearer(s.heartbeatKeyFor("hive-alpha"), "hive-alpha") {
		t.Fatal("positive control failed: a legitimate per-hive heartbeat was rejected, " +
			"so the rejection below proves nothing")
	}

	if s.verifyHeartbeatBearer(s.heartbeatKey(), "hive-alpha") {
		t.Fatal("F2: the fleet-wide bearer is still accepted. It is one value shared by " +
			"every spoke, so it cannot bind identity and any spoke may claim any hive_id.")
	}
	// Still correctly flagged as NOT identity-bound by the retained telemetry.
	if s.heartbeatBearerIsPerHive(s.heartbeatKey(), "hive-alpha") {
		t.Fatal("fleet-wide bearer misreported as per-hive")
	}
}
