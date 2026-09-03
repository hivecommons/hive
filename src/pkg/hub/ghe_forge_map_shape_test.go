package hub

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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

	// FORGE IS PER-HIVE (the 2026-08-05 regression). A CLAIMED hive that records
	// no host means public github.com — the field's documented meaning — and
	// must NOT inherit its cluster's GHE forge: vllm-d hosts github.com projects
	// (ibm/alchemy-logging) beside its github.ibm.com ones.
	t.Run("a CLAIMED hive with no recorded host is public, even on the GHE cluster (alchemy)", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{
			ID: "alchemy", Status: statusAssigned, Org: "ibm",
		}, cluster)
		if !sameGitHubHost(got.Forge, publicForgeHost) {
			t.Fatalf("Forge = %q, want public — a claimed hive's empty github_host means github.com, never the cluster's forge", got.Forge)
		}
		if got.AppID == testGHEAppID {
			t.Errorf("a claimed blank-host hive was handed the GHE App %d — the exact flip that degraded 9 spokes", testGHEAppID)
		}
		// The public election must carry EXPLICIT public urls so a spoke wrongly
		// sitting on GHE urls is moved off them (empty would mean "unchanged").
		if got.APIURL != config.DefaultGitHubAPIURL || got.BaseURL != config.DefaultGitHubBaseURL {
			t.Errorf("public election urls = api=%q base=%q, want explicit public urls", got.APIURL, got.BaseURL)
		}
	})

	t.Run("a CLAIMED hive recording github.com stays public on the GHE cluster", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{
			ID: "alchemy-explicit", Status: statusAssigned, Org: "ibm", GitHubHost: publicForgeHost,
		}, cluster)
		if !sameGitHubHost(got.Forge, publicForgeHost) || got.AppID == testGHEAppID {
			t.Errorf("got forge=%q app=%d — an explicit github.com host must never be overridden by the cluster", got.Forge, got.AppID)
		}
	})

	t.Run("an UNCLAIMED placeholder still inherits the cluster default", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{ID: "pool-1", Status: statusAvailable}, cluster)
		if got.AppID != testGHEAppID || !sameGitHubHost(got.Forge, identGHEHost) {
			t.Errorf("got app=%d forge=%q, want the cluster default — the placeholder pool serves the cluster's forge", got.AppID, got.Forge)
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
	if got := forgeAPIURLForHost("", backfillGitHubHostFromCluster(&SaaSHive{}, cluster)); got != identGHEAPIURL {
		t.Errorf("forgeAPIURLForHost = %q, want %q", got, identGHEAPIURL)
	}
}

// TestNoClusterKeyedForgeFlip pins the 2026-08-05 regression closed: a CLAIMED
// hive on a GHE-default cluster whose spoke reports the public forge must NEVER
// have its github_host rewritten to the cluster's forge, and must never be
// delivered the GHE App — unless its OWN recorded host is the GHE host. The
// vllm-d cluster hosts github.com projects (ibm/alchemy-logging) beside its
// github.ibm.com ones; cluster membership proves nothing about one hive.
func TestNoClusterKeyedForgeFlip(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "vllm-d")

	newHub := func() *HubServer {
		return &HubServer{
			logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
			clusters: map[string]ClusterConfig{
				"hive-oke": *hiveOKECluster(),
				"vllm-d":   *vllmDForgeMapCluster(),
			},
		}
	}

	// A healthy github.com spoke: public forge, public App.
	publicReport := func(id string) *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID:       id,
			ClusterID:    "vllm-d",
			GitHubAPIURL: "", // blank api_url = api.github.com
			GitHubAppID:  config.PublicGitHubAppID,
		}
	}

	for _, hive := range []*SaaSHive{
		// The alchemy class: claimed for a github.com org, host recorded public.
		{ID: "alchemy-explicit", Status: statusAssigned, ClusterID: "vllm-d", Org: "ibm", GitHubHost: "github.com"},
		// Same class, host never recorded — empty means public github.com.
		{ID: "alchemy-blank", Status: statusAssigned, ClusterID: "vllm-d", Org: "ibm", GitHubHost: ""},
	} {
		t.Run(hive.ID, func(t *testing.T) {
			if err := saveSaaSHive(hive); err != nil {
				t.Fatal(err)
			}
			s := newHub()
			payload := publicReport(hive.ID)

			// The full heartbeat repair surface must leave the record alone…
			s.reconcileGitHubHostFromSpoke(payload)
			s.repairMisflippedForgeFromRequest(payload)
			if got := loadSaaSHive(hive.ID); got == nil || got.GitHubHost != hive.GitHubHost {
				t.Fatalf("github_host was rewritten to %q — the cluster-keyed flip is back", got.GitHubHost)
			}
			// …and the identity delivery must not hand it the GHE App.
			if cfg := s.appKeySyncForHeartbeat(payload); cfg != nil && cfg.AppID == testGHEAppID {
				t.Fatalf("a github.com project on the GHE cluster was delivered the GHE App %d — the exact 9-spoke regression", testGHEAppID)
			}
		})
	}

	t.Run("REVERSE: a public-host hive whose spoke was flipped onto the GHE App is delivered its public identity back", func(t *testing.T) {
		if err := saveSaaSHive(&SaaSHive{
			ID: "alchemy-flipped", Status: statusAssigned, ClusterID: "vllm-d",
			Org: "ibm", GitHubHost: publicForgeHost,
		}); err != nil {
			t.Fatal(err)
		}
		s := newHub()
		// The 23:56Z state: the spoke adopted app 5686 + GHE urls, but kept its
		// old PUBLIC installation_id — which 404s under the GHE App.
		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:               "alchemy-flipped",
			ClusterID:            "vllm-d",
			GitHubAPIURL:         identGHEAPIURL,
			GitHubBaseURL:        identGHEBaseURL,
			GitHubAppID:          testGHEAppID,
			GitHubInstallationID: 146551814,
		})
		if cfg == nil || cfg.AppID != testPublicAppID {
			t.Fatalf("the mis-flipped spoke must be delivered its public App back, got %+v", cfg)
		}
		// EXPLICIT public urls: the spoke sits on GHE urls, and empty means
		// "unchanged" on the wire — the set would half-apply and be refused.
		if cfg.APIURL != config.DefaultGitHubAPIURL || cfg.BaseURL != config.DefaultGitHubBaseURL {
			t.Errorf("public restore urls = api=%q base=%q, want explicit public urls", cfg.APIURL, cfg.BaseURL)
		}
		// The app_id changes (GHE -> public), so the installation resets with it;
		// RediscoverAndAdopt then re-adopts the old public installation, which is
		// valid again under app %d.
		if !cfg.ResetInstallation || cfg.InstallationID != 0 {
			t.Errorf("reverse delivery must reset installation_id (got reset=%v id=%d)", cfg.ResetInstallation, cfg.InstallationID)
		}
	})

	t.Run("a GHE-host hive whose spoke runs public IS still repaired (EPM, delivery-side)", func(t *testing.T) {
		if err := saveSaaSHive(&SaaSHive{
			ID: "epm", Status: statusAssigned, ClusterID: "vllm-d", Org: "epm", GitHubHost: identGHEHost,
		}); err != nil {
			t.Fatal(err)
		}
		s := newHub()
		cfg := s.appKeySyncForHeartbeat(publicReport("epm"))
		if cfg == nil || cfg.AppID != testGHEAppID {
			t.Fatalf("a hive whose OWN host is GHE must still be moved off the public App, got %+v", cfg)
		}
		// The app_id changes, so the stale public installation_id must be reset
		// with it — it belongs to the previous App and would 404 under this one.
		if !cfg.ResetInstallation || cfg.InstallationID != 0 {
			t.Errorf("app-changing delivery must reset installation_id (got reset=%v id=%d)", cfg.ResetInstallation, cfg.InstallationID)
		}
	})
}

// TestRepairMisflippedForgeFromRequest exercises the restore for records the
// removed cluster-keyed guard STOMPED: hub meta and spoke both say GHE, the
// spoke is failing auth on it, and the hive's own provision request records
// public github.com — the one surviving record of the org's real host.
func TestRepairMisflippedForgeFromRequest(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	oldReqs := provisionRequestsDir
	provisionRequestsDir = t.TempDir()
	t.Cleanup(func() { provisionRequestsDir = oldReqs })

	newHub := func() *HubServer {
		return &HubServer{
			logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
			clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
		}
	}

	// The mis-flipped state: meta stomped to GHE, spoke adopted the GHE forge
	// and is dying on it (404 on token mint -> not-installed + banner).
	misflipped := func(id string) *SaaSHive {
		return &SaaSHive{
			ID: id, Status: statusAssigned, ClusterID: "vllm-d",
			Owner: "alchemist", Org: "ibm", GitHubHost: identGHEHost,
		}
	}
	brokenGHEReport := func(id string) *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID:            id,
			ClusterID:         "vllm-d",
			GitHubAPIURL:      identGHEAPIURL,
			GitHubBaseURL:     identGHEBaseURL,
			GitHubAppID:       testGHEAppID,
			GitHubAppRequired: true,
			GitHubAppState:    spokeAppStateNotInstalled,
		}
	}
	publicRequest := func(id string) *ProvisionRequest {
		return &ProvisionRequest{
			Username: "alchemist", Org: "ibm", GitHubHost: githubHostPublic,
			Status: provisionStatusApproved, AssignedHive: id,
		}
	}

	run := func(t *testing.T, h *SaaSHive, req *ProvisionRequest, payload *HeartbeatPayload) (bool, string) {
		t.Helper()
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
		if req != nil {
			if err := saveProvisionRequest(req); err != nil {
				t.Fatal(err)
			}
		}
		changed := newHub().repairMisflippedForgeFromRequest(payload)
		got := ""
		if reloaded := loadSaaSHive(h.ID); reloaded != nil {
			got = reloaded.GitHubHost
		}
		return changed, got
	}

	t.Run("the mis-flipped alchemy hive is restored to public", func(t *testing.T) {
		changed, got := run(t, misflipped("flip-1"), publicRequest("flip-1"), brokenGHEReport("flip-1"))
		if !changed || got != publicForgeHost {
			t.Fatalf("got (changed=%v, host=%q), want (true, %s)", changed, got, publicForgeHost)
		}
	})

	t.Run("a request naming the GHE host restores nothing (genuine GHE hive)", func(t *testing.T) {
		req := publicRequest("flip-2")
		req.GitHubHost = identGHEHost
		changed, got := run(t, misflipped("flip-2"), req, brokenGHEReport("flip-2"))
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q) — a GHE request must keep its GHE host", changed, got)
		}
	})

	t.Run("a silent (pre-host-field) request is UNKNOWN, never evidence", func(t *testing.T) {
		req := publicRequest("flip-3")
		req.GitHubHost = ""
		changed, got := run(t, misflipped("flip-3"), req, brokenGHEReport("flip-3"))
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q) — silence must not flip a hive public", changed, got)
		}
	})

	t.Run("a HEALTHY spoke on GHE is never touched, whatever the paperwork says", func(t *testing.T) {
		payload := brokenGHEReport("flip-4")
		payload.GitHubAppRequired = false
		payload.GitHubAppState = ""
		changed, got := run(t, misflipped("flip-4"), publicRequest("flip-4"), payload)
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q) — a working hive must not be flipped", changed, got)
		}
	})

	t.Run("an operator forge switch outranks the request record", func(t *testing.T) {
		h := misflipped("flip-5")
		h.ForgeDelivered = true // a completed deliberate switch to GHE
		changed, got := run(t, h, publicRequest("flip-5"), brokenGHEReport("flip-5"))
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q) — a completed switch is a decision", changed, got)
		}
	})

	t.Run("a request assigned to a DIFFERENT hive is no authority", func(t *testing.T) {
		req := publicRequest("some-other-hive")
		changed, got := run(t, misflipped("flip-6"), req, brokenGHEReport("flip-6"))
		if changed || got != identGHEHost {
			t.Errorf("got (changed=%v, host=%q) — another hive's request proves nothing here", changed, got)
		}
	})

	t.Run("nil payload / nil server are safe", func(t *testing.T) {
		s := newHub()
		if s.repairMisflippedForgeFromRequest(nil) {
			t.Error("nil payload must be a no-op")
		}
		var nilServer *HubServer
		if nilServer.repairMisflippedForgeFromRequest(&HeartbeatPayload{HiveID: "x"}) {
			t.Error("nil server must be a no-op")
		}
	})
}

// TestBrokenSpokeForgeReportNotAdopted pins the reconcile stand-down: a spoke
// FAILING App auth on the GHE forge it reports must not have that forge adopted
// into the hive record — adopting it would launder a mis-delivery into meta and
// fight the restore above.
func TestBrokenSpokeForgeReportNotAdopted(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
	}
	if err := saveSaaSHive(&SaaSHive{
		ID: "restored", Status: statusAssigned, ClusterID: "vllm-d",
		Owner: "alchemist", Org: "ibm", GitHubHost: publicForgeHost,
	}); err != nil {
		t.Fatal(err)
	}
	payload := &HeartbeatPayload{
		HiveID:            "restored",
		ClusterID:         "vllm-d",
		GitHubHost:        identGHEHost,
		GitHubAPIURL:      identGHEAPIURL,
		GitHubAppID:       testGHEAppID,
		GitHubAppRequired: true,
		GitHubAppState:    spokeAppStateNotInstalled,
	}
	if s.reconcileGitHubHostFromSpoke(payload) {
		t.Fatal("a failing spoke's forge report was adopted — the restore would be undone on the next beat")
	}
	if h := loadSaaSHive("restored"); h == nil || h.GitHubHost != publicForgeHost {
		t.Fatalf("github_host = %q, want %q", h.GitHubHost, publicForgeHost)
	}
	// The same report from a spoke that LOOKS healthy (fresh boot: no failure
	// reported yet) must not be adopted either — an explicit recorded host is
	// never overwritten from a spoke report. This is the second half of the
	// 2026-08-05 loop: the restored meta was re-stomped by a freshly-booted
	// spoke still carrying the mis-delivered GHE identity, and the wrong-forge
	// repair then re-delivered the GHE App against the re-stomped record.
	payload.GitHubAppRequired = false
	payload.GitHubAppState = ""
	payload.GitHubAppID = testGHEAppID
	payload.GitHubInstallationID = 146551814 // stale public id a pre-atomic-set flip left behind
	if s.reconcileGitHubHostFromSpoke(payload) {
		t.Fatal("a spoke report overwrote an explicit github_host — the hand-repair revert loop is back")
	}
	if h := loadSaaSHive("restored"); h == nil || h.GitHubHost != publicForgeHost {
		t.Fatalf("github_host = %q, want %q — the restored record must hold", h.GitHubHost, publicForgeHost)
	}
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
	// installation_id is PART of the atomic set: the delivery changes the app_id
	// (public -> GHE), so the spoke's public installation_id — valid only under
	// the public App — must be reset with it, never retained to 404 on
	// /app/installations/<id>/access_tokens.
	if !cfg.ResetInstallation {
		t.Error("app-changing delivery did not reset installation_id — the 2026-08-05 9-spoke 404")
	}
	if cfg.InstallationID != 0 {
		t.Errorf("app-changing delivery carried installation_id %d, want 0", cfg.InstallationID)
	}
}

// TestSameAppDeliveryPreservesInstallation is the other half of the atomic-set
// rule: a delivery that does NOT change the spoke's app_id (a key-only repair)
// must leave installation_id strictly alone.
func TestSameAppDeliveryPreservesInstallation(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "vllm-d")

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
	}
	if err := saveSaaSHive(&SaaSHive{
		ID: "ghe-keyless", Status: statusAssigned, ClusterID: "vllm-d",
		Org: "epm", GitHubHost: identGHEHost,
	}); err != nil {
		t.Fatal(err)
	}
	// The spoke already runs the RIGHT App but holds no key: a key-only push.
	cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
		HiveID:               "ghe-keyless",
		ClusterID:            "vllm-d",
		GitHubAppID:          testGHEAppID,
		GitHubInstallationID: 12345,
	})
	if cfg == nil {
		t.Fatal("expected a key delivery for a keyless spoke")
	}
	if cfg.ResetInstallation {
		t.Error("a same-app delivery reset installation_id — a key-only fault must never clear a working installation")
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
	// And no repair may touch its record.
	if s.repairMisflippedForgeFromRequest(payload) {
		t.Error("the mis-flip restore fired on a legitimate public hive")
	}
}

// TestStaleInstallationReset covers the self-heal for the genuine-GHE half of
// the 2026-08-05 fleet: spokes whose app identity is fully converged on the GHE
// App but whose installation_id still belongs to the PREVIOUS (public) App —
// the pre-atomic-set deliveries swapped app_id and left the old id in place, so
// every token mint 404s while every config field looks right.
func TestStaleInstallationReset(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
	}
	if err := saveSaaSHive(&SaaSHive{
		ID: "certus", Status: statusAssigned, ClusterID: "vllm-d",
		Org: "certus", GitHubHost: identGHEHost,
	}); err != nil {
		t.Fatal(err)
	}
	broken := func() *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID:               "certus",
			ClusterID:            "vllm-d",
			GitHubAPIURL:         identGHEAPIURL,
			GitHubBaseURL:        identGHEBaseURL,
			GitHubAppID:          testGHEAppID, // converged on the hub's own answer
			GitHubInstallationID: 146551814,    // the old PUBLIC App's installation
			GitHubAppRequired:    true,
			GitHubAppState:       spokeAppStateNotInstalled, // 404 on token mint
		}
	}

	cfg := s.appKeySyncForHeartbeat(broken())
	if cfg == nil || !cfg.ResetInstallation {
		t.Fatalf("converged-app + failing-auth + stale installation must deliver a reset, got %+v", cfg)
	}
	if cfg.AppID != 0 || cfg.PrivateKey != "" {
		t.Errorf("the reset must ride bare — app identity is already correct (got app_id=%d has_key=%v)", cfg.AppID, cfg.PrivateKey != "")
	}

	// Read-back idempotence: once the spoke reports installation 0, nothing more.
	confirmed := broken()
	confirmed.GitHubInstallationID = 0
	if cfg := s.appKeySyncForHeartbeat(confirmed); cfg != nil && cfg.ResetInstallation {
		t.Error("reset re-delivered after the spoke confirmed installation 0")
	}

	// A HEALTHY converged spoke is never reset, whatever id it holds.
	healthy := broken()
	healthy.GitHubAppRequired = false
	healthy.GitHubAppState = ""
	if cfg := s.appKeySyncForHeartbeat(healthy); cfg != nil && cfg.ResetInstallation {
		t.Error("a healthy spoke's installation was reset — positive failure evidence is required")
	}

	// An operator-armed reset outranks this path (delivered elsewhere).
	h := loadSaaSHive("certus")
	h.RequestedAppReset = true
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	if cfg := s.appKeySyncForHeartbeat(broken()); cfg != nil && cfg.ResetInstallation {
		t.Error("automatic reset fired while an operator reset owns the field")
	}
}

// TestWrongForgeRepairTargetsElectedForge pins the residual 2026-08-05 gate bug
// closed: the wrong-forge repair must deliver the App of the HIVE'S ELECTED
// forge, never compare the spoke against the CLUSTER's App.
//
// The live failure: six public hives (llm-d wva, llm-d-incubation, kellyaa,
// kalantar, ai-native, alchemy) had their hub meta correctly restored to
// github_host=github.com after the #2713 mis-flip, but their spokes stayed on
// the GHE cluster's own App 5686. The delivery gate compared the spoke's app_id
// against the cluster's (5686 == 5686) and stood down — zero re-deliveries
// across many heartbeats, until each spoke was hand-restored from its
// /data/hive.yaml.bak. The gate must instead compare against — and deliver —
// the elected forge's App (public 3568013), with #2732's installation-reset
// semantics (app change → ResetInstallation → rediscovery re-adopts the org's
// existing public installation).
func TestWrongForgeRepairTargetsElectedForge(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "vllm-d")

	// The 2026-08-05 spoke state: mis-flipped onto the GHE cluster's own App,
	// GHE urls adopted, old public installation_id still in place (it 404s
	// under App 5686).
	misflippedReport := func(id string) *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID:               id,
			ClusterID:            "vllm-d",
			GitHubAPIURL:         identGHEAPIURL,
			GitHubBaseURL:        identGHEBaseURL,
			GitHubAppID:          testGHEAppID,
			GitHubInstallationID: 146551814,
		}
	}
	publicHive := func(id string) *SaaSHive {
		return &SaaSHive{
			ID: id, Status: statusAssigned, ClusterID: "vllm-d",
			Org: "llm-d", GitHubHost: publicForgeHost,
		}
	}
	assertPublicRestore := func(t *testing.T, cfg *HeartbeatGitHubAppConfig) {
		t.Helper()
		if cfg == nil {
			t.Fatal("NO delivery — the exact live symptom: six restored hives sat on the wrong GHE identity across many heartbeats")
		}
		if cfg.AppID != testPublicAppID {
			t.Fatalf("delivered app_id = %d, want the elected forge's public App %d — never the cluster's %d", cfg.AppID, testPublicAppID, testGHEAppID)
		}
		// Explicit public urls: the spoke sits on GHE urls and empty means
		// "unchanged" on the wire.
		if cfg.APIURL != config.DefaultGitHubAPIURL || cfg.BaseURL != config.DefaultGitHubBaseURL {
			t.Errorf("restore urls = api=%q base=%q, want explicit public urls", cfg.APIURL, cfg.BaseURL)
		}
		// #2732's atomic-set rule: the app changes, so the stale id resets and
		// rediscovery re-adopts the org's existing public installation.
		if !cfg.ResetInstallation || cfg.InstallationID != 0 {
			t.Errorf("app-changing delivery must reset installation_id (got reset=%v id=%d)", cfg.ResetInstallation, cfg.InstallationID)
		}
	}

	// The exact live case, hardest form: NO cluster in the fleet names a public
	// App at all, so the elected forge's App can only come from the build's own
	// constant. Before the fix appIdentityForHive resolved AppID 0 → nil → the
	// repair was structurally unreachable.
	t.Run("public-elected hive mis-flipped onto the cluster App is restored (no public App in fleet)", func(t *testing.T) {
		s := &HubServer{
			logger:   appKeyTestLogger(),
			clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
		}
		if err := saveSaaSHive(publicHive("wva")); err != nil {
			t.Fatal(err)
		}
		assertPublicRestore(t, s.appKeySyncForHeartbeat(misflippedReport("wva")))
	})

	// The literal 5686 == 5686 gate: an under-specified cluster record (flat GHE
	// app_id, no urls, no default_forge) keys the GHE App under PUBLIC in the
	// fleet's forge map. The resolver then answered the hive's public election
	// with the GHE App itself, and the delivery gate compared the spoke's broken
	// App against the same broken App and stood down forever.
	t.Run("a leaked cluster App under the public forge label cannot dead-end the gate", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				// Sorts before vllm-d, so its poisoned public entry wins the map.
				"a-underspecified": {ID: "a-underspecified", GitHubAppID: testGHEAppID, GitHubAppSlug: identGHESlug},
				"vllm-d":           *vllmDForgeMapCluster(),
			},
		}
		if err := saveSaaSHive(publicHive("kellyaa")); err != nil {
			t.Fatal(err)
		}
		assertPublicRestore(t, s.appKeySyncForHeartbeat(misflippedReport("kellyaa")))
	})

	// Regression guard for #2732's original direction (the EPM class): a
	// GHE-elected hive whose spoke runs the public App must still be moved onto
	// the GHE identity — in the same minimal one-cluster fleet.
	t.Run("GHE-elected hive on the public App still gets the GHE identity", func(t *testing.T) {
		s := &HubServer{
			logger:   appKeyTestLogger(),
			clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "epm-guard", Status: statusAssigned, ClusterID: "vllm-d",
			Org: "epm", GitHubHost: identGHEHost,
		}); err != nil {
			t.Fatal(err)
		}
		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:               "epm-guard",
			ClusterID:            "vllm-d",
			GitHubAppID:          testPublicAppID,
			GitHubInstallationID: 146551814,
		})
		if cfg == nil || cfg.AppID != testGHEAppID {
			t.Fatalf("a GHE-elected hive on the public App must be delivered the GHE identity, got %+v", cfg)
		}
		if !cfg.ResetInstallation || cfg.InstallationID != 0 {
			t.Errorf("app-changing delivery must reset installation_id (got reset=%v id=%d)", cfg.ResetInstallation, cfg.InstallationID)
		}
	})

	// The healthy case stays untouched: a hive whose elected forge IS its
	// cluster's forge, converged on that App and key, gets nothing.
	t.Run("spoke on the cluster App of a cluster-forge hive is never touched", func(t *testing.T) {
		s := &HubServer{
			logger:   appKeyTestLogger(),
			clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "certus-healthy", Status: statusAssigned, ClusterID: "vllm-d",
			Org: "certus", GitHubHost: identGHEHost,
		}); err != nil {
			t.Fatal(err)
		}
		clusterFP, err := config.AppKeyFingerprint(loadClusterAppKey("vllm-d"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:                  "certus-healthy",
			ClusterID:               "vllm-d",
			GitHubAPIURL:            identGHEAPIURL,
			GitHubBaseURL:           identGHEBaseURL,
			GitHubAppID:             testGHEAppID,
			GitHubInstallationID:    777,
			GitHubAppKeyFingerprint: clusterFP,
		})
		if cfg != nil {
			t.Fatalf("a converged cluster-forge hive was pushed a change: %+v", cfg)
		}
	})

	// An App this build does not recognise is a deliberate pin, not evidence of
	// a wrong forge: silence never triggers the repair.
	t.Run("unknown spoke App stays protected as a deliberate pin", func(t *testing.T) {
		s := &HubServer{
			logger:   appKeyTestLogger(),
			clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
		}
		if err := saveSaaSHive(publicHive("visual")); err != nil {
			t.Fatal(err)
		}
		const thirdPartyAppID = 4240368 // daviddiaz0317-visual-hive: real fleet shape
		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:                  "visual",
			ClusterID:               "vllm-d",
			GitHubAppID:             thirdPartyAppID,
			GitHubInstallationID:    888,
			GitHubAppKeyPerHive:     true,
			GitHubAppKeyFingerprint: "sha256:cccccccccccccccccccccccccccccccc",
		})
		if cfg != nil {
			t.Fatalf("an unrecognised App was 'repaired' — unknown is never a mismatch: %+v", cfg)
		}
	})
}

func TestPlaceholderSentinelRepairsKnownClusterDefaultForgeWithoutConfiguredAppID(t *testing.T) {
	withTempAppKeyDir(t) // deliberately no stored key: this test is app_id-only repair.
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	t.Run("GHE default cluster with no github_app_id repairs sentinel to builtin GHE App", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				"a-ks-wec2": {ID: "a-ks-wec2", DefaultForge: identGHEHost},
			},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "ghe-default", Status: statusAssigned, ClusterID: "a-ks-wec2",
			Org: "z-innersource", GitHubHost: identGHEHost,
		}); err != nil {
			t.Fatal(err)
		}

		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:       "ghe-default",
			ClusterID:    "a-ks-wec2",
			GitHubHost:   identGHEHost,
			GitHubAPIURL: identGHEAPIURL, GitHubBaseURL: identGHEBaseURL,
			GitHubAppID: config.PlaceholderAppID,
		})
		if cfg == nil {
			t.Fatal("sentinel spoke got no repair; the default-forge builtin App rescue was not applied")
		}
		if cfg.AppID != testGHEAppID || cfg.AppSlug != identGHESlug {
			t.Fatalf("repair app = %d slug %q, want GHE app %d slug %q", cfg.AppID, cfg.AppSlug, testGHEAppID, identGHESlug)
		}
		if cfg.PrivateKey != "" {
			t.Errorf("no cluster key was configured, so repair must be app_id-only; got private key length %d", len(cfg.PrivateKey))
		}
		if cfg.APIURL != identGHEAPIURL || cfg.BaseURL != identGHEBaseURL {
			t.Errorf("repair urls = api=%q base=%q, want GHE urls", cfg.APIURL, cfg.BaseURL)
		}
	})

	t.Run("flat URL GHE default evidence also repairs sentinel without github_app_id", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				"legacy-ghe": {ID: "legacy-ghe", GitHubBaseURL: identGHEBaseURL, GitHubAPIURL: identGHEAPIURL},
			},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "legacy-ghe-default", Status: statusAssigned, ClusterID: "legacy-ghe",
			Org: "legacy", GitHubHost: identGHEHost,
		}); err != nil {
			t.Fatal(err)
		}

		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:      "legacy-ghe-default",
			ClusterID:   "legacy-ghe",
			GitHubHost:  identGHEHost,
			GitHubAppID: config.PlaceholderAppID,
		})
		if cfg == nil || cfg.AppID != testGHEAppID {
			t.Fatalf("flat URL GHE default repair = %+v, want App %d", cfg, testGHEAppID)
		}
	})

	t.Run("claimed public hive on a GHE default cluster is not flipped onto GHE", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				"vllm-d": {ID: "vllm-d", DefaultForge: identGHEHost},
			},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "alchemy-public", Status: statusAssigned, ClusterID: "vllm-d",
			Org: "ibm", GitHubHost: publicForgeHost,
		}); err != nil {
			t.Fatal(err)
		}

		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:               "alchemy-public",
			ClusterID:            "vllm-d",
			GitHubHost:           identGHEHost,
			GitHubAPIURL:         identGHEAPIURL,
			GitHubBaseURL:        identGHEBaseURL,
			GitHubAppID:          testGHEAppID,
			GitHubInstallationID: 146551814,
		})
		if cfg == nil {
			t.Fatal("public hive on the wrong GHE App must be restored to public, not left broken")
		}
		if cfg.AppID == testGHEAppID {
			t.Fatalf("public hive was flipped/re-delivered onto GHE App %d: %+v", testGHEAppID, cfg)
		}
		if cfg.AppID != testPublicAppID || cfg.APIURL != config.DefaultGitHubAPIURL || cfg.BaseURL != config.DefaultGitHubBaseURL {
			t.Fatalf("public hive repair = %+v, want public App %d and public urls", cfg, testPublicAppID)
		}
	})

	t.Run("unknown default forge with no App still invents nothing", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				"third-party": {ID: "third-party", DefaultForge: "git.example.com"},
			},
		}
		if err := saveSaaSHive(&SaaSHive{
			ID: "unknown-forge", Status: statusAssigned, ClusterID: "third-party",
			Org: "example", GitHubHost: "git.example.com",
		}); err != nil {
			t.Fatal(err)
		}

		cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:      "unknown-forge",
			ClusterID:   "third-party",
			GitHubHost:  "git.example.com",
			GitHubAppID: config.PlaceholderAppID,
		})
		if cfg != nil {
			t.Fatalf("unknown forge with no configured App was invented as a repair: %+v", cfg)
		}
	})

	t.Run("public pin still blocks a GHE identity when no hive election is available", func(t *testing.T) {
		s := &HubServer{
			logger: appKeyTestLogger(),
			clusters: map[string]ClusterConfig{
				"vllm-d": {ID: "vllm-d", DefaultForge: identGHEHost, GitHubAppID: testGHEAppID, GitHubAppSlug: identGHESlug},
			},
		}

		cfg := s.appKeyConfigForHeartbeat(
			"legacy-public-pin",
			"vllm-d",
			"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			false,
			true,
			testPublicAppID,
			0,
			appKeyTestLogger(),
			nil,
		)
		if cfg != nil {
			t.Fatalf("hivePublicPinned path delivered a GHE cluster identity: %+v", cfg)
		}
	})
}

// TestHandRepairDoesNotRevert pins the FULL 2026-08-05 feedback loop closed, as
// observed on hosted-kalantar-msb-soft-reflect-jsro:
//
//  1. operator restores hub meta github_host -> github.com and the spoke to the
//     public App;
//  2. the spoke reboots (or beats) still carrying — or re-delivered — the GHE
//     identity, and reports it BEFORE its first token mint fails, so it looks
//     healthy; reconcileGitHubHostFromSpoke re-stomped meta back to
//     github.ibm.com from that report;
//  3. with meta back at GHE and the spoke on the public App, the EPM-class
//     wrong-forge branch fired and re-delivered the GHE identity with an
//     installation reset.
//
// Every hand-repair reverted within minutes; all six restored metas were
// re-stomped within ~30 minutes. The loop must be broken at step 2 (a spoke
// report never overwrites an explicit meta host) while step 3's delivery — now
// derived from the intact meta — moves the SPOKE back to public instead.
func TestHandRepairDoesNotRevert(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	storeKeyFor(t, "vllm-d")

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"vllm-d": *vllmDForgeMapCluster()},
	}
	// The hand-restored record: meta explicitly public.
	if err := saveSaaSHive(&SaaSHive{
		ID: "kalantar", Status: statusAssigned, ClusterID: "vllm-d",
		Org: "kalantar", GitHubHost: publicForgeHost,
	}); err != nil {
		t.Fatal(err)
	}

	// Step 2's beat: a freshly-booted spoke still on the mis-delivered GHE
	// identity — GHE urls, the cluster's App, a leftover non-zero installation —
	// reporting NO failure yet (its first github call has not happened).
	freshBootGHE := &HeartbeatPayload{
		HiveID:               "kalantar",
		ClusterID:            "vllm-d",
		GitHubHost:           identGHEHost,
		GitHubAPIURL:         identGHEAPIURL,
		GitHubBaseURL:        identGHEBaseURL,
		GitHubAppID:          testGHEAppID,
		GitHubInstallationID: 146551814,
	}

	// The record must hold against the full heartbeat repair surface.
	if s.reconcileGitHubHostFromSpoke(freshBootGHE) {
		t.Fatal("a fresh-boot spoke report overwrote the hand-restored github_host — the revert loop's step 2")
	}
	s.repairMisflippedForgeFromRequest(freshBootGHE)
	if h := loadSaaSHive("kalantar"); h == nil || h.GitHubHost != publicForgeHost {
		t.Fatalf("github_host = %q, want %q — the hand-repair reverted", h.GitHubHost, publicForgeHost)
	}

	// And the SAME beat's delivery must move the spoke back to public — never
	// re-deliver the GHE identity (the loop's step 3).
	cfg := s.appKeySyncForHeartbeat(freshBootGHE)
	if cfg == nil {
		t.Fatal("no delivery — the mis-flipped spoke must be moved back onto the public App")
	}
	if cfg.AppID == testGHEAppID {
		t.Fatalf("the GHE identity was re-delivered against a public meta — the loop's step 3: %+v", cfg)
	}
	if cfg.AppID != testPublicAppID || !cfg.ResetInstallation || cfg.InstallationID != 0 {
		t.Fatalf("want the public App %d with an installation reset, got %+v", testPublicAppID, cfg)
	}

	// Once the spoke converges on the public identity, everything goes quiet:
	// no further delivery, and its public report never touches the record.
	converged := &HeartbeatPayload{
		HiveID:               "kalantar",
		ClusterID:            "vllm-d",
		GitHubAppID:          testPublicAppID,
		GitHubInstallationID: 146551814, // rediscovery re-adopted the old public installation
	}
	if s.reconcileGitHubHostFromSpoke(converged) {
		t.Error("a converged public report rewrote the record")
	}
	if cfg := s.appKeySyncForHeartbeat(converged); cfg != nil && (cfg.AppID != 0 || cfg.ResetInstallation) {
		t.Errorf("a converged spoke was pushed an identity change: %+v", cfg)
	}
	if h := loadSaaSHive("kalantar"); h == nil || h.GitHubHost != publicForgeHost {
		t.Fatalf("github_host = %q after convergence, want %q", h.GitHubHost, publicForgeHost)
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
