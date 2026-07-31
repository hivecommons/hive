package hub

import (
	"log/slog"
	"testing"
)

// TestParseForgeTarget locks the validation contract: a forge is validated into
// a known kind, never accepted as an arbitrary string.
func TestParseForgeTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		kind    ForgeKind
		host    string
		apiURL  string
	}{
		{name: "public bare", in: "github.com", kind: ForgeGitHub, host: "github.com", apiURL: ""},
		{name: "public via URL", in: "https://github.com/", kind: ForgeGitHub, host: "github.com"},
		{name: "public via api host", in: "api.github.com", kind: ForgeGitHub, host: "github.com"},
		{name: "uppercase normalizes", in: "GitHub.COM", kind: ForgeGitHub, host: "github.com"},
		{
			name: "ghe bare", in: "github.ibm.com",
			kind: ForgeGitHubEnterprise, host: "github.ibm.com",
			apiURL: "https://github.ibm.com/api/v3",
		},
		{
			// The exact paste an operator is most likely to make — an org URL.
			// It must normalize to the same target as the bare host, not be
			// recorded verbatim and produce a spoke that talks to nothing.
			name: "ghe pasted org URL", in: "https://github.ibm.com/enricom-ibm",
			kind: ForgeGitHubEnterprise, host: "github.ibm.com",
			apiURL: "https://github.ibm.com/api/v3",
		},
		{
			name: "future ghe host", in: "github.cisco.com",
			kind: ForgeGitHubEnterprise, host: "github.cisco.com",
			apiURL: "https://github.cisco.com/api/v3",
		},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "whitespace rejected", in: "   ", wantErr: true},
		{name: "no dot rejected", in: "notahost", wantErr: true},
		{name: "space in host rejected", in: "git hub.com", wantErr: true},
		{name: "double dot rejected", in: "github..com", wantErr: true},
		{name: "trailing dot rejected", in: "github.com.", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForgeTarget(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseForgeTarget(%q) = %+v, want an error — arbitrary strings must not be accepted", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseForgeTarget(%q) unexpected error: %v", tc.in, err)
			}
			if got.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.Host != tc.host {
				t.Errorf("host = %q, want %q", got.Host, tc.host)
			}
			if got.APIURL != tc.apiURL {
				t.Errorf("api url = %q, want %q", got.APIURL, tc.apiURL)
			}
		})
	}
}

// TestForgeSwitchSurvivesHeartbeat is the whole point of the feature, and it is
// a REGRESSION TEST for a failure verified on the live fleet.
//
// github_host was edited to "github.ibm.com" in a hive's meta.json on the hub
// and the hub restarted. It did not stick: the spoke re-reported github.com on
// its next beat and the hub adopted it. A hub-side edit is provably
// insufficient, because the spoke is the one that reports the host and nothing
// the hub writes to meta changes what the spoke runs.
//
// The switch must therefore PUSH, and keep pushing until the spoke confirms.
func TestForgeSwitchSurvivesHeartbeat(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}

	// A hive on github.com that an operator has just switched to github.ibm.com:
	// the target is recorded and delivery is armed.
	saveSaaSHive(&SaaSHive{
		ID: "vllmd03", Status: "running", Org: "enricom-ibm",
		Repos: []string{"jackrabbit"}, PrimaryRepo: "jackrabbit", ACMMLevel: 2,
		GitHubHost: "github.ibm.com", RequestedGitHubHost: "github.ibm.com",
		ForgeDelivered: false,
	})

	// Beat 1: the spoke still reports the OLD host and the OLD api url. This is
	// exactly the state that silently reverted the manual edit. The hub must
	// push the enterprise API URL.
	got := projectConfigForHiveID("vllmd03", "enricom-ibm", []string{"jackrabbit"}, "jackrabbit", 2, "", "https://api.github.com")
	if got == nil {
		t.Fatal("no push for an undelivered forge switch — this is the exact production silence that reverted the manual edit")
	}
	if got.GitHubAPIURL != "https://github.ibm.com/api/v3" {
		t.Errorf("pushed GitHubAPIURL = %q, want the enterprise API", got.GitHubAPIURL)
	}

	// ...and the hub must NOT declare victory on the spoke's stale report.
	// Latching here would end the delivery with no evidence it worked.
	if s.adoptSpokeForge("vllmd03", "github.com") {
		t.Error("adopted delivery from a spoke still reporting the OLD host")
	}
	if h := loadSaaSHive("vllmd03"); h == nil || h.ForgeDelivered {
		t.Fatalf("ForgeDelivered must stay false while the spoke reports the old host, got %+v", h)
	}

	// Beat 2: the spoke has adopted the API URL. HostLabel() falls back to
	// api_url, so it now reports github.ibm.com — which is what closes the
	// read-back and stops the push for good.
	if !s.adoptSpokeForge("vllmd03", "github.ibm.com") {
		t.Fatal("spoke reported the requested host but delivery did not complete")
	}
	h := loadSaaSHive("vllmd03")
	if h == nil || !h.ForgeDelivered || h.GitHubHost != "github.ibm.com" {
		t.Fatalf("after delivery meta = %+v, want ForgeDelivered=true host=github.ibm.com", h)
	}

	// IDEMPOTENCE: the push stops permanently. Continuing to push would make
	// the forge un-editable on the spoke forever — the #2061 ACMM failure mode.
	if got := projectConfigForHiveID("vllmd03", "enricom-ibm", []string{"jackrabbit"}, "jackrabbit", 2, "", "https://github.ibm.com/api/v3"); got != nil {
		t.Errorf("delivered forge must never push again, got %+v", got)
	}
	if s.adoptSpokeForge("vllmd03", "github.ibm.com") {
		t.Error("re-adopting a delivered forge must be a no-op")
	}
}

// TestForgeSwitchBackToPublicIsDeliverable covers the direction that the
// existing GHE push could never handle.
//
// Everywhere else an empty github_api_url means "leave the spoke alone", and
// gheAPIURLForHost("github.com") is "" by definition — so a naive
// implementation can move a hive TO enterprise but never back. The public
// target is therefore delivered as an EXPLICIT api.github.com.
func TestForgeSwitchBackToPublicIsDeliverable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	saveSaaSHive(&SaaSHive{
		ID: "back", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 2,
		GitHubHost: "github.com", RequestedGitHubHost: "github.com",
		ForgeDelivered: false,
	})

	got := projectConfigForHiveID("back", "o", []string{"r"}, "r", 2, "", "https://github.ibm.com/api/v3")
	if got == nil {
		t.Fatal("switching back to public github.com produced no push — the hive would stay on the enterprise API forever")
	}
	if got.GitHubAPIURL != defaultPublicAPIURL {
		t.Errorf("pushed GitHubAPIURL = %q, want an explicit %q (empty would be ignored by the spoke)", got.GitHubAPIURL, defaultPublicAPIURL)
	}

	if !s.adoptSpokeForge("back", "github.com") {
		t.Fatal("spoke reported github.com but delivery did not complete")
	}
	if got := projectConfigForHiveID("back", "o", []string{"r"}, "r", 2, "", defaultPublicAPIURL); got != nil {
		t.Errorf("delivered public forge must never push again, got %+v", got)
	}
}

// TestForgeDeliveryIgnoresSilentSpoke locks "unknown is not a match".
//
// A spoke too old to report github_host sends "". Latching delivery on that
// would end the push with zero evidence the switch landed, leaving the hive on
// the old forge while the hub believes it moved — a silent failure, and the
// worst outcome available.
func TestForgeDeliveryIgnoresSilentSpoke(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	saveSaaSHive(&SaaSHive{
		ID: "old", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 2,
		GitHubHost: "github.ibm.com", RequestedGitHubHost: "github.ibm.com",
	})
	if s.adoptSpokeForge("old", "") {
		t.Error("an empty host report is UNKNOWN, not a delivery confirmation")
	}
	if h := loadSaaSHive("old"); h == nil || h.ForgeDelivered {
		t.Fatalf("silent spoke must not complete delivery, got %+v", h)
	}
}

// TestForgePushIsInertWithoutASwitch guarantees the feature is invisible to the
// ~every hive nobody has switched. Both new fields default to their zero values
// on every existing record, so the push must be gated on a NON-EMPTY request.
func TestForgePushIsInertWithoutASwitch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	// A perfectly ordinary delivered hive, as every record on the PVC looks today.
	saveSaaSHive(&SaaSHive{
		ID: "plain", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, RequestedACMMLevel: 3,
		ClaimDelivered: true, ACMMDelivered: true,
	})
	if got := projectConfigForHiveID("plain", "o", []string{"r"}, "r", 3, "", "https://api.github.com"); got != nil {
		t.Errorf("a hive with no forge switch must push nothing, got %+v", got)
	}
	if s.adoptSpokeForge("plain", "github.com") {
		t.Error("a hive with no forge switch must not record a delivery")
	}
	// And an unparseable stored target is never pushed rather than pushing junk.
	saveSaaSHive(&SaaSHive{
		ID: "junk", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, RequestedGitHubHost: "not a host",
	})
	if got := pendingForgeAPIURL(loadSaaSHive("junk"), ""); got != "" {
		t.Errorf("unparseable stored forge target must push nothing, got %q", got)
	}
}

// TestForgeAppIDComesFromClusterConfigOnly locks the credential half: no App ID
// is hardcoded in Go, and a cluster whose forge does not match the target must
// contribute nothing rather than pushing an App from the wrong forge.
func TestForgeAppIDComesFromClusterConfigOnly(t *testing.T) {
	s := &HubServer{logger: slog.Default()}

	// A github.com cluster (hive-oke shape): no GHE URLs, its own app id.
	const publicClusterApp int64 = 111
	publicCluster := &ClusterConfig{ID: "hive-oke", GitHubAppID: publicClusterApp}
	// An enterprise cluster (vllm-d shape): GHE base/api URLs, its own app id.
	const gheClusterApp int64 = 222
	gheCluster := &ClusterConfig{
		ID: "vllm-d", GitHubAppID: gheClusterApp,
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}

	public, err := parseForgeTarget("github.com")
	if err != nil {
		t.Fatal(err)
	}
	ghe, err := parseForgeTarget("github.ibm.com")
	if err != nil {
		t.Fatal(err)
	}

	if got := s.forgeAppIDForTarget(publicCluster, public); got != publicClusterApp {
		t.Errorf("github.com hive on a github.com cluster: app id = %d, want %d from cluster config", got, publicClusterApp)
	}
	if got := s.forgeAppIDForTarget(gheCluster, ghe); got != gheClusterApp {
		t.Errorf("GHE hive on a GHE cluster: app id = %d, want %d from cluster config", got, gheClusterApp)
	}
	// Cross-forge: the cluster's App is registered on the OTHER forge, so it
	// names nothing on the target. Zero = "say nothing", which leaves the
	// spoke's credentials untouched rather than pushing a wrong-forge App.
	if got := s.forgeAppIDForTarget(publicCluster, ghe); got != 0 {
		t.Errorf("github.com cluster must contribute no App for a GHE target, got %d", got)
	}
	if got := s.forgeAppIDForTarget(gheCluster, public); got != 0 {
		t.Errorf("GHE cluster must contribute no App for a github.com target, got %d", got)
	}
	// A cluster with no configured App, and a nil cluster, must both be silent.
	if got := s.forgeAppIDForTarget(&ClusterConfig{ID: "bare"}, public); got != 0 {
		t.Errorf("cluster with no github_app_id must contribute nothing, got %d", got)
	}
	if got := s.forgeAppIDForTarget(nil, public); got != 0 {
		t.Errorf("nil cluster must contribute nothing, got %d", got)
	}
}
