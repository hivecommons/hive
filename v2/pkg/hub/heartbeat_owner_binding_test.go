package hub

import (
	"encoding/json"
	"testing"
)

// findRegistryOwner returns the stored Owner for a hive_id, or "" if absent.
func findRegistryOwner(s *HubServer, hiveID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			return h.Owner, true
		}
	}
	return "", false
}

// TestHeartbeatOwnerNotAssertableForExistingEntry is the C1/N3 authorization-
// binding regression (CWE-639). Owner is the principal every saas.go write
// handler checks (`h.Owner == username`). A heartbeat authenticates on the
// single fleet-shared bearer and then supplies hive_id + owner in its body, so a
// body-asserted Owner must NEVER overwrite the Owner of an existing registry
// entry — otherwise any hive could seize another hive's ownership by beating with
// its hive_id.
func TestHeartbeatOwnerNotAssertableForExistingEntry(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// A local (non-SaaS) hive already registered with Owner "alice".
	s.mu.Lock()
	s.registry.Hives = append(s.registry.Hives, RegistryEntry{
		ID:    "local-hive",
		Owner: "alice",
	})
	s.mu.Unlock()

	// An attacker beats as local-hive claiming to own it.
	body, _ := json.Marshal(HeartbeatPayload{HiveID: "local-hive", Owner: "attacker"})
	if rec := postHeartbeat(t, s, string(body)); rec.Code != 200 {
		t.Fatalf("heartbeat status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	got, ok := findRegistryOwner(s, "local-hive")
	if !ok {
		t.Fatal("local-hive vanished from the registry")
	}
	if got != "alice" {
		t.Fatalf("Owner = %q after a hostile heartbeat; the body must not rewrite an existing entry's Owner (want %q)", got, "alice")
	}
}

// TestHeartbeatOwnerFromAuthoritativeSaaSRecord confirms the positive half: for a
// hive with a SaaS record, the registry Owner is taken from that authoritative
// store, never from the heartbeat body.
func TestHeartbeatOwnerFromAuthoritativeSaaSRecord(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// Provisioned SaaS hive owned by "carol". saveSaaSHive writes to the temp
	// saasHivesDir set up by helperSetupTempDirs.
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-x", Owner: "carol", ClusterID: "hive-oke", Status: "running"}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	// The spoke beats claiming a different owner.
	body, _ := json.Marshal(HeartbeatPayload{HiveID: "hosted-x", Owner: "attacker"})
	if rec := postHeartbeat(t, s, string(body)); rec.Code != 200 {
		t.Fatalf("heartbeat status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	got, ok := findRegistryOwner(s, "hosted-x")
	if !ok {
		t.Fatal("hosted-x was not registered")
	}
	if got != "carol" {
		t.Fatalf("Owner = %q; must come from the authoritative SaaS record (want %q)", got, "carol")
	}
}
