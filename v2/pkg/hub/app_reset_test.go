package hub

import "testing"

// TestAppResetIsDeliveredUntilConfirmed pins the operator-facing contract for
// "Reset App": the request survives until the spoke reports installation_id 0.
//
// It cannot be expressed by pushing a zero. The spoke adopts an installation
// only when the pushed value is NON-zero, deliberately — zero means "the hub is
// not speaking to this field", and treating it as a clear once turned a
// key-only fault into a total auth outage. So the reset is its own flag.
func TestAppResetIsDeliveredUntilConfirmed(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	h := &SaaSHive{ID: "reset-me", ClusterID: "hive-oke", RequestedAppReset: true}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: appKeyTestLogger()}

	t.Run("delivered while the spoke still reports an installation", func(t *testing.T) {
		got := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
			HiveID: "reset-me", GitHubInstallationID: 145050760,
		})
		if got == nil || !got.ResetInstallation {
			t.Fatalf("reset not delivered: %+v", got)
		}
		// A lost beat must not drop it.
		again := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
			HiveID: "reset-me", GitHubInstallationID: 145050760,
		})
		if again == nil || !again.ResetInstallation {
			t.Fatal("reset was dropped after one delivery; a lost beat would lose the operator's click")
		}
	})

	t.Run("cleared once the spoke reports installation 0", func(t *testing.T) {
		if got := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
			HiveID: "reset-me", GitHubInstallationID: 0,
		}); got != nil {
			t.Fatalf("still delivering after confirmation: %+v", got)
		}
		// And it stays cleared — the hive must not be reset forever.
		if h2 := loadSaaSHive("reset-me"); h2 == nil || h2.RequestedAppReset {
			t.Fatal("RequestedAppReset was not cleared on the hive record")
		}
		if got := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
			HiveID: "reset-me", GitHubInstallationID: 145050760,
		}); got != nil && got.ResetInstallation {
			t.Fatal("reset re-armed itself after being confirmed")
		}
	})
}

// TestNoResetMeansNoResetFlag: a hive that was never reset must never receive
// the flag. Sending it spuriously would blank a working installation and take
// the hive offline until its owner reinstalls.
func TestNoResetMeansNoResetFlag(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	if err := saveSaaSHive(&SaaSHive{ID: "healthy", ClusterID: "hive-oke"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: appKeyTestLogger()}
	got := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
		HiveID: "healthy", GitHubInstallationID: 12345,
	})
	if got != nil && got.ResetInstallation {
		t.Fatal("a healthy hive was told to reset its App")
	}
}
