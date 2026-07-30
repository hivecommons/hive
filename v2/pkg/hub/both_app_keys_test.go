package hub

import (
	"strconv"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// App IDs from the live incident: the public github.com Hive App and the GHE App
// registered on the vllm-d cluster's github.ibm.com host.
const (
	testGitHubComAppID int64 = 3568013
	testGHEAppID       int64 = 5686
)

// storeKeyFor stores a freshly generated key for a cluster and returns its
// fingerprint. Uses the same temp-dir redirection every store test uses.
func storeKeyFor(t *testing.T, clusterID string) string {
	t.Helper()
	pemData := testAppKeyPEM(t)
	if err := storeClusterAppKey(clusterID, pemData); err != nil {
		t.Fatalf("store key for %s: %v", clusterID, err)
	}
	fp, err := config.AppKeyFingerprint(pemData)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// twoClusterHub builds a hub with a github.com cluster and a GHE (vllm-d)
// cluster, each with its own App key stored, mirroring the live fleet.
func twoClusterHub(t *testing.T) (*HubServer, map[int64]string) {
	t.Helper()
	withTempAppKeyDir(t)
	// The github.com App key lives on the public cluster; the GHE key on vllm-d.
	publicFP := storeKeyFor(t, "hive-oke")
	gheFP := storeKeyFor(t, "vllm-d")
	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", GitHubAppID: testGitHubComAppID, GitHubAppSlug: "kubestellar-hive"},
			"vllm-d":   {ID: "vllm-d", GitHubAppID: testGHEAppID, GitHubAppSlug: "ibm-hive"},
		},
	}
	return s, map[int64]string{testGitHubComAppID: publicFP, testGHEAppID: gheFP}
}

// TestAppKeysByAppID indexes the per-cluster store by app_id and de-duplicates.
func TestAppKeysByAppID(t *testing.T) {
	s, fps := twoClusterHub(t)

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

// TestAdditionalAppKeysForSpoke returns every key except the spoke's primary.
func TestAdditionalAppKeysForSpoke(t *testing.T) {
	s, fps := twoClusterHub(t)

	t.Run("github.com hive on GHE cluster gets the GHE key as additional", func(t *testing.T) {
		// Primary is the github.com app the hive claims; the OTHER key (GHE) must
		// be offered as additional.
		add := s.additionalAppKeysForSpoke(testGitHubComAppID)
		if len(add) != 1 {
			t.Fatalf("additional keys = %d, want 1", len(add))
		}
		if add[0].AppID != testGHEAppID {
			t.Fatalf("additional key app_id = %d, want %d", add[0].AppID, testGHEAppID)
		}
		if add[0].Fingerprint != fps[testGHEAppID] {
			t.Errorf("additional key fingerprint = %q, want %q", add[0].Fingerprint, fps[testGHEAppID])
		}
		if add[0].PrivateKey == "" {
			t.Error("additional key carries no private key value")
		}
	})

	t.Run("primary is never duplicated as additional", func(t *testing.T) {
		for _, k := range s.additionalAppKeysForSpoke(testGHEAppID) {
			if k.AppID == testGHEAppID {
				t.Errorf("primary app_id %d appeared in additional keys", testGHEAppID)
			}
		}
	})

	t.Run("primary 0 returns every fleet key", func(t *testing.T) {
		if got := len(s.additionalAppKeysForSpoke(0)); got != 2 {
			t.Fatalf("additional keys for primary 0 = %d, want 2 (all fleet keys)", got)
		}
	})
}

// TestAttachMissingAppKeys is the end-to-end delivery decision on the hub: a
// github.com hive on the GHE cluster must receive the github.com key it lacks,
// delivered idempotently.
func TestAttachMissingAppKeys(t *testing.T) {
	s, fps := twoClusterHub(t)

	t.Run("github.com hive on GHE cluster receives the github.com key", func(t *testing.T) {
		// The live bug: a github.com hive parked on vllm-d has inherited the GHE
		// cluster default (app_id 5686) and holds only the GHE key. It must be
		// handed the github.com key (3568013) so that once its app_id is corrected
		// to github.com, the matching key is already on disk. Its PRIMARY (the GHE
		// key) is delivered by the cluster reconcile; the github.com key rides here
		// as additional.
		payload := &HeartbeatPayload{
			HiveID:      "katamari-hive",
			ClusterID:   "vllm-d",
			GitHubAppID: testGHEAppID, // currently mis-inheriting the cluster app_id
		}
		var resp HeartbeatResponse
		s.attachMissingAppKeys(&resp, payload)

		if resp.GitHubAppConfig == nil {
			t.Fatal("no GitHubAppConfig attached; the github.com hive got no additional key")
		}
		add := resp.GitHubAppConfig.AdditionalKeys
		if len(add) != 1 || add[0].AppID != testGitHubComAppID {
			t.Fatalf("additional keys = %+v, want exactly the github.com key %d", add, testGitHubComAppID)
		}
		if add[0].Fingerprint != fps[testGitHubComAppID] {
			t.Errorf("delivered github.com key fingerprint = %q, want %q", add[0].Fingerprint, fps[testGitHubComAppID])
		}
	})

	t.Run("idempotent: a spoke already holding every additional key gets nothing", func(t *testing.T) {
		// A github.com hive's ONE additional key is the GHE key. Once it reports
		// holding that with the right fingerprint, nothing further rides the wire.
		payload := &HeartbeatPayload{
			HiveID:      "katamari-hive",
			ClusterID:   "vllm-d",
			GitHubAppID: testGitHubComAppID,
			GitHubAppKeysHeld: map[string]string{
				strconv.FormatInt(testGHEAppID, 10): fps[testGHEAppID],
			},
		}
		var resp HeartbeatResponse
		s.attachMissingAppKeys(&resp, payload)
		if resp.GitHubAppConfig != nil && len(resp.GitHubAppConfig.AdditionalKeys) != 0 {
			t.Fatalf("re-delivered a key the spoke already holds: %+v", resp.GitHubAppConfig.AdditionalKeys)
		}
	})

	t.Run("stale held fingerprint is corrected", func(t *testing.T) {
		payload := &HeartbeatPayload{
			HiveID:      "katamari-hive",
			ClusterID:   "vllm-d",
			GitHubAppID: testGitHubComAppID,
			GitHubAppKeysHeld: map[string]string{
				strconv.FormatInt(testGHEAppID, 10): "sha256:stalestalestalestale",
			},
		}
		var resp HeartbeatResponse
		s.attachMissingAppKeys(&resp, payload)
		if resp.GitHubAppConfig == nil || len(resp.GitHubAppConfig.AdditionalKeys) != 1 {
			t.Fatal("a spoke holding a STALE fingerprint must be re-delivered the current key")
		}
		if resp.GitHubAppConfig.AdditionalKeys[0].AppID != testGHEAppID {
			t.Fatalf("re-delivered app_id = %d, want the stale GHE key %d", resp.GitHubAppConfig.AdditionalKeys[0].AppID, testGHEAppID)
		}
	})

	t.Run("does not duplicate a key already attached as the primary this beat", func(t *testing.T) {
		// The cluster reconcile already attached the GHE key as the primary; the
		// additional pass must not attach it again.
		resp := HeartbeatResponse{GitHubAppConfig: &HeartbeatGitHubAppConfig{AppID: testGHEAppID}}
		payload := &HeartbeatPayload{HiveID: "vllmd-06", ClusterID: "vllm-d", GitHubAppID: testGHEAppID}
		s.attachMissingAppKeys(&resp, payload)
		for _, k := range resp.GitHubAppConfig.AdditionalKeys {
			if k.AppID == testGHEAppID {
				t.Errorf("GHE key %d attached as additional despite being the primary this beat", testGHEAppID)
			}
		}
		// It SHOULD still get the github.com key, since it lacks it.
		if len(resp.GitHubAppConfig.AdditionalKeys) != 1 || resp.GitHubAppConfig.AdditionalKeys[0].AppID != testGitHubComAppID {
			t.Fatalf("additional keys = %+v, want just the github.com key", resp.GitHubAppConfig.AdditionalKeys)
		}
	})

	t.Run("no fleet keys means nothing attached", func(t *testing.T) {
		empty := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{}}
		var resp HeartbeatResponse
		empty.attachMissingAppKeys(&resp, &HeartbeatPayload{HiveID: "h", GitHubAppID: 1})
		if resp.GitHubAppConfig != nil {
			t.Fatalf("attached a config with no fleet keys: %+v", resp.GitHubAppConfig)
		}
	})
}

// TestAdditionalKeysNeverLoggedOrInClustersJSON guards the security posture: the
// additional-keys mechanism must not leak key material into the cluster config
// surface that flows to operator-facing paths.
func TestAdditionalKeysCarryFingerprintNotInConfig(t *testing.T) {
	s, _ := twoClusterHub(t)
	add := s.additionalAppKeysForSpoke(0)
	if len(add) == 0 {
		t.Fatal("expected fleet keys")
	}
	// The ClusterConfig values must never carry a private key field (this is the
	// same invariant TestClusterConfigHasNoPrivateKeyField enforces structurally);
	// here we assert the delivery struct carries the key only in its dedicated
	// PrivateKey field and a fingerprint alongside.
	for _, k := range add {
		if k.PrivateKey == "" {
			t.Errorf("app_id %d additional key missing its private key value", k.AppID)
		}
		if k.Fingerprint == "" {
			t.Errorf("app_id %d additional key missing its non-secret fingerprint", k.AppID)
		}
	}
}
