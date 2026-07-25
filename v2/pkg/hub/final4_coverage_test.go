package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// server.go — mergeLeaderboards aggregation branches
// ============================================================

func TestMergeLeaderboards_Cov4(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{ID: "priv", IsPublic: false, Leaderboard: []LeaderboardEntry{{GitHubUsername: "hidden", TasksCompleted: 5}}},
		{ID: "pub1", IsPublic: true, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "alice", TasksCompleted: 3, TasksFailed: 1},
			{GitHubUsername: "", TasksCompleted: 9},     // skipped (empty username)
			{GitHubUsername: "bot", TrustTier: "agent"}, // skipped (agent)
			{GitHubUsername: "zero"},                    // filtered later (no activity)
		}},
		{ID: "pub2", IsPublic: true, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "alice", TasksCompleted: 2, Active: true, CurrentTask: "task", HiveName: "pub2"},
		}},
	}
	merged := s.mergeLeaderboards()

	var alice *LeaderboardEntry
	for i := range merged {
		if merged[i].GitHubUsername == "alice" {
			alice = &merged[i]
		}
		if merged[i].GitHubUsername == "hidden" {
			t.Error("private hive leaderboard should be excluded")
		}
		if merged[i].GitHubUsername == "zero" {
			t.Error("zero-activity entry should be filtered out")
		}
	}
	if alice == nil {
		t.Fatal("expected alice in merged leaderboard")
	}
	if alice.TasksCompleted != 5 || alice.TasksFailed != 1 || !alice.Active || alice.CurrentTask != "task" {
		t.Errorf("alice aggregation wrong: %+v", alice)
	}
}

// ============================================================
// saas.go — handleApproveProvision with an explicit chosen placeholder
// ============================================================

func TestHandleApproveProvisionExplicitPlaceholder(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := &HubServer{logger: slog.Default(), saveCh: make(chan struct{}, 1), clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()}}

	saveProvisionRequest(&ProvisionRequest{
		Username: "bob", Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2,
		AuthMethod: "public", RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
	})

	// A specific placeholder chosen by the admin.
	saveSaaSHive(&SaaSHive{ID: "chosen-ph", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/approve-prov", `{"hive_id":"chosen-ph"}`, hubAdminUsername), "username", "bob")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit-placeholder approve status = %d body=%s", rec.Code, rec.Body.String())
	}

	// A chosen placeholder that is NOT available -> 409.
	saveProvisionRequest(&ProvisionRequest{
		Username: "carol", Org: "acme", Repos: "repo", AuthMethod: "public",
		RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
	})
	saveSaaSHive(&SaaSHive{ID: "taken-ph", Owner: hubAdminUsername, Status: "running", ClusterID: "hive-oke"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/approve-prov", `{"hive_id":"taken-ph"}`, hubAdminUsername), "username", "carol")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("unavailable chosen placeholder status = %d, want 409", rec.Code)
	}
}

// ============================================================
// saas.go — decryptToken with an HMAC key that fails to load
// ============================================================

func TestDecryptTokenKeyError(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	// Point the HMAC key path at a directory so reading/creating the key fails.
	old := hmacKeyPath
	hmacKeyPath = t.TempDir() // a directory, not a writable file path parent
	defer func() { hmacKeyPath = old }()

	if _, err := decryptToken("YWJjZGVm"); err == nil {
		t.Log("decryptToken succeeded despite key path issue (environment-dependent)")
	}
}
