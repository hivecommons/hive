package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the project-identity carry-forward for DEGRADED beats.
//
// Three beat classes legitimately omit the project fields: the pre-restart
// "upgrading" beat (a minimal payload built outside the normal collect path,
// and the LAST beat the hub holds for the whole restart window that follows),
// the identity-only minimal liveness beat (fresh restart, collect timed out
// with no cache — marked stats_stale), and the upgrade-failure report. The
// registry entry is otherwise rebuilt from the payload VERBATIM, so those
// beats blanked org/primaryRepo/repos: the public-directory row lost its repo
// link (contributors had nothing to contribute to) and the hive rendered as
// "org/" until the next full collect landed. Observed live on the
// hivecommons/hive spoke, which auto-upgrades several times a day.

// degradedCarryTestServer isolates the SaaS store and registry on disk and
// returns a hub server with no bearer requirement, mirroring the other
// handleHeartbeat tests.
func degradedCarryTestServer(t *testing.T) *HubServer {
	t.Helper()
	dir := t.TempDir()
	origHives, origRegistry := saasHivesDir, registryPath
	saasHivesDir = filepath.Join(dir, "hives")
	registryPath = filepath.Join(dir, "hub-registry.json")
	t.Cleanup(func() { saasHivesDir, registryPath = origHives, origRegistry })

	srv := NewHubServer(0, slog.Default(), "test", "v4")
	t.Cleanup(srv.StopSaveLoop)
	srv.setHubSecret("")
	return srv
}

func postBeat(t *testing.T, srv *HubServer, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d (%s), want 200", w.Code, w.Body.String())
	}
}

func registryEntryByID(t *testing.T, srv *HubServer, hiveID string) RegistryEntry {
	t.Helper()
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	for i := range srv.registry.Hives {
		if srv.registry.Hives[i].ID == hiveID {
			return srv.registry.Hives[i]
		}
	}
	t.Fatalf("hive %s missing from registry", hiveID)
	return RegistryEntry{}
}

// TestHeartbeatUpgradingBeatCarriesProjectIdentityForward is the observed
// outage: the pre-restart upgrading beat omits primary_repo/repos, and the
// spoke then EXITS — so the blanked entry is what the hub serves for the whole
// restart window.
func TestHeartbeatUpgradingBeatCarriesProjectIdentityForward(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-upgrading-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"]}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","upgrading":true,"git_hash":"abc1234"}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.PrimaryRepo != "hive" {
		t.Errorf("PrimaryRepo = %q after upgrading beat, want %q carried forward — a blank primaryRepo breaks the public-directory repo link for the whole restart window", entry.PrimaryRepo, "hive")
	}
	if len(entry.Repos) != 1 || entry.Repos[0] != "hive" {
		t.Errorf("Repos = %v after upgrading beat, want [hive] carried forward", entry.Repos)
	}
	if entry.Name != "hivecommons/hive" {
		t.Errorf("Name = %q after upgrading beat, want %q — the name must be recomputed from the carried fields, not the payload's empty ones", entry.Name, "hivecommons/hive")
	}
	if !entry.Upgrading {
		t.Error("Upgrading = false; the carry-forward must not eat the beat's actual upgrade signal")
	}
}

// TestHeartbeatStatsStaleBeatCarriesProjectIdentityForward covers the minimal
// liveness beat: identity only, org included but repos historically absent
// (old spokes omit them from the published identity), marked stats_stale.
func TestHeartbeatStatsStaleBeatCarriesProjectIdentityForward(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-minimal-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"]}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","stats_stale":true}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.PrimaryRepo != "hive" || len(entry.Repos) != 1 {
		t.Errorf("after stats_stale beat PrimaryRepo = %q, Repos = %v; want hive/[hive] carried forward", entry.PrimaryRepo, entry.Repos)
	}
	if entry.Name != "hivecommons/hive" {
		t.Errorf("Name = %q, want hivecommons/hive", entry.Name)
	}
}

// TestHeartbeatUpgradeFailedBeatCarriesOrgForward covers ReportUpgradeFailure,
// whose payload is hive_id + SHAs + cause only: even the org is absent, so
// without carry-forward the entry's name degraded to "/".
func TestHeartbeatUpgradeFailedBeatCarriesOrgForward(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-upgradefailed-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"]}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","upgrade_failed":true,"upgrade_error":"image pull failed"}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.Org != "hivecommons" {
		t.Errorf("Org = %q after upgrade-failure beat, want hivecommons carried forward", entry.Org)
	}
	if entry.PrimaryRepo != "hive" || len(entry.Repos) != 1 {
		t.Errorf("PrimaryRepo = %q, Repos = %v after upgrade-failure beat; want hive/[hive] carried forward", entry.PrimaryRepo, entry.Repos)
	}
	if entry.Name != "hivecommons/hive" {
		t.Errorf("Name = %q, want hivecommons/hive", entry.Name)
	}
	if !entry.UpgradeFailed {
		t.Error("UpgradeFailed = false; the carry-forward must not eat the failure signal itself")
	}
}

// TestHeartbeatHealthyBeatStillOverwrites is the negative control: a full,
// non-degraded collect always wins, INCLUDING a genuine clear. Without this
// gate the carry-forward would pin a removed project forever.
func TestHeartbeatHealthyBeatStillOverwrites(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-healthy-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"]}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons"}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.PrimaryRepo != "" {
		t.Errorf("PrimaryRepo = %q after a healthy beat that cleared it, want empty — carry-forward must apply to degraded beats only", entry.PrimaryRepo)
	}
	if len(entry.Repos) != 0 {
		t.Errorf("Repos = %v after a healthy beat that cleared them, want empty", entry.Repos)
	}
}

// TestHeartbeatDegradedBeatUnderNewOrgDropsOldRepos: a reset/reassignment
// changes the org (a reset slot reports its synthetic "available-<id>" org).
// The previous tenant's repos must never survive into the new org's entry,
// even on a degraded beat.
func TestHeartbeatDegradedBeatUnderNewOrgDropsOldRepos(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-reset-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"hivecommons","primary_repo":"hive","repos":["hive"]}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"available-`+hiveID+`","stats_stale":true}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.PrimaryRepo != "" || len(entry.Repos) != 0 {
		t.Errorf("PrimaryRepo = %q, Repos = %v under a NEW org, want both empty — the old tenant's repos must not survive a reset", entry.PrimaryRepo, entry.Repos)
	}
	if entry.Org != "available-"+hiveID {
		t.Errorf("Org = %q, want the newly reported org", entry.Org)
	}
}
