package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The hub-side half of the write-path guard: appKeyConfigForHeartbeat must
// refuse to CONSTRUCT a push whose identity set is internally inconsistent.
//
// HeartbeatGitHubAppConfig carries app_id and app_slug and has no api_url
// field, so a clusters.json entry naming one forge's App beside another forge's
// URLs is not something the spoke can reassemble — it is exactly the delivery
// that half-applied onto seven hives on 2026-07-31.

// TestPushGuard_RefusesCrossForgeClusterIdentity pins the incident's clusters.json
// shape: a GHE app_id and slug on a cluster whose GitHub URLs are public
// (empty == api.github.com). Nothing may be pushed.
func TestPushGuard_RefusesCrossForgeClusterIdentity(t *testing.T) {
	withTempAppKeyDir(t)

	s := &HubServer{
		clusters: map[string]ClusterConfig{
			// The bad edit: the GHE App ID pasted onto a cluster that DECLARES
			// public github.com. Declaring the forge is what makes the two
			// halves comparable — see the gate in appKeyConfigForHeartbeat.
			"hive-oke": {
				ID:            "hive-oke",
				GitHubAppID:   config.EnterpriseGitHubAppID,
				GitHubAppSlug: config.EnterpriseGitHubAppSlug,
				GitHubAPIURL:  config.DefaultGitHubAPIURL,
				GitHubBaseURL: config.DefaultGitHubBaseURL,
			},
		},
		logger: appKeyTestLogger(),
	}

	got := s.appKeyConfigForHeartbeat("oke11", "hive-oke",
		"sha256:whatever", true, false, config.PlaceholderAppID, 0, s.logger)
	if got != nil {
		t.Fatalf("the hub CONSTRUCTED a cross-forge identity push %+v — this is the delivery that produced 404 Integration not found on seven hives", got)
	}
}

// TestPushGuard_HealthyClusterStillPushes is the negative control. The guard
// must not break the ordinary repair path: a consistent public cluster still
// delivers its App to a sentinel spoke.
func TestPushGuard_HealthyClusterStillPushes(t *testing.T) {
	withTempAppKeyDir(t)

	s := &HubServer{
		clusters: map[string]ClusterConfig{
			// Consistent public identity with EMPTY URLs — the live shape of
			// most clusters. If the guard rejects this, the fleet stops
			// self-repairing.
			"hive-oke": {
				ID:            "hive-oke",
				GitHubAppID:   config.PublicGitHubAppID,
				GitHubAppSlug: config.PublicGitHubAppSlug,
			},
		},
		logger: appKeyTestLogger(),
	}

	got := s.appKeyConfigForHeartbeat("oke11", "hive-oke",
		"sha256:whatever", true, false, config.PlaceholderAppID, 0, s.logger)
	if got == nil {
		t.Fatal("a consistent public cluster was refused — the guard broke the sentinel repair path")
	}
	if got.AppID != config.PublicGitHubAppID {
		t.Errorf("AppID = %d, want %d", got.AppID, config.PublicGitHubAppID)
	}
}

// TestPushGuard_HealthyGHEClusterStillPushes covers the other valid set: a GHE
// cluster that declares its URLs consistently.
func TestPushGuard_HealthyGHEClusterStillPushes(t *testing.T) {
	withTempAppKeyDir(t)

	s := &HubServer{
		clusters: map[string]ClusterConfig{
			"vllm-d": {
				ID:            "vllm-d",
				GitHubAppID:   config.EnterpriseGitHubAppID,
				GitHubAppSlug: config.EnterpriseGitHubAppSlug,
				GitHubAPIURL:  config.EnterpriseGitHubAPIURL,
				GitHubBaseURL: config.EnterpriseGitHubBaseURL,
			},
		},
		logger: appKeyTestLogger(),
	}

	got := s.appKeyConfigForHeartbeat("vd01", "vllm-d",
		"sha256:whatever", true, false, config.PlaceholderAppID, 0, s.logger)
	if got == nil {
		t.Fatal("a consistent GHE cluster was refused")
	}
	if got.AppID != config.EnterpriseGitHubAppID {
		t.Errorf("AppID = %d, want %d", got.AppID, config.EnterpriseGitHubAppID)
	}
}

// TestPushGuard_UnderSpecifiedClusterIsNotGated pins the fail-safe gate. Several
// real cluster records carry github_app_id and github_app_slug with NO forge
// URLs — vllm-d is one. Reading that silence as "public github.com" would refuse
// every such GHE cluster and stop the key/app_id repair path fleet-wide, which
// is the same outage this guard exists to prevent, pointed the other way.
func TestPushGuard_UnderSpecifiedClusterIsNotGated(t *testing.T) {
	withTempAppKeyDir(t)

	s := &HubServer{
		clusters: map[string]ClusterConfig{
			// The real vllm-d fixture shape: the GHE App, no declared URLs.
			"vllm-d": {
				ID:            "vllm-d",
				GitHubAppID:   config.EnterpriseGitHubAppID,
				GitHubAppSlug: "kubestellar-hive-ghe",
			},
		},
		logger: appKeyTestLogger(),
	}

	got := s.appKeyConfigForHeartbeat("vd01", "vllm-d",
		"sha256:whatever", true, false, config.PlaceholderAppID, 0, s.logger)
	if got == nil {
		t.Fatal("a cluster that declares no forge URLs was refused — silence is not evidence of public GitHub, and this stops the repair path on every un-backfilled GHE cluster")
	}
	if got.AppID != config.EnterpriseGitHubAppID {
		t.Errorf("AppID = %d, want %d", got.AppID, config.EnterpriseGitHubAppID)
	}
}

// TestPushGuard_UnknownAppIDIsNotGated keeps the guard from becoming a gate on
// deployments this build knows nothing about — a third forge, or a self-hosted
// App. Silence about an unknown App ID is the correct behaviour.
func TestPushGuard_UnknownAppIDIsNotGated(t *testing.T) {
	withTempAppKeyDir(t)

	const unknownAppID = 4242424
	s := &HubServer{
		clusters: map[string]ClusterConfig{
			"byo": {
				ID:            "byo",
				GitHubAppID:   unknownAppID,
				GitHubAppSlug: "someones-own-app",
				GitHubAPIURL:  "https://git.example.com/api/v3",
				GitHubBaseURL: "https://git.example.com",
			},
		},
		logger: appKeyTestLogger(),
	}

	got := s.appKeyConfigForHeartbeat("byo01", "byo",
		"sha256:whatever", true, false, config.PlaceholderAppID, 0, s.logger)
	if got == nil {
		t.Fatal("a self-hosted App was refused — the guard must claim nothing about App IDs it does not recognise")
	}
}
