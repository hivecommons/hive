package hub

import (
	"log/slog"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// The regression window: a GHE cluster written in the 2026-07-31 shape —
// `default_forge` + a `forges` map, with the flat github_base_url/github_api_url
// LEFT BLANK. Before this fix, clusterForgeHost and backfillGitHubHostFromCluster
// read only the flat fields, so such a cluster resolved to PUBLIC github.com and
// every hive on it that recorded no host got the public identity (app_id 0 /
// blank urls) — the EPM placeholder that claimed github.ibm.com yet ran a blank
// forge against github.com.
//
// vllmDForgeMapCluster is that shape. The identity lives only in the map.
func vllmDForgeMapCluster() *ClusterConfig {
	return &ClusterConfig{
		ID:           "vllm-d",
		DefaultForge: identGHEHost,
		Forges: map[string]ClusterForgeIdentity{
			identGHEHost: {AppID: testGHEAppID, AppSlug: identGHESlug},
		},
	}
}

// TestClusterDefaultForgeShapesAgree pins that the resolver's notion of a
// cluster's default forge matches clusterDefaultForge for BOTH shapes. The split
// between these two was the bug.
func TestClusterDefaultForgeShapesAgree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster *ClusterConfig
		want    string
	}{
		{"flat-field GHE cluster", vllmDCluster(), identGHEHost},
		{"forge-map GHE cluster (default_forge)", vllmDForgeMapCluster(), identGHEHost},
		{"public cluster", hiveOKECluster(), publicForgeHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterForgeHost(tc.cluster); !sameGitHubHost(got, tc.want) {
				t.Errorf("clusterForgeHost = %q, want %q", got, tc.want)
			}
			if got := clusterDefaultForge(tc.cluster); !sameGitHubHost(got, tc.want) {
				t.Errorf("clusterDefaultForge = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveHiveIdentityForgeMapShape is the core regression assertion: a hive
// with NO recorded host on a forge-map GHE cluster must resolve to the GHE App
// and GHE urls — never the public/blank identity that dropped it onto github.com.
func TestResolveHiveIdentityForgeMapShape(t *testing.T) {
	cluster := vllmDForgeMapCluster()

	t.Run("no recorded host inherits the GHE default (the EPM case)", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{ID: "hosted-available-vllmd-260731-19h2", Org: "epm"}, cluster)
		if got.AppID != testGHEAppID {
			t.Errorf("AppID = %d, want the GHE App %d — a blank-host hive on a forge-map GHE cluster must NOT resolve public", got.AppID, testGHEAppID)
		}
		if !sameGitHubHost(got.Forge, identGHEHost) {
			t.Errorf("Forge = %q, want %q", got.Forge, identGHEHost)
		}
		// The atomic set must be COMPLETE and coherent: GHE urls, never blank.
		if got.APIURL != identGHEAPIURL || got.BaseURL != identGHEBaseURL {
			t.Errorf("urls did not name the GHE forge: api=%q base=%q — a GHE default must never travel with blank/public urls", got.APIURL, got.BaseURL)
		}
		if got.AppSlug != identGHESlug {
			t.Errorf("AppSlug = %q, want %q", got.AppSlug, identGHESlug)
		}
	})

	t.Run("an explicit GHE election still resolves the GHE App", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{ID: "vllmd-x", GitHubHost: identGHEHost}, cluster)
		if got.AppID != testGHEAppID || !sameGitHubHost(got.Forge, identGHEHost) {
			t.Errorf("got app=%d forge=%q, want app=%d forge=%s", got.AppID, got.Forge, testGHEAppID, identGHEHost)
		}
	})

	t.Run("a public election on the forge-map GHE cluster stays public", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{ID: "elected-public", GitHubHost: publicForgeHost}, cluster)
		if !sameGitHubHost(got.Forge, publicForgeHost) || !got.FromHiveIntent {
			t.Errorf("got forge=%q intent=%v, want public election", got.Forge, got.FromHiveIntent)
		}
		// The forge-map cluster names no public App, so none is invented.
		if got.AppID != 0 {
			t.Errorf("AppID = %d, want 0 — this cluster names no public App to hand out", got.AppID)
		}
	})
}

// TestBackfillGitHubHostForgeMapShape proves the assign/approve-time backfill
// fills the GHE host for the forge-map shape too, so a fresh GHE claim never
// lands blank.
func TestBackfillGitHubHostForgeMapShape(t *testing.T) {
	cluster := vllmDForgeMapCluster()

	if got := backfillGitHubHostFromCluster(&SaaSHive{}, cluster); got != identGHEHost {
		t.Errorf("blank-host backfill = %q, want %q — a forge-map GHE cluster must backfill its GHE host", got, identGHEHost)
	}
	// The public sentinel still stays public on a forge-map GHE cluster.
	if got := backfillGitHubHostFromCluster(&SaaSHive{GitHubBaseURL: githubHostPublic}, cluster); got != "" {
		t.Errorf("public pin backfill = %q, want empty — an explicit public pin must stay public", got)
	}
	// And the backfilled host must actually drive a GHE API URL on the heartbeat.
	if got := gheAPIURLForHost(backfillGitHubHostFromCluster(&SaaSHive{}, cluster)); got != identGHEAPIURL {
		t.Errorf("gheAPIURLForHost = %q, want %q", got, identGHEAPIURL)
	}
}

// TestGuardGHEHiveNotOnPublicForge exercises the never-blank-for-GHE guard: a
// claimed GHE-cluster hive that is POSITIVELY reporting the public github.com
// forge is repaired by re-recording the cluster's GHE host, while every
// legitimate public case is left untouched.
func TestGuardGHEHiveNotOnPublicForge(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	newHub := func() *HubServer {
		return &HubServer{
			logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
			clusters: map[string]ClusterConfig{
				"hive-oke": *hiveOKECluster(),
				// The GHE cluster in the forge-map shape — the shape that hid the bug.
				"vllm-d": *vllmDForgeMapCluster(),
			},
		}
	}

	// A spoke report that names the public forge with the public app_id.
	publicReport := func(id string) *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID:       id,
			GitHubAPIURL: "", // blank api_url = api.github.com
			GitHubAppID:  config.PublicGitHubAppID,
		}
	}

	run := func(t *testing.T, hive *SaaSHive, payload *HeartbeatPayload) (bool, string) {
		t.Helper()
		if err := saveSaaSHive(hive); err != nil {
			t.Fatal(err)
		}
		s := newHub()
		changed := s.guardGHEHiveNotOnPublicForge(payload)
		got := ""
		if reloaded := loadSaaSHive(hive.ID); reloaded != nil {
			got = reloaded.GitHubHost
		}
		return changed, got
	}

	t.Run("claimed GHE hive running public is repaired to the GHE host", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "epm-blank", Status: statusAssigned, ClusterID: "vllm-d", Org: "epm", GitHubHost: ""},
			publicReport("epm-blank"),
		)
		if !changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q), want (true, %s)", changed, got, identGHEHost)
		}
	})

	t.Run("a persisted-but-public github_host on a GHE cluster is repaired", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "epm-public", Status: statusAssigned, ClusterID: "vllm-d", Org: "epm", GitHubHost: "github.com"},
			publicReport("epm-public"),
		)
		if !changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q), want (true, %s)", changed, got, identGHEHost)
		}
	})

	t.Run("an explicit public pin is never repaired", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "pinned", Status: statusAssigned, ClusterID: "vllm-d", GitHubBaseURL: githubHostPublic, GitHubHost: "github.com"},
			publicReport("pinned"),
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com) — a public pin is a decision", changed, got)
		}
	})

	t.Run("a GHE hive already recording the GHE host is a no-op (spoke just not converged)", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "converging", Status: statusAssigned, ClusterID: "vllm-d", GitHubHost: identGHEHost},
			publicReport("converging"),
		)
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q), want (false, %s)", changed, got, identGHEHost)
		}
	})

	t.Run("an in-flight forge switch is never raced", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "switching", Status: statusAssigned, ClusterID: "vllm-d", GitHubHost: "github.com", RequestedGitHubHost: identGHEHost},
			publicReport("switching"),
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com) — the switch owns the host", changed, got)
		}
	})

	t.Run("a public-cluster hive running public is correct and untouched", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "console", Status: statusAssigned, ClusterID: "hive-oke", GitHubHost: ""},
			publicReport("console"),
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — public cluster running public is fine", changed, got)
		}
	})

	t.Run("an unclaimed placeholder is skipped", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "placeholder", Status: statusAvailable, ClusterID: "vllm-d", GitHubHost: ""},
			publicReport("placeholder"),
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — assign owns a placeholder", changed, got)
		}
	})

	t.Run("a spoke too old to report (no app_id) is UNKNOWN, never a fault", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "old-spoke", Status: statusAssigned, ClusterID: "vllm-d", GitHubHost: ""},
			&HeartbeatPayload{HiveID: "old-spoke"}, // no api_url, no app_id
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — silence is not evidence", changed, got)
		}
	})

	t.Run("nil payload / nil server are safe", func(t *testing.T) {
		s := newHub()
		if s.guardGHEHiveNotOnPublicForge(nil) {
			t.Error("nil payload must be a no-op")
		}
		var nilServer *HubServer
		if nilServer.guardGHEHiveNotOnPublicForge(&HeartbeatPayload{HiveID: "x"}) {
			t.Error("nil server must be a no-op")
		}
	})
}

// TestGHEClaimDeliversCompleteAtomicSet proves the heartbeat App-config path
// delivers ALL FOUR forge fields — app_id, app_slug, api_url, base_url —
// together for a GHE claim, so a spoke stuck on the public app_id converges onto
// the complete GHE identity rather than a half-set. It runs against the forge-map
// cluster shape, the shape that used to resolve public.
func TestGHEClaimDeliversCompleteAtomicSet(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "vllm-d")

	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"vllm-d": *vllmDForgeMapCluster(),
		},
	}

	// A claimed GHE hive whose spoke still carries the PUBLIC app_id (a placeholder
	// provisioned public, now claimed for a github.ibm.com org).
	if err := saveSaaSHive(&SaaSHive{
		ID: "epm-claim", Status: statusAssigned, ClusterID: "vllm-d",
		Org: "epm", GitHubHost: identGHEHost,
	}); err != nil {
		t.Fatal(err)
	}
	payload := &HeartbeatPayload{
		HiveID:      "epm-claim",
		ClusterID:   "vllm-d",
		GitHubAppID: config.PublicGitHubAppID, // spoke still on the public App
	}

	cfg := s.appKeySyncForHeartbeat(payload)
	if cfg == nil {
		t.Fatal("no app config delivered — a spoke on the public app_id against a GHE claim must be corrected")
	}
	if cfg.AppID != testGHEAppID {
		t.Errorf("delivered app_id = %d, want the GHE App %d", cfg.AppID, testGHEAppID)
	}
	if cfg.AppSlug != identGHESlug {
		t.Errorf("delivered app_slug = %q, want %q", cfg.AppSlug, identGHESlug)
	}
	if cfg.APIURL != identGHEAPIURL {
		t.Errorf("delivered api_url = %q, want %q — the URL must travel WITH the App, never blank", cfg.APIURL, identGHEAPIURL)
	}
	if cfg.BaseURL != identGHEBaseURL {
		t.Errorf("delivered base_url = %q, want %q — the complete atomic set, never blank", cfg.BaseURL, identGHEBaseURL)
	}
	// The delivered set must itself be internally coherent (the write-path guard).
	if err := config.RejectIdentitySet(config.GitHubConfig{
		AppID: cfg.AppID, AppSlug: cfg.AppSlug, APIURL: cfg.APIURL, BaseURL: cfg.BaseURL,
	}); err != nil {
		t.Errorf("delivered a half-identity: %v", err)
	}
}

// TestPublicHiveStillFineWithBlankForge is the don't-break-public guard: a
// github.com hive on a public cluster, running the public App with blank urls —
// the ~48-of-51 steady state — must NOT be handed any forge push. Blank stays
// blank.
func TestPublicHiveStillFineWithBlankForge(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "hive-oke")

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"hive-oke": *hiveOKECluster()},
	}
	if err := saveSaaSHive(&SaaSHive{
		ID: "console", Status: statusAssigned, ClusterID: "hive-oke",
		Org: "kubestellar", GitHubHost: publicForgeHost,
	}); err != nil {
		t.Fatal(err)
	}
	// The spoke reports the public App and the cluster key fingerprint already —
	// fully converged. Nothing should be pushed.
	pem := loadClusterAppKey("hive-oke")
	clusterFP, err := config.AppKeyFingerprint(pem)
	if err != nil {
		t.Fatal(err)
	}
	payload := &HeartbeatPayload{
		HiveID:                  "console",
		ClusterID:               "hive-oke",
		GitHubAppID:             config.PublicGitHubAppID,
		GitHubAppKeyFingerprint: clusterFP,
	}
	if cfg := s.appKeySyncForHeartbeat(payload); cfg != nil {
		t.Errorf("a converged public hive was pushed a forge change: app_id=%d api_url=%q base_url=%q — blank must stay blank",
			cfg.AppID, cfg.APIURL, cfg.BaseURL)
	}
	// And the guard must never fire on it.
	if s.guardGHEHiveNotOnPublicForge(payload) {
		t.Error("the never-blank-for-GHE guard fired on a legitimate public hive")
	}
}

// TestForgeAppsAcrossFleetForgeMapShape proves a forge declared only in the
// `forges` map is discoverable fleet-wide, so a hive that elects it on a
// DIFFERENT cluster can still borrow the App.
func TestForgeAppsAcrossFleetForgeMapShape(t *testing.T) {
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		clusters: map[string]ClusterConfig{
			"hive-oke": *hiveOKECluster(),       // flat public App
			"vllm-d":   *vllmDForgeMapCluster(), // GHE App only in the forges map
		},
	}
	apps := s.forgeAppsAcrossFleet()
	if got := apps[publicForgeHost]; got.AppID != testPublicAppID {
		t.Errorf("public forge app = %d, want %d", got.AppID, testPublicAppID)
	}
	if got := apps[identGHEHost]; got.AppID != testGHEAppID {
		t.Errorf("GHE forge app = %d, want %d — a forges-map App must be discoverable fleet-wide", got.AppID, testGHEAppID)
	}
}
