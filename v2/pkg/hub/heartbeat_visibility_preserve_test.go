package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestHeartbeatPreservesHubSetPrivate verifies that a hive whose operator set it
// PRIVATE in the SaaS store keeps that setting even though its spoke keeps
// reporting is_public: true on every heartbeat. This is the available-placeholder
// revert bug: pre-provisioned "Available slot" spokes report their provisioning
// default (public) forever, and the hub used to let that report overwrite the
// operator's private choice on the very next beat — then, because the registry
// value had been made to match the spoke, never sent the correction back.
//
// The fix anchors the registry entry's IsPublic to the SaaS store (the operator's
// source of truth) so the registry the dashboard reads stays private AND the hub
// pushes is_public=false back to the spoke via the heartbeat response.
func TestHeartbeatPreservesHubSetPrivate(t *testing.T) {
	dir := t.TempDir()
	origHives, origRegistry := saasHivesDir, registryPath
	saasHivesDir = filepath.Join(dir, "hives")
	registryPath = filepath.Join(dir, "hub-registry.json")
	t.Cleanup(func() { saasHivesDir, registryPath = origHives, origRegistry })

	const hiveID = "hosted-available-oke-260812-test-available-ok-1234"

	// Operator has set this available placeholder PRIVATE (is_public=false) in
	// the SaaS store — exactly what handleToggleVisibility persists.
	if err := saveSaaSHive(&SaaSHive{
		ID:       hiveID,
		Owner:    "clubanderson",
		Status:   statusAvailable,
		IsPublic: false,
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewHubServer(0, slog.Default(), "test", "v4")
	srv.setHubSecret("")

	// The placeholder spoke reports its provisioning default: is_public=true.
	body := `{"hive_id":"` + hiveID + `","org":"available-oke-260812-test","is_public":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// (a) The registry the dashboard reads must stay PRIVATE — the spoke's
	// public self-report must not revert the operator's setting.
	var gotEntry *RegistryEntry
	srv.mu.RLock()
	for i := range srv.registry.Hives {
		if srv.registry.Hives[i].ID == hiveID {
			gotEntry = &srv.registry.Hives[i]
			break
		}
	}
	srv.mu.RUnlock()
	if gotEntry == nil {
		t.Fatalf("hive %s missing from registry after heartbeat", hiveID)
	}
	if gotEntry.IsPublic {
		t.Errorf("registry IsPublic = true; operator-set PRIVATE was reverted to public by the spoke report")
	}

	// (b) The hub must push the authoritative is_public=false back to the spoke
	// in the heartbeat response so the spoke re-syncs (entry.IsPublic !=
	// payload.IsPublic). Before the fix, entry.IsPublic had been made equal to
	// the spoke's true, so no correction was sent.
	var resp HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	if resp.IsPublic == nil {
		t.Fatalf("response did not push authoritative visibility to the spoke")
	}
	if *resp.IsPublic {
		t.Errorf("response IsPublic = true; want false (the hub's authoritative value)")
	}
}

// TestHeartbeatPreservesHubSetPrivateOnFirstBeat covers the first-heartbeat path
// (no pre-existing registry row): a brand-new available placeholder that has
// never been in the registry, set private in the SaaS store, whose first beat
// reports public, must still land in the registry as private.
func TestHeartbeatPreservesHubSetPrivateOnFirstBeat(t *testing.T) {
	dir := t.TempDir()
	origHives, origRegistry := saasHivesDir, registryPath
	saasHivesDir = filepath.Join(dir, "hives")
	registryPath = filepath.Join(dir, "hub-registry.json")
	t.Cleanup(func() { saasHivesDir, registryPath = origHives, origRegistry })

	const hiveID = "hosted-available-oke-260812-fresh-available-ok-5678"
	if err := saveSaaSHive(&SaaSHive{
		ID:       hiveID,
		Owner:    "clubanderson",
		Status:   statusAvailable,
		IsPublic: false,
	}); err != nil {
		t.Fatal(err)
	}

	srv := NewHubServer(0, slog.Default(), "test", "v4")
	srv.setHubSecret("")

	body := `{"hive_id":"` + hiveID + `","org":"available-oke-260812-fresh","is_public":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	for i := range srv.registry.Hives {
		if srv.registry.Hives[i].ID == hiveID {
			if srv.registry.Hives[i].IsPublic {
				t.Errorf("first-beat registry IsPublic = true; operator-set PRIVATE not honored on initial registration")
			}
			return
		}
	}
	t.Fatalf("hive %s missing from registry after first heartbeat", hiveID)
}
