package hub

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// App IDs from the live incident: the public github.com Hive App and the GHE App
// registered on the vllm-d cluster's github.ibm.com host.
const (
	testGitHubComAppID int64 = 3568013
	testGHEAppID       int64 = 5686
)

// storeKeyFor stores a freshly generated key for a cluster and returns its PEM
// and fingerprint. Uses the same temp-dir redirection every store test uses.
func storeKeyFor(t *testing.T, clusterID string) (pem string, fingerprint string) {
	t.Helper()
	pemData := testAppKeyPEM(t)
	if err := storeClusterAppKey(clusterID, pemData); err != nil {
		t.Fatalf("store key for %s: %v", clusterID, err)
	}
	fp, err := config.AppKeyFingerprint(pemData)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return pemData, fp
}

// twoClusterHub builds a hub with a github.com cluster and a GHE (vllm-d)
// cluster, each with its own App key stored, mirroring the live fleet. It
// returns the fingerprints and PEM bodies keyed by app_id so a test can assert
// which key material a given hive is (and is not) handed.
func twoClusterHub(t *testing.T) (s *HubServer, fps map[int64]string, pems map[int64]string) {
	t.Helper()
	withTempAppKeyDir(t)
	// The github.com App key lives on the public cluster; the GHE key on vllm-d.
	publicPEM, publicFP := storeKeyFor(t, "hive-oke")
	ghePEM, gheFP := storeKeyFor(t, "vllm-d")
	s = &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", GitHubAppID: testGitHubComAppID, GitHubAppSlug: "kubestellar-hive"},
			"vllm-d":   {ID: "vllm-d", GitHubAppID: testGHEAppID, GitHubAppSlug: "kubestellar-hive-ghe"},
		},
	}
	fps = map[int64]string{testGitHubComAppID: publicFP, testGHEAppID: gheFP}
	pems = map[int64]string{testGitHubComAppID: publicPEM, testGHEAppID: ghePEM}
	return s, fps, pems
}

// TestAppKeysByAppID indexes the per-cluster store by app_id and de-duplicates.
// appKeysByAppID is RETAINED after the C1/N3 fix — provisioning renders a single
// hive's own manifest from it — but the heartbeat path no longer enumerates
// other tenants' keys from it.
func TestAppKeysByAppID(t *testing.T) {
	s, fps, _ := twoClusterHub(t)

	byID := s.appKeysByAppID()
	if len(byID) != 2 {
		t.Fatalf("appKeysByAppID returned %d keys, want 2", len(byID))
	}
	for appID, wantFP := range fps {
		got, ok := byID[appID]
		if !ok {
			t.Fatalf("appKeysByAppID missing app_id %d", appID)
		}
		if got.Fingerprint != wantFP {
			t.Errorf("app_id %d fingerprint = %q, want %q", appID, got.Fingerprint, wantFP)
		}
		if got.PrivateKey == "" {
			t.Errorf("app_id %d has no private key", appID)
		}
	}

	t.Run("cluster with no app_id contributes nothing", func(t *testing.T) {
		s.clusters["nokey"] = ClusterConfig{ID: "nokey"} // GitHubAppID == 0
		if got := len(s.appKeysByAppID()); got != 2 {
			t.Fatalf("appKeysByAppID = %d keys after adding a keyless cluster, want 2", got)
		}
	})

	t.Run("two clusters sharing an app_id de-duplicate", func(t *testing.T) {
		// A second cluster fronting the SAME github.com App.
		storeKeyFor(t, "hive-oke-2")
		s.clusters["hive-oke-2"] = ClusterConfig{ID: "hive-oke-2", GitHubAppID: testGitHubComAppID}
		byID := s.appKeysByAppID()
		if _, ok := byID[testGitHubComAppID]; !ok {
			t.Fatal("shared app_id vanished after adding a second cluster for it")
		}
		// Still exactly two distinct app_ids.
		if len(byID) != 2 {
			t.Fatalf("appKeysByAppID = %d distinct app_ids, want 2", len(byID))
		}
	})
}

// TestHeartbeatDeliversOnlyCallerOwnKey is the C1/N3 regression guard. A
// heartbeat delivers ONLY the caller hive's own cluster App key — never any
// other tenant's key material. It replaces the former TestAttachMissingAppKeys /
// TestAdditionalAppKeysForSpoke suites, whose expectation (broadcast every other
// fleet key on every beat) WAS the vulnerability.
func TestHeartbeatDeliversOnlyCallerOwnKey(t *testing.T) {
	s, _, pems := twoClusterHub(t)

	t.Run("a hive on the GHE cluster receives ONLY the GHE key", func(t *testing.T) {
		payload := &HeartbeatPayload{HiveID: "vllmd-06", ClusterID: "vllm-d"}
		cfg := s.appKeySyncForHeartbeat(payload)
		if cfg == nil {
			t.Fatal("keyless spoke on vllm-d got no config; it must receive its own cluster key")
		}
		// Its own cluster (GHE) key is delivered as the PRIMARY key.
		if strings.TrimSpace(cfg.PrivateKey) != strings.TrimSpace(pems[testGHEAppID]) {
			t.Error("primary key delivered is not the caller's own GHE cluster key")
		}
		if cfg.AppID != testGHEAppID {
			t.Errorf("primary app_id = %d, want the caller's own cluster app_id %d", cfg.AppID, testGHEAppID)
		}
		// No other tenant's key may ride along. AdditionalKeys must always be nil
		// now: the broadcast lane is gone.
		if len(cfg.AdditionalKeys) != 0 {
			t.Fatalf("heartbeat carried %d additional keys; the cross-tenant broadcast must be gone", len(cfg.AdditionalKeys))
		}
		// Belt-and-braces: the OTHER tenant's PEM must never appear anywhere in the
		// wire config.
		assertNoKeyMaterialLeak(t, cfg, pems[testGitHubComAppID], "vllm-d hive")
	})

	t.Run("a hive on the github.com cluster receives ONLY the github.com key", func(t *testing.T) {
		payload := &HeartbeatPayload{HiveID: "oke-hive", ClusterID: "hive-oke"}
		cfg := s.appKeySyncForHeartbeat(payload)
		if cfg == nil {
			t.Fatal("keyless spoke on hive-oke got no config; it must receive its own cluster key")
		}
		if strings.TrimSpace(cfg.PrivateKey) != strings.TrimSpace(pems[testGitHubComAppID]) {
			t.Error("primary key delivered is not the caller's own github.com cluster key")
		}
		if len(cfg.AdditionalKeys) != 0 {
			t.Fatalf("heartbeat carried %d additional keys; the cross-tenant broadcast must be gone", len(cfg.AdditionalKeys))
		}
		assertNoKeyMaterialLeak(t, cfg, pems[testGHEAppID], "hive-oke hive")
	})
}

// TestHeartbeatCrossHiveImpersonationLeaksNoKey is the explicit impersonation
// regression: hive A, beating while asserting a DIFFERENT hive's / cluster's
// identity, must never be handed the other cluster's key material. The response
// is bound to the cluster whose key the caller is entitled to (its own), and the
// only key that can ride is that cluster's own key — the broadcast that formerly
// leaked every fleet key is gone.
func TestHeartbeatCrossHiveImpersonationLeaksNoKey(t *testing.T) {
	s, _, pems := twoClusterHub(t)

	// Hive A legitimately lives on the github.com cluster. It beats asserting the
	// github.com cluster (its own). It must receive its own key and NOTHING of the
	// GHE tenant's.
	aCfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{HiveID: "hive-A", ClusterID: "hive-oke"})
	if aCfg == nil {
		t.Fatal("hive-A got no config on its own cluster")
	}
	assertNoKeyMaterialLeak(t, aCfg, pems[testGHEAppID], "hive-A must not receive the GHE tenant key")

	// The GHE tenant's key can ONLY be produced when a caller is authoritatively
	// on the GHE cluster — i.e. beating as itself. There is no code path left that
	// hands the GHE key to a caller entitled only to the github.com key. Confirm
	// the GHE key is reachable solely for a GHE-cluster caller, and that a
	// github.com-cluster caller reporting the WRONG cluster still never receives
	// the other cluster's key beyond what that reported cluster entitles.
	bCfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{HiveID: "hive-B", ClusterID: "vllm-d"})
	if bCfg == nil || strings.TrimSpace(bCfg.PrivateKey) != strings.TrimSpace(pems[testGHEAppID]) {
		t.Fatal("a genuine GHE-cluster caller must still receive its own GHE key")
	}
	if len(bCfg.AdditionalKeys) != 0 {
		t.Fatalf("GHE caller carried %d additional keys; broadcast must be gone", len(bCfg.AdditionalKeys))
	}
	// And it must not also be handed the github.com tenant's key.
	assertNoKeyMaterialLeak(t, bCfg, pems[testGitHubComAppID], "GHE caller must not receive the github.com tenant key")
}

// assertNoKeyMaterialLeak fails if the forbidden PEM (another tenant's private
// key) appears anywhere in the delivered heartbeat App config — the primary key
// slot or any (now-always-empty) additional-key slot.
func assertNoKeyMaterialLeak(t *testing.T, cfg *HeartbeatGitHubAppConfig, forbiddenPEM, ctx string) {
	t.Helper()
	if cfg == nil {
		return
	}
	needle := strings.TrimSpace(forbiddenPEM)
	if needle == "" {
		t.Fatalf("%s: test bug — forbidden PEM is empty", ctx)
	}
	if strings.Contains(strings.TrimSpace(cfg.PrivateKey), needle) {
		t.Fatalf("%s: another tenant's key leaked into the primary key slot", ctx)
	}
	for _, ak := range cfg.AdditionalKeys {
		if strings.Contains(strings.TrimSpace(ak.PrivateKey), needle) {
			t.Fatalf("%s: another tenant's key leaked into an additional key (app_id %d)", ctx, ak.AppID)
		}
	}
}
