package hub

import (
	"log/slog"
	"strings"
	"testing"
)

// N1 (CWE-200/639/862): the heartbeat bearer must be bound to the claimed hive.
//
// The bearer used to be checked against s.heartbeatKey() alone — one fleet-wide
// value derived from a FIXED label, so every spoke held identical bytes.
// Possession proved "some provisioned spoke" and nothing more, and the handler
// then trusted payload.HiveID verbatim. Three key-delivery lanes read off that
// claimed ID (pending OpenRouter key, legacy pending App config, standing
// cluster App key), so any spoke could beat as any victim and be handed the
// victim's key material. The code said so itself: "the handler trusts the
// body-supplied hive_id (all hives share one bearer)".

func n1Hub(t *testing.T) *HubServer {
	t.Helper()
	return &HubServer{logger: slog.Default(), hubSecret: "test-master-secret-n1"}
}

// TestHeartbeatBearerIsPerHive is the core property: the bearer a spoke is
// provisioned with authenticates ONLY that hive.
func TestHeartbeatBearerIsPerHive(t *testing.T) {
	s := n1Hub(t)

	alphaBearer := s.heartbeatKeyFor("hive-alpha")
	bravoBearer := s.heartbeatKeyFor("hive-bravo")

	if alphaBearer == "" || bravoBearer == "" {
		t.Fatal("per-hive bearer derivation returned empty")
	}
	if alphaBearer == bravoBearer {
		t.Fatal("two hives derived the same heartbeat bearer — identity cannot be bound")
	}

	// Its own hive: accepted.
	if !s.verifyHeartbeatBearer(alphaBearer, "hive-alpha") {
		t.Error("a hive's own per-hive bearer was rejected for its own ID")
	}
	// Someone else's hive: refused. This is the N1 IDOR.
	if s.verifyHeartbeatBearer(alphaBearer, "hive-bravo") {
		t.Fatal("N1: hive-alpha's bearer authenticated a heartbeat CLAIMING to be " +
			"hive-bravo — the victim's key material would be delivered to the attacker")
	}
}

// TestHeartbeatLegacyFleetBearerStillAccepted pins the deliberate rollout
// compatibility path. Every spoke in the field currently holds the fleet-wide
// bearer and will keep holding it until the hub re-provisions it; rejecting it
// outright would break every heartbeat in the fleet at once (the failure mode
// documented for the v2-hub/v4-spoke split in #2773).
func TestHeartbeatLegacyFleetBearerStillAccepted(t *testing.T) {
	s := n1Hub(t)
	legacy := s.heartbeatKey()

	if !s.verifyHeartbeatBearer(legacy, "hive-alpha") {
		t.Fatal("legacy fleet bearer rejected — this would break every spoke that has " +
			"not yet re-provisioned")
	}
	// And it is correctly reported as NOT the modern, identity-bound path, so
	// rollout telemetry can tell an operator when the legacy lane is safe to cut.
	if s.heartbeatBearerIsPerHive(legacy, "hive-alpha") {
		t.Error("legacy bearer misreported as per-hive — an operator would remove the " +
			"compatibility lane while spokes still depend on it")
	}
	if !s.heartbeatBearerIsPerHive(s.heartbeatKeyFor("hive-alpha"), "hive-alpha") {
		t.Error("per-hive bearer not reported as per-hive")
	}
}

// TestHeartbeatBearerRejectsGarbage covers the ordinary auth failures, so the
// dual-accept path cannot be mistaken for accept-anything.
func TestHeartbeatBearerRejectsGarbage(t *testing.T) {
	s := n1Hub(t)
	for _, bad := range []string{"", "not-a-key", strings.Repeat("a", 64)} {
		if s.verifyHeartbeatBearer(bad, "hive-alpha") {
			t.Errorf("verifyHeartbeatBearer accepted %q", bad)
		}
	}
}

// TestHeartbeatBearerFailsClosedWithoutIdentity: an empty hive ID must not
// derive a usable per-hive key. Otherwise every identity-less caller would share
// one key again — silently recreating the bug.
func TestHeartbeatBearerFailsClosedWithoutIdentity(t *testing.T) {
	s := n1Hub(t)
	if got := s.heartbeatKeyFor(""); got != "" {
		t.Errorf("empty hiveID derived %q, want \"\"", got)
	}
	// With no identity the per-hive branch cannot match, so only the legacy
	// bearer can authenticate — never an empty or arbitrary string.
	if s.verifyHeartbeatBearer("anything", "") {
		t.Error("verifyHeartbeatBearer accepted an arbitrary bearer for an empty hive ID")
	}
}

// TestProvisionHeartbeatKeyMatchesHubExpectation is the wiring check: what
// provisioning stamps into the spoke's Deployment must be exactly what the hub
// re-derives at verify time. If these ever diverge, every freshly provisioned
// spoke fails to heartbeat — a rollout-breaking bug that unit-testing the two
// halves separately would miss.
func TestProvisionHeartbeatKeyMatchesHubExpectation(t *testing.T) {
	const master = "test-master-secret-n1"
	t.Setenv("HIVE_HUB_SECRET", master)

	s := &HubServer{logger: slog.Default(), hubSecret: master}
	provisioned := provisionHeartbeatKey("hive-alpha")
	expected := s.heartbeatKeyFor("hive-alpha")

	if provisioned == "" {
		t.Fatal("provisionHeartbeatKey returned empty — the spoke would get no bearer")
	}
	if provisioned != expected {
		t.Fatalf("provisioning and verification disagree:\n  provisioned = %s\n  hub expects = %s\n"+
			"every newly provisioned spoke would fail to heartbeat", provisioned, expected)
	}
}

// TestNoAdditionalAppKeysInProvisionedManifest covers N1's second half: a spoke
// must receive ONLY its own App key.
//
// Provisioning used to render every OTHER App key in the fleet into each new
// hive's Secret, so one tenant's pod held other tenants' GitHub App private
// keys — and with the 0644 projection (N5, fixed in #3140) every agent UID in
// that pod could read them.
func TestNoAdditionalAppKeysInProvisionedManifest(t *testing.T) {
	m := renderManifest(t, false)

	// The per-app-id key entries are named gh-app-key-<appid>.pem. The hive's
	// OWN key is gh-app-key.pem (no numeric suffix) and must still be there.
	if strings.Contains(m, "gh-app-key-") {
		t.Fatal("N1: the rendered manifest still carries per-app-id keys for OTHER Apps — " +
			"one tenant's pod would hold other tenants' GitHub App private keys")
	}
}
