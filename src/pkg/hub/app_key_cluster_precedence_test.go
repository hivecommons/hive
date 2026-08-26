package hub

import (
	"strings"
	"testing"
)

// A spoke provisioned under an old cluster ID keeps reporting that ID in every
// heartbeat. The hub's OWN record — the one the registry stamps, the
// app-creds-undelivered alert checks the PEM store under, and the PUT
// /api/saas/admin/cluster-app-keys/{id} remedy names — can meanwhile say the
// hive belongs to a different cluster. Key delivery used to trust the spoke's
// report first, so the operator could do exactly what the alert said (upload
// the key under the hub record's cluster, "hive-oke"), have the alert confirm
// "the hub holds a key" — and still watch every beat look the key up under the
// spoke's stale ID ("oke-260812-8yiz") and deliver nothing, forever
// (kelly-headwaters, #4316/#4323). The hub record must win on disagreement.
func TestAppKeySyncForHeartbeatHubRecordClusterWinsOverStaleSpokeReport(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	keyPEM := testAppKeyPEM(t)
	// The key lives where the alert told the operator to put it: the cluster
	// the hub records for this hive.
	if err := storeClusterAppKey("hive-oke", keyPEM); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"hive-oke": {ID: "hive-oke", GitHubAppID: 3568013, GitHubAppSlug: "kubestellar-hive"},
		},
	}

	// The hub's record: kelly-headwaters belongs to hive-oke.
	if err := saveSaaSHive(&SaaSHive{
		ID:        "kelly-headwaters",
		ClusterID: "hive-oke",
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("stale spoke-reported cluster does not block delivery", func(t *testing.T) {
		// The spoke still reports the cluster ID it was provisioned under,
		// which the hub's cluster registry no longer knows a key for.
		got := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:            "kelly-headwaters",
			ClusterID:         "oke-260812-8yiz",
			GitHubAppRequired: true,
			GitHubAppState:    appStateKeyMissingToken,
		})
		if got == nil {
			t.Fatal("no key delivered: the spoke's stale cluster ID overrode the hub's own record, the exact undelivered-key wedge")
		}
		if got.AppID != 3568013 {
			t.Errorf("AppID = %d, want 3568013", got.AppID)
		}
		if strings.TrimSpace(got.PrivateKey) != strings.TrimSpace(keyPEM) {
			t.Error("delivered key is not the key stored for the hub-recorded cluster")
		}
	})

	t.Run("spoke report still serves hives without a hub record", func(t *testing.T) {
		got := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:    "byo-spoke-no-record",
			ClusterID: "hive-oke",
		})
		if got == nil {
			t.Fatal("a hive with no SaaS record must still resolve its cluster from the spoke's report")
		}
		if got.AppID != 3568013 {
			t.Errorf("AppID = %d, want 3568013", got.AppID)
		}
	})
}
