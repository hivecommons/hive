package hub

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAssignTimeAppIdentityUnregisteredGHECluster reproduces the a-ks-wec2
// outage shape: five spokes assigned onto a GitHub Enterprise cluster that is
// ABSENT from clusters.json sat on config.PlaceholderAppID in dashboard-only
// mode, because appIdentityForHive returns nil for an unknown cluster before
// any App logic runs and assign had no other source of identity.
//
// The hive's elected forge is a fact independent of the cluster registry, and
// the GHE App is a build constant, so assign must still answer with the full
// identity set.
func TestAssignTimeAppIdentityUnregisteredGHECluster(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{
		"hive-oke": {ID: "hive-oke", InCluster: true, Domain: "hive.kubestellar.io"},
	}

	h := &SaaSHive{
		ID:         "hosted-available-akswec2-260810-hvam",
		Owner:      "clubanderson",
		Org:        "polaris-staging",
		Status:     "assigned",
		ClusterID:  "a-ks-wec2",
		GitHubHost: "github.ibm.com",
	}

	cfg := srv.assignTimeAppIdentity(h)
	if cfg == nil {
		t.Fatal("assignTimeAppIdentity returned nil for a GHE hive on an unregistered cluster: " +
			"the spoke keeps the placeholder app_id and never authenticates")
	}
	if cfg.AppID != config.EnterpriseGitHubAppID {
		t.Errorf("AppID = %d, want the enterprise App %d", cfg.AppID, config.EnterpriseGitHubAppID)
	}
	if cfg.AppID == config.PlaceholderAppID {
		t.Error("assign must never deliver the placeholder sentinel as a real App ID")
	}
	// An App ID presented to the wrong forge returns "404 Integration not
	// found", so the forge URLs must ride along with it.
	if cfg.APIURL != config.EnterpriseGitHubAPIURL {
		t.Errorf("APIURL = %q, want %q", cfg.APIURL, config.EnterpriseGitHubAPIURL)
	}
	if cfg.BaseURL != config.EnterpriseGitHubBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, config.EnterpriseGitHubBaseURL)
	}
	if cfg.AppSlug != config.EnterpriseGitHubAppSlug {
		t.Errorf("AppSlug = %q, want %q", cfg.AppSlug, config.EnterpriseGitHubAppSlug)
	}
	// installation_id is discovered by the spoke once it can authenticate as
	// the App; a non-zero value here would pin a guess, and requiring one is
	// what made the automatic path unreachable.
	if cfg.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 (spoke auto-discovers it)", cfg.InstallationID)
	}
}

// TestAssignTimeAppIdentityPublicForge covers the other build-constant forge and
// asserts the public case does NOT pin forge URLs — the spoke already defaults
// to public github.com, so writing them would state a value as an override.
func TestAssignTimeAppIdentityPublicForge(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{}

	h := &SaaSHive{
		ID:         "hosted-public-1",
		Owner:      "clubanderson",
		Org:        "kubestellar",
		Status:     "assigned",
		ClusterID:  "not-registered",
		GitHubHost: "github.com",
	}

	cfg := srv.assignTimeAppIdentity(h)
	if cfg == nil {
		t.Fatal("assignTimeAppIdentity returned nil for a public-github hive")
	}
	if cfg.AppID != config.PublicGitHubAppID {
		t.Errorf("AppID = %d, want the public App %d", cfg.AppID, config.PublicGitHubAppID)
	}
	if cfg.APIURL != "" || cfg.BaseURL != "" {
		t.Errorf("public forge must not pin URLs, got APIURL=%q BaseURL=%q", cfg.APIURL, cfg.BaseURL)
	}
}

// TestAssignTimeAppIdentityUnknownForgeSaysNothing guards the invention risk:
// a forge this build cannot name must yield nil so assign leaves the spoke
// alone rather than guessing an App ID that would 404 against that host.
func TestAssignTimeAppIdentityUnknownForgeSaysNothing(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{}

	h := &SaaSHive{
		ID:         "hosted-thirdparty-1",
		Owner:      "someone",
		Org:        "acme",
		Status:     "assigned",
		ClusterID:  "not-registered",
		GitHubHost: "github.acme.example",
	}

	if cfg := srv.assignTimeAppIdentity(h); cfg != nil {
		t.Errorf("expected nil for an unnameable forge, got AppID=%d", cfg.AppID)
	}
}

// TestAssignTimeAppIdentityRegisteredClusterWins keeps the cluster registry
// authoritative when it IS present: the builtin constants are a fallback for an
// unregistered cluster, never an override of a configured App.
func TestAssignTimeAppIdentityRegisteredClusterWins(t *testing.T) {
	const customAppID int64 = 4242
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{
		"custom": {
			ID:            "custom",
			Domain:        "custom.example",
			GitHubAppID:   customAppID,
			GitHubAppSlug: "custom-app",
			GitHubBaseURL: "https://github.ibm.com",
			GitHubAPIURL:  "https://github.ibm.com/api/v3",
		},
	}

	h := &SaaSHive{
		ID:         "hosted-custom-1",
		Owner:      "clubanderson",
		Org:        "team",
		Status:     "assigned",
		ClusterID:  "custom",
		GitHubHost: "github.ibm.com",
	}

	cfg := srv.assignTimeAppIdentity(h)
	if cfg == nil {
		t.Fatal("assignTimeAppIdentity returned nil for a registered cluster")
	}
	if cfg.AppID != customAppID {
		t.Errorf("AppID = %d, want the cluster's configured App %d (builtin must not override)", cfg.AppID, customAppID)
	}
}

// TestAppIdentityForHiveUnregisteredClusterRepairs covers the ONGOING repair
// path, not just assign. Delivery at assign is a single pending push consumed
// by one heartbeat; if it is missed, the every-beat reconcile is what must
// still converge. That reconcile runs through appIdentityForHive, which
// returned nil for an unknown cluster and produced the misleading
// "cluster names no github app — cannot repair" warning forever.
func TestAppIdentityForHiveUnregisteredClusterRepairs(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{
		"hive-oke": {ID: "hive-oke", InCluster: true, Domain: "hive.kubestellar.io"},
	}

	h := &SaaSHive{
		ID:         "hosted-available-akswec2-260810-hvam",
		Owner:      "clubanderson",
		Org:        "polaris-staging",
		Status:     "assigned",
		ClusterID:  "a-ks-wec2",
		GitHubHost: "github.ibm.com",
	}

	identity := srv.appIdentityForHive(h, clusterIDForHive(h))
	if identity == nil {
		t.Fatal("appIdentityForHive returned nil for a GHE hive on an unregistered cluster: " +
			"the every-beat repair can never converge and the spoke keeps the placeholder")
	}
	if identity.AppID != config.EnterpriseGitHubAppID {
		t.Errorf("AppID = %d, want %d", identity.AppID, config.EnterpriseGitHubAppID)
	}
	if identity.APIURL != config.EnterpriseGitHubAPIURL {
		t.Errorf("APIURL = %q, want %q", identity.APIURL, config.EnterpriseGitHubAPIURL)
	}
}

// TestAppIdentityForHiveUnregisteredClusterUnclaimedSaysNothing is the guard on
// the change above: the fallback keys off HIVE INTENT only. An unclaimed
// placeholder has elected nothing, so inferring a forge for it would be exactly
// the 2026-08-05 mistake (public hives flipped onto the GHE App).
func TestAppIdentityForHiveUnregisteredClusterUnclaimedSaysNothing(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{}

	h := &SaaSHive{
		ID:        "hosted-available-1",
		Owner:     hubAdminUsername,
		Status:    statusAvailable,
		ClusterID: "a-ks-wec2",
	}

	if identity := srv.appIdentityForHive(h, clusterIDForHive(h)); identity != nil {
		t.Errorf("unclaimed placeholder on an unregistered cluster must yield nil, got AppID=%d", identity.AppID)
	}
}
