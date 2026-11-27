package hub

// A colony-only boot skips the boot sync, so it runs the forget pass on its
// own (#20112). Every person here is synthetic.

import (
	"errors"
	"os"
	"testing"

	"github.com/hivecommons/colony/store"
)

// A user removed from /data/saas/users while the hub was down is erased from
// colony on the next colony-only boot; the others are kept.
func TestHubColonyOnlyBootForgetsDeletedUsers(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if ok, err := hubColonyOnlyMigrated(st); err != nil || !ok {
		t.Fatalf("colony-only document = %v, %v", ok, err)
	}
	closeHubCustomerStore()

	for _, p := range saaSUserFilePaths("bob-example") {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	st = useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if _, err := st.Get(crmPersonID(users[1])); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hub-deleted user kept in colony-only: %v", err)
	}
	for _, u := range []*SaaSUser{users[0], users[2]} {
		if _, err := st.Get(crmPersonID(u)); err != nil {
			t.Fatalf("user %s with a json record erased: %v", crmPersonID(u), err)
		}
	}
}

// A colony-only boot that finds no users directory erases nobody.
func TestHubColonyOnlyBootMissingUsersDirForgetsNobody(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()

	if err := os.RemoveAll(saasUsersDir); err != nil {
		t.Fatal(err)
	}
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	for _, u := range users {
		if _, err := st.Get(crmPersonID(u)); err != nil {
			t.Fatalf("user %s erased with the users directory missing: %v", crmPersonID(u), err)
		}
	}
}
