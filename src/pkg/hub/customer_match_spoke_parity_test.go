package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// The spoke's hub import (#19163) must derive the same match key and hash
// the same way as the hub's match endpoint, or no consented person ever
// matches.
func TestCustomerMatchSpokeParity(t *testing.T) {
	const master, hive = "test-master-secret", "hive-a"
	t.Setenv(spoke.EnvCustomerMatchKey, "")
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(spoke.EnvHiveID, hive)
	key := spoke.SpokeCustomerMatchKey("")
	if key == "" || key != customerMatchKey(master, hive) {
		t.Fatalf("spoke-derived match key %q != hub key %q", key, customerMatchKey(master, hive))
	}
	if key == derivePerHiveKey(master, infoHeartbeatKey, hive) {
		t.Fatal("the match key must not be the heartbeat bearer")
	}
	for _, login := range []string{"ada", " Ada ", "OctoCat"} {
		if got, want := customers.HubMatchHash(key, login), customerMatchHash(key, login); got != want || !validCustomerMatchHash(got) {
			t.Fatalf("hash(%q): spoke %q, hub %q", login, got, want)
		}
	}
	if got := spoke.SpokeCustomerMatchKey("hive-b"); got != customerMatchKey(master, "hive-b") {
		t.Fatalf("an explicit hive id must win over HIVE_ID: %q", got)
	}
	t.Setenv(spoke.EnvCustomerMatchKey, "injected")
	if spoke.SpokeCustomerMatchKey("") != "injected" {
		t.Fatal("an injected key must win")
	}
	t.Setenv(spoke.EnvCustomerMatchKey, "")
	t.Setenv("HIVE_HUB_SECRET", "")
	if spoke.SpokeCustomerMatchKey("") != "" {
		t.Fatal("no master and no injected key must give no key")
	}
}
