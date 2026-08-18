package hub

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestAuthorizedUsersForHiveID verifies the hub builds the per-hive access list
// delivered to spokes via heartbeat: the owner is always included as owner,
// granted users appear with their role, and a hive with no SaaS record yields
// nil (so a non-hosted spoke's own allowlist is left untouched).
func TestAuthorizedUsersForHiveID(t *testing.T) {
	dir := t.TempDir()
	origHives, origUsers := saasHivesDir, saasUsersDir
	saasHivesDir = filepath.Join(dir, "hives")
	saasUsersDir = filepath.Join(dir, "users")
	os.MkdirAll(saasHivesDir, 0o755)
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasHivesDir, saasUsersDir = origHives, origUsers })

	// No SaaS record -> nil.
	if got := authorizedUsersForHiveID("hosted-nope"); got != nil {
		t.Errorf("unknown hive should return nil, got %v", got)
	}

	// Create a hive owned by "alice".
	h := &SaaSHive{ID: "hosted-x", Owner: "alice"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	// Grant "bob" read-write and "carol" read on that hive.
	for name, role := range map[string]string{"bob": "read-write", "carol": "read"} {
		u := ensureSaaSUser(name)
		u.GitHubUsername = name
		if u.Hives == nil {
			u.Hives = map[string]string{}
		}
		u.Hives["hosted-x"] = role
		saveSaaSUser(u)
	}

	got := authorizedUsersForHiveID("hosted-x")
	sort.Strings(got)
	want := []string{"alice:owner", "bob:read-write", "carol:read"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestAuthorizedUsersForHiveID_OwnerNotDuplicated ensures an owner who ALSO has
// an explicit access record is listed once, as owner.
func TestAuthorizedUsersForHiveID_OwnerNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	origHives, origUsers := saasHivesDir, saasUsersDir
	saasHivesDir = filepath.Join(dir, "hives")
	saasUsersDir = filepath.Join(dir, "users")
	os.MkdirAll(saasHivesDir, 0o755)
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasHivesDir, saasUsersDir = origHives, origUsers })

	saveSaaSHive(&SaaSHive{ID: "hosted-y", Owner: "dave"})
	u := ensureSaaSUser("dave")
	u.GitHubUsername = "dave"
	u.Hives = map[string]string{"hosted-y": "owner"}
	saveSaaSUser(u)

	got := authorizedUsersForHiveID("hosted-y")
	if len(got) != 1 || got[0] != "dave:owner" {
		t.Errorf("owner should appear once as owner, got %v", got)
	}
}
