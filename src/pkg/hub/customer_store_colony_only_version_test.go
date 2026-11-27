package hub

// Tests for the colony-only guard (#20116): an image older than colony-only
// opens colony.db with a colony release that refuses a newer schema_version,
// which store.Open of this release shows the same way. Every person here is
// synthetic.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hivecommons/colony/store"
)

// openAsOlderImage opens colony.db the way an image older than colony-only
// does: store.Open with no guard handling.
func openAsOlderImage(t *testing.T) error {
	t.Helper()
	st, err := store.Open(hubCustomerStorePath(), store.Options{LockTimeout: hubCustomerStoreLockTimeout})
	if err == nil {
		_ = st.Close()
	}
	return err
}

// A colony-only store is refused by an older image once this hub stops.
func TestHubColonyOnlyGuardRefusesOlderImage(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()

	if err := openAsOlderImage(t); !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("older image open = %v, want ErrSchemaTooNew", err)
	}
}

// This image opens its own guarded store again and stays colony-only, and
// guards it again at once.
func TestHubColonyOnlyGuardReopens(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	ada := loadSaaSUser("ada-example")
	ada.Notes = "guarded synthetic note"
	if err := saveSaaSUser(ada); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()

	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if ok, err := hubColonyOnlyMigrated(st); err != nil || !ok {
		t.Fatalf("colony-only document after reopen = %v, %v", ok, err)
	}
	if got := loadSaaSUser("ada-example"); got == nil || got.Notes != "guarded synthetic note" || got.FullName != "Ada Example" {
		t.Fatalf("ada after reopen = %+v", got)
	}
	db, err := hubStoreBolt(st)
	if err != nil {
		t.Fatal(err)
	}
	if on, err := hubColonyOnlyGuarded(db); err != nil || !on {
		t.Fatalf("guard while open = %v, %v; want set", on, err)
	}
}

// A store that never went colony-only is not guarded: older images keep
// opening it.
func TestHubColonyOnlyGuardNotSetOutsideColonyOnly(t *testing.T) {
	for _, mode := range []string{hubCustomerStoreShadow, hubCustomerStoreColony} {
		t.Run(mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			fixtureUsers(t)
			useHubCustomerStore(t, mode)
			closeHubCustomerStore()
			if err := openAsOlderImage(t); err != nil {
				t.Fatalf("older image open of a %s store = %v", mode, err)
			}
		})
	}
}

// Rolling back from colony-only drops the guard with the marker.
func TestHubColonyOnlyGuardClearedByRollback(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()
	useHubCustomerStore(t, hubCustomerStoreColony)
	closeHubCustomerStore()

	if err := openAsOlderImage(t); err != nil {
		t.Fatalf("older image open after rollback = %v", err)
	}
}

// colony export opens the guarded store and leaves it guarded.
func TestHubColonyOnlyGuardKeptByExport(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()

	if _, err := ExportHubCustomerStore(filepath.Dir(saasUsersDir), t.TempDir(), false); err != nil {
		t.Fatalf("export of a guarded store: %v", err)
	}
	if err := openAsOlderImage(t); !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("older image open after export = %v, want ErrSchemaTooNew", err)
	}
}

// Releasing puts the real schema_version back, so store.Open upgrades
// nothing and reads the same version.
func TestHubColonyOnlyGuardReleaseRestoresVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), store.DefaultFileName)
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := setHubColonyOnlyGuard(st, true); err != nil {
		t.Fatal(err)
	}
	if v, err := st.SchemaVersion(); err != nil || v != hubColonyOnlyGuardSchema {
		t.Fatalf("guarded schema_version = %d, %v", v, err)
	}
	_ = st.Close()
	if err := releaseHubColonyOnlyGuard(path, hubCustomerStoreLockTimeout); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path, store.Options{})
	if err != nil {
		t.Fatalf("open after release: %v", err)
	}
	defer func() { _ = st.Close() }()
	if v, err := st.SchemaVersion(); err != nil || v != store.SchemaVersion {
		t.Fatalf("released schema_version = %d, %v; want %d", v, err, store.SchemaVersion)
	}
}

// The guard is written through the open store's own handle; a colony
// release that renames it fails here.
func TestHubColonyOnlyGuardReachesStoreHandle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), store.DefaultFileName), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := hubStoreBolt(st); err != nil {
		t.Fatal(err)
	}
}
