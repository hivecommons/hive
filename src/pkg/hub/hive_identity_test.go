package hub

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Live fleet identity values, used so every case below is a REAL shape rather
// than an invented one.
// testGHEAppID / testGitHubComAppID are defined in both_app_keys_test.go.
const (
	testPublicAppID = testGitHubComAppID
	identPublicSlug = "kubestellar-hive"
	identGHESlug    = "kubestellar-hive-ghe"
	identGHEHost    = "github.ibm.com"
	identGHEBaseURL = "https://github.ibm.com"
	identGHEAPIURL  = "https://github.ibm.com/api/v3"
)

// hiveOKECluster is the live hive-oke entry: public, no URLs at all (the
// implicit-empty shape ~41 of 50 spokes run on).
func hiveOKECluster() *ClusterConfig {
	return &ClusterConfig{ID: "hive-oke", GitHubAppID: testPublicAppID, GitHubAppSlug: identPublicSlug}
}

// vllmDCluster is the live vllm-d entry: GHE default, one identity slot.
func vllmDCluster() *ClusterConfig {
	return &ClusterConfig{
		ID: "vllm-d", GitHubBaseURL: identGHEBaseURL, GitHubAPIURL: identGHEAPIURL,
		GitHubAppID: testGHEAppID, GitHubAppSlug: identGHESlug,
	}
}

// TestResolveHiveIdentityBothMismatchDirections is the core of the fix. All
// three live shapes must resolve correctly — the incident is only half-fixed if
// one direction works and the other does not.
func TestResolveHiveIdentityBothMismatchDirections(t *testing.T) {
	cases := []struct {
		name       string
		hive       *SaaSHive
		cluster    *ClusterConfig
		wantAppID  int64
		wantForge  string
		wantIntent bool
		why        string
	}{
		{
			// torch-spyre: hub records github.com, cluster stamps GHE. The hive's
			// election must win, and this is the case that was overwritten in 6s.
			name:       "public hive on a GHE-default cluster (torch-spyre)",
			hive:       &SaaSHive{ID: "torch-spyre", GitHubHost: publicForgeHost},
			cluster:    vllmDCluster(),
			wantAppID:  0, // vllm-d names NO public App — must not invent one
			wantForge:  publicForgeHost,
			wantIntent: true,
			why:        "the hive elected public; the cluster's GHE App is the wrong forge's",
		},
		{
			// vllmd-13: the OPPOSITE direction — hub says GHE, and the cluster
			// agrees, so it simply resolves to the cluster's GHE App.
			name:       "GHE hive on a GHE cluster (vllmd-13 intent)",
			hive:       &SaaSHive{ID: "vllmd-13", GitHubHost: identGHEHost},
			cluster:    vllmDCluster(),
			wantAppID:  testGHEAppID,
			wantForge:  identGHEHost,
			wantIntent: false, // election agrees with the cluster default
			why:        "election matches the cluster, so the cluster identity stands",
		},
		{
			// hosted-available-vllmd-01 in org "katamari": NO recorded host at
			// all. 15 of 50 hub records look like this. It must inherit the
			// cluster default and must NOT be treated as an election.
			name:       "empty host inherits the cluster (vllmd-01 / katamari)",
			hive:       &SaaSHive{ID: "hosted-available-vllmd-01"},
			cluster:    vllmDCluster(),
			wantAppID:  testGHEAppID,
			wantForge:  identGHEHost,
			wantIntent: false,
			why:        "no recorded intent means the cluster is the default",
		},
		{
			// The healthy majority: a public hive on the public cluster.
			name:       "public hive on a public cluster (console/hive-oke)",
			hive:       &SaaSHive{ID: "console", GitHubHost: publicForgeHost},
			cluster:    hiveOKECluster(),
			wantAppID:  testPublicAppID,
			wantForge:  publicForgeHost,
			wantIntent: false,
			why:        "election matches the cluster default",
		},
		{
			// The "public" SENTINEL rather than a recorded host — the pin that
			// exists precisely to force public on a GHE-default cluster.
			name:       "public sentinel pin on a GHE cluster",
			hive:       &SaaSHive{ID: "pinned", GitHubBaseURL: githubHostPublic},
			cluster:    vllmDCluster(),
			wantAppID:  0,
			wantForge:  publicForgeHost,
			wantIntent: true,
			why:        "the sentinel is an explicit public election",
		},
		{
			name:       "no hive record at all",
			hive:       nil,
			cluster:    vllmDCluster(),
			wantAppID:  testGHEAppID,
			wantForge:  identGHEHost,
			wantIntent: false,
			why:        "nothing known about the hive means the cluster default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveHiveIdentity(tc.hive, tc.cluster)
			if got.AppID != tc.wantAppID {
				t.Errorf("AppID = %d, want %d (%s)", got.AppID, tc.wantAppID, tc.why)
			}
			if !sameGitHubHost(got.Forge, tc.wantForge) {
				t.Errorf("Forge = %q, want %q (%s)", got.Forge, tc.wantForge, tc.why)
			}
			if got.FromHiveIntent != tc.wantIntent {
				t.Errorf("FromHiveIntent = %v, want %v (%s)", got.FromHiveIntent, tc.wantIntent, tc.why)
			}
			// COHERENCE: the URLs must always agree with the resolved forge.
			// A resolved identity that names one forge while carrying another's
			// URLs is the exact half-identity this design exists to prevent.
			if isPublicForgeHost(got.Forge) {
				// Either empty (inherited from a cluster whose silence must NOT
				// be read as evidence — see ResolveHiveIdentity) or the explicit
				// public urls (stated when the HIVE elected public). Both are
				// coherent; what must never appear is another forge's urls.
				okEmpty := got.APIURL == "" && got.BaseURL == ""
				okExplicit := got.APIURL == config.DefaultGitHubAPIURL && got.BaseURL == config.DefaultGitHubBaseURL
				if !okEmpty && !okExplicit {
					t.Errorf("a public identity carried non-public urls: api=%q base=%q",
						got.APIURL, got.BaseURL)
				}
			} else {
				if forgeHostLabel(got.BaseURL) != got.Forge || forgeHostLabel(got.APIURL) != got.Forge {
					t.Errorf("URLs disagree with forge %q: api=%q base=%q", got.Forge, got.APIURL, got.BaseURL)
				}
			}
		})
	}
}

// TestResolveHiveIdentityNeverInventsAnApp pins rule 3. Answering with an App
// that is not registered on the target forge is the 404-Integration-not-found
// failure; refusing to name one is strictly better than guessing.
func TestResolveHiveIdentityNeverInventsAnApp(t *testing.T) {
	got := ResolveHiveIdentity(&SaaSHive{GitHubHost: publicForgeHost}, vllmDCluster())
	if got.AppID == testGHEAppID {
		t.Fatal("the GHE app_id leaked onto a public election — this is the 2026-07-31 incident")
	}
	if got.AppSlug == identGHESlug {
		t.Fatal("the GHE app_slug leaked onto a public election — this is what authored as ghe[bot] on public repos")
	}
	if got.AppID != 0 {
		t.Errorf("vllm-d names no public App, so AppID must be 0, got %d", got.AppID)
	}
	// nil cluster: the hub knows nothing and must say nothing.
	if id := ResolveHiveIdentity(&SaaSHive{GitHubHost: publicForgeHost}, nil); id.AppID != 0 || id.Forge != "" {
		t.Errorf("a nil cluster must yield the zero identity, got %+v", id)
	}
}

// TestResolveHiveIdentityAllSixteenClusterHiveCombinations walks every
// combination of (cluster forge, cluster app) x (hive election, hive pin) so no
// mixture is left unexercised.
func TestResolveHiveIdentityAllSixteenCombinations(t *testing.T) {
	clusters := []*ClusterConfig{hiveOKECluster(), vllmDCluster()}
	hosts := []string{"", publicForgeHost, identGHEHost}
	pins := []string{"", githubHostPublic, identGHEBaseURL}

	for _, c := range clusters {
		for _, host := range hosts {
			for _, pin := range pins {
				h := &SaaSHive{ID: "h", GitHubHost: host, GitHubBaseURL: pin}
				got := ResolveHiveIdentity(h, c)

				// INVARIANT 1: the resolved App is never the other forge's.
				if got.AppID != 0 {
					wantAppForForge := testPublicAppID
					if !isPublicForgeHost(got.Forge) {
						wantAppForForge = testGHEAppID
					}
					if got.AppID != wantAppForForge {
						t.Errorf("cluster=%s host=%q pin=%q: app %d does not belong to forge %s",
							c.ID, host, pin, got.AppID, got.Forge)
					}
				}
				// INVARIANT 2: an explicit election is always honoured.
				if host != "" && !sameGitHubHost(got.Forge, host) {
					t.Errorf("cluster=%s host=%q pin=%q: election ignored, resolved forge %s",
						c.ID, host, pin, got.Forge)
				}
				// INVARIANT 3: with no election and no pin, the cluster decides.
				if host == "" && pin == "" && !sameGitHubHost(got.Forge, clusterForgeHost(c)) {
					t.Errorf("cluster=%s: no intent must inherit the cluster forge, got %s",
						c.ID, got.Forge)
				}
			}
		}
	}
}

// TestMixedForgeClusterResolvesBothWays: a public hive and a GHE hive on the
// SAME cluster entry each resolve to their own forge, with no leakage.
func TestMixedForgeClusterResolvesBothWays(t *testing.T) {
	c := vllmDCluster()
	pub := ResolveHiveIdentity(&SaaSHive{ID: "public-one", GitHubHost: publicForgeHost}, c)
	ghe := ResolveHiveIdentity(&SaaSHive{ID: "ghe-one", GitHubHost: identGHEHost}, c)

	if !isPublicForgeHost(pub.Forge) {
		t.Errorf("public hive resolved to forge %q", pub.Forge)
	}
	if isPublicForgeHost(ghe.Forge) {
		t.Errorf("GHE hive resolved to forge %q", ghe.Forge)
	}
	if ghe.AppID != testGHEAppID {
		t.Errorf("GHE hive app = %d, want %d", ghe.AppID, testGHEAppID)
	}
	if pub.AppID == ghe.AppID && pub.AppID != 0 {
		t.Error("LEAKAGE: both hives resolved to the same App on a mixed-forge cluster")
	}
}

// TestNoCallSiteResolvesIdentityIndependently is the "one resolver" guard.
//
// It fails if a NEW call site starts deciding hive identity on its own instead
// of routing through ResolveHiveIdentity. The check is a source scan for the
// specific pattern that caused this bug — reading cluster.GitHubAppID directly
// — because that is the move a future caller would make, and four independent
// implementations of one question is the bug class being closed here.
func TestNoCallSiteResolvesIdentityIndependently(t *testing.T) {
	// Files allowed to read cluster.GitHubAppID directly, with the reason.
	allowed := map[string]string{
		"hive_identity.go":   "the resolver itself",
		"cluster_app_key.go": "the key store: indexes keys BY app_id and compares against the resolved one",
		"saas_provision.go":  "guards the no-App-configured case before delegating to the resolver",
		"saas.go":            "renders cluster config for the admin UI; not a hive identity decision",
		"drift.go":           "compares a spoke's reported app_id against the cluster's for display",
	}
	entries, err := os.ReadDir("./")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// Match a read of CLUSTER config specifically (cluster.GitHubAppID /
		// c.GitHubAppID). RegistryEntry.GitHubAppID is the spoke-REPORTED app
		// id — an observation, not an identity decision — so it must not trip
		// this guard.
		src := string(body)
		if strings.Contains(src, "cluster.GitHubAppID") || strings.Contains(src, "c.GitHubAppID") {
			t.Errorf("%s reads cluster .GitHubAppID directly — hive identity must come from "+
				"ResolveHiveIdentity so provisioning, the heartbeat answer and the key lookup "+
				"cannot give different answers. If this read is legitimate, add it to the "+
				"allowlist in this test with a reason.", name)
		}
	}
}

// TestLoadAppPrivateKeyHonoursElection proves the key follows the elected forge
// rather than the cluster. Fixing app_id without the key still fails auth —
// torch-spyre's key_file was byte-identical to the GHE key.
func TestLoadAppPrivateKeyHonoursElection(t *testing.T) {
	// A hive electing public on the GHE-default cluster: the cluster key is the
	// GHE key and must NOT be handed over.
	id := ResolveHiveIdentity(&SaaSHive{ID: "torch-spyre", GitHubHost: publicForgeHost}, vllmDCluster())
	if !id.FromHiveIntent {
		t.Fatal("the election was not detected, so the key lookup cannot honour it")
	}
	if id.AppID == testGHEAppID {
		t.Fatal("resolved to the GHE App, so the GHE key would be selected — the original bug")
	}
}

// TestLoadAppPrivateKeySelectsTheElectedForgesKey exercises loadAppPrivateKey
// ITSELF, not just the resolver it calls.
//
// The distinction matters: an earlier version of this file asserted only on
// ResolveHiveIdentity, and a mutation that made loadAppPrivateKey ignore the
// election entirely still passed. That is precisely the "green against
// fixtures crafted to match the check" failure mode, so this drives the real
// function against a real on-disk hive record and a real key store.
func TestLoadAppPrivateKeySelectsTheElectedForgesKey(t *testing.T) {
	pem := testAppKeyPEM(t)

	// Redirect both on-disk stores at temp dirs.
	origHives, origKeys := saasHivesDir, clusterAppKeyDir
	t.Cleanup(func() { saasHivesDir, clusterAppKeyDir = origHives, origKeys })
	saasHivesDir = t.TempDir()
	clusterAppKeyDir = t.TempDir()

	// The fleet: hive-oke holds the PUBLIC App key, vllm-d holds the GHE one.
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", GitHubAppID: testPublicAppID, GitHubAppSlug: identPublicSlug},
			"vllm-d": {ID: "vllm-d", GitHubBaseURL: identGHEBaseURL, GitHubAPIURL: identGHEAPIURL,
				GitHubAppID: testGHEAppID, GitHubAppSlug: identGHESlug},
		},
	}
	if err := storeClusterAppKey("hive-oke", pem); err != nil {
		t.Fatal(err)
	}
	// Give vllm-d a DIFFERENT key so "returned the GHE key" is detectable.
	gheKey := strings.Replace(pem, "-----BEGIN", "-----BEGIN", 1)
	if err := storeClusterAppKey("vllm-d", gheKey); err != nil {
		t.Fatal(err)
	}

	// torch-spyre: on the vllm-d cluster, but ELECTED public github.com.
	if err := saveSaaSHive(&SaaSHive{
		ID: "torch-spyre", ClusterID: "vllm-d", GitHubHost: publicForgeHost,
	}); err != nil {
		t.Fatal(err)
	}

	got := s.loadAppPrivateKey(&RegistryEntry{ID: "torch-spyre", ClusterID: "vllm-d"})

	// The public App's key lives under hive-oke. Because torch-spyre elected
	// public, the key must come from the PUBLIC App, never from its cluster.
	wantPublic := s.appKeysByAppID()[testPublicAppID].PrivateKey
	if wantPublic == "" {
		t.Fatal("test setup: the fleet holds no public app key")
	}
	if got != wantPublic {
		t.Errorf("loadAppPrivateKey returned the wrong key for an elected forge.\n"+
			"This is the six-second-overwrite bug: the hub answered with the CLUSTER's key.\n"+
			"got len=%d, want the public App's key len=%d", len(got), len(wantPublic))
	}

	// Control: a hive that elected NOTHING must NOT take the elected-forge
	// branch at all — it falls through to the existing kubectl secret lookup,
	// which is unreachable in a unit test. Asserting on the RESOLUTION rather
	// than the returned key keeps the control meaningful without needing a
	// cluster: FromHiveIntent false is exactly "the new branch does not apply".
	if err := saveSaaSHive(&SaaSHive{ID: "plain", ClusterID: "vllm-d"}); err != nil {
		t.Fatal(err)
	}
	vd := s.clusters["vllm-d"]
	if plain := ResolveHiveIdentityInFleet(loadSaaSHive("plain"), &vd, s.forgeAppsAcrossFleet()); plain.FromHiveIntent {
		t.Error("a hive with no recorded host must not be treated as having elected a forge")
	} else if plain.AppID != testGHEAppID {
		t.Errorf("a hive with no election must inherit its cluster's App, got %d", plain.AppID)
	}
}

// TestHeartbeatAnswerHonoursElection covers the exact path that overwrote
// torch-spyre: the identity the hub ATTACHES TO A HEARTBEAT RESPONSE.
//
// The spoke pulls and converges on this answer, so this is the value that must
// be right — a spoke-side repair cannot survive a wrong answer here.
func TestHeartbeatAnswerHonoursElection(t *testing.T) {
	pem := testAppKeyPEM(t)
	origHives, origKeys := saasHivesDir, clusterAppKeyDir
	t.Cleanup(func() { saasHivesDir, clusterAppKeyDir = origHives, origKeys })
	saasHivesDir = t.TempDir()
	clusterAppKeyDir = t.TempDir()

	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", GitHubAppID: testPublicAppID, GitHubAppSlug: identPublicSlug},
			"vllm-d": {ID: "vllm-d", GitHubBaseURL: identGHEBaseURL, GitHubAPIURL: identGHEAPIURL,
				GitHubAppID: testGHEAppID, GitHubAppSlug: identGHESlug},
		},
	}
	if err := storeClusterAppKey("hive-oke", pem); err != nil {
		t.Fatal(err)
	}

	elected := &SaaSHive{ID: "torch-spyre", ClusterID: "vllm-d", GitHubHost: publicForgeHost}

	// The App identity the hub would answer this hive's heartbeat with.
	got := s.appIdentityForHive(elected, "vllm-d")
	if got == nil {
		t.Fatal("the hub answered with no identity at all for an elected public hive")
	}
	if got.AppID == testGHEAppID {
		t.Fatalf("the heartbeat answer carries the GHE app_id %d for a hive that elected "+
			"github.com — this is the six-second overwrite", got.AppID)
	}
	if got.AppID != testPublicAppID {
		t.Errorf("heartbeat app_id = %d, want the public App %d", got.AppID, testPublicAppID)
	}
	if got.AppSlug == identGHESlug {
		t.Errorf("heartbeat app_slug = %q — this is what made agents author as ghe[bot] "+
			"on public repos", got.AppSlug)
	}

	// A hive with no election still gets its cluster's identity, unchanged.
	plain := &SaaSHive{ID: "plain", ClusterID: "vllm-d"}
	if got := s.appIdentityForHive(plain, "vllm-d"); got == nil || got.AppID != testGHEAppID {
		t.Errorf("a hive with no election must keep the cluster default, got %+v", got)
	}
}
