package hub

// Tests for the narrow-only merge of a consents directory newer than the
// colony-only migration (#20115). Every person here is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/colony/store"
)

// writeNewerConsentFiles puts a consents directory holding files beside a
// migrated store, last changed at mtime.
func writeNewerConsentFiles(t *testing.T, files map[string]string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(customerConsentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(customerConsentDir(), name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(customerConsentDir(), mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func mergeConsentDoc(t *testing.T, records map[string]customerConsent) string {
	t.Helper()
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// migratedFixture migrates the fixture users to colony-only and closes the
// store, returning the users and the migration time.
func mergeMigratedFixture(t *testing.T) ([]*SaaSUser, time.Time) {
	t.Helper()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	at := hubMigratedAt(t, st)
	closeHubCustomerStore()
	return users, at
}

func mergeWithdrawnConsent(hive string, at time.Time) customerConsent {
	return customerConsent{Person: "ada-example", HiveID: hive, TextVersion: customerConsentTextVersion,
		GrantedAt: at, UpdatedAt: at, WithdrawnAt: &at}
}

// A withdrawal made in the directory while colony-only was not running wins
// over colony's grant, in a colony-only boot and in a rollback, and the
// directory is then set aside, kept.
func TestHubColonyOnlyMergeDirectoryWithdrawalWins(t *testing.T) {
	for _, tc := range []struct{ mode, aside string }{
		{hubCustomerStoreColonyOnly, hubIgnoredSuffix},
		{hubCustomerStoreColony, hubRestoredAsideSuffix},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users, at := mergeMigratedFixture(t)
			w := at.Add(time.Hour).UTC()
			body := mergeConsentDoc(t, map[string]customerConsent{"own": mergeWithdrawnConsent("own", w)})
			writeNewerConsentFiles(t, map[string]string{"ada-example.json": body}, w)

			useHubCustomerStore(t, tc.mode)
			c := readMatchConsent(t, users[0], "own")
			if c.WithdrawnAt == nil || !c.WithdrawnAt.Equal(w) || c.Fields != (customerConsentFields{}) {
				t.Fatalf("consent after the merge = %+v, want the directory's withdrawal", c)
			}
			if data, err := os.ReadFile(filepath.Join(customerConsentDir()+tc.aside, "ada-example.json")); err != nil || string(data) != body {
				t.Errorf("the merged directory was not kept aside as consents%s: %v", tc.aside, err)
			}
		})
	}
}

// A withdrawal colony holds wins over a grant, wider or not, in the
// directory.
func TestHubColonyOnlyMergeColonyWithdrawalWins(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	w := time.Now().UTC()
	writeMatchConsent(t, users[0], "own", func(c *customerConsent) {
		c.Fields, c.UpdatedAt, c.WithdrawnAt = customerConsentFields{}, w, &w
	})
	at := hubMigratedAt(t, st)
	closeHubCustomerStore()
	later := at.Add(time.Hour).UTC()
	writeNewerConsentFiles(t, map[string]string{"ada-example.json": mergeConsentDoc(t, map[string]customerConsent{
		"own": {Person: "ada-example", HiveID: "own", TextVersion: customerConsentTextVersion, GrantedAt: later, UpdatedAt: later,
			Fields: customerConsentFields{RealName: true, Organization: true, Country: true}},
	})}, later)

	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if c := readMatchConsent(t, users[0], "own"); c.WithdrawnAt == nil || c.Fields != (customerConsentFields{}) {
		t.Errorf("a grant in the directory undid colony's withdrawal: %+v", c)
	}
	if _, err := os.Stat(customerConsentDir()); !os.IsNotExist(err) {
		t.Errorf("the merged directory is still in place: %v", err)
	}
}

// Nothing in the directory widens what colony holds: a grant both hold keeps
// only the fields both grant, and a grant only the directory holds is not
// added.
func TestHubColonyOnlyMergeNeverWidens(t *testing.T) {
	for _, mode := range []string{hubCustomerStoreColonyOnly, hubCustomerStoreColony} {
		t.Run(mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users, at := mergeMigratedFixture(t)
			later := at.Add(time.Hour).UTC()
			// Colony grants real_name and country for "own".
			writeNewerConsentFiles(t, map[string]string{"ada-example.json": mergeConsentDoc(t, map[string]customerConsent{
				"own": {Person: "ada-example", HiveID: "own", TextVersion: customerConsentTextVersion, GrantedAt: later, UpdatedAt: later,
					Fields: customerConsentFields{RealName: true, Organization: true}},
				"other": {Person: "ada-example", HiveID: "other", TextVersion: customerConsentTextVersion, GrantedAt: later, UpdatedAt: later,
					Fields: customerConsentFields{RealName: true, Organization: true, Country: true}},
			})}, later)

			useHubCustomerStore(t, mode)
			if c := readMatchConsent(t, users[0], "own"); c.WithdrawnAt != nil || c.Fields != (customerConsentFields{RealName: true}) {
				t.Errorf("own after the merge = %+v, want real_name only (narrowed, never widened)", c.Fields)
			}
			if c := readMatchConsent(t, users[0], "other"); c.HiveID != "" {
				t.Errorf("a grant only the directory held was added: %+v", c)
			}
		})
	}
}

// Anything the merge cannot classify still refuses colony-only with nothing
// moved or written, even beside a withdrawal it could merge.
func TestHubColonyOnlyMergeRefusesUnclassifiable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files func(t *testing.T, withdrawal string, at time.Time) map[string]string
	}{
		{"grant colony holds is missing", func(*testing.T, string, time.Time) map[string]string {
			return map[string]string{"ada-example.json": `{}`}
		}},
		{"unknown field", func(*testing.T, string, time.Time) map[string]string {
			return map[string]string{"ada-example.json": `{"own":{"person":"ada-example","hive_id":"own","fields_v2":{}}}`}
		}},
		{"file for no known user", func(_ *testing.T, w string, _ time.Time) map[string]string {
			return map[string]string{"ada-example.json": w, "zed-example.json": `{}`}
		}},
		{"not a consent file", func(_ *testing.T, w string, _ time.Time) map[string]string {
			return map[string]string{"ada-example.json": w, "notes.txt": "synthetic"}
		}},
		{"delivered grant colony has no record of", func(t *testing.T, _ string, at time.Time) map[string]string {
			return map[string]string{"ada-example.json": mergeConsentDoc(t, map[string]customerConsent{
				"own":   mergeWithdrawnConsent("own", at),
				"other": {Person: "ada-example", HiveID: "other", GrantedAt: at, UpdatedAt: at, LastDeliveredAt: &at},
			})}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users, at := mergeMigratedFixture(t)
			later := at.Add(time.Hour).UTC()
			files := tc.files(t, mergeConsentDoc(t, map[string]customerConsent{"own": mergeWithdrawnConsent("own", later)}), later)
			writeNewerConsentFiles(t, files, later)

			got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColonyOnly)
			t.Cleanup(closeHubCustomerStore)
			if got != hubCustomerStoreJSON {
				t.Fatalf("mode = %q, want json (refused)", got)
			}
			for name, body := range files {
				if data, err := os.ReadFile(filepath.Join(customerConsentDir(), name)); err != nil || string(data) != body {
					t.Errorf("%s was moved or rewritten by a refused merge: %v", name, err)
				}
			}
			if _, err := os.Stat(customerConsentDir() + hubIgnoredSuffix); !os.IsNotExist(err) {
				t.Errorf("a refused merge set the directory aside: %v", err)
			}
			closeHubCustomerStore()
			// The store is still colony-only, so the refused merge must leave
			// it guarded against older images (#20116). Release the guard the
			// way this hub does before it opens the file.
			if err := openAsOlderImage(t); !errors.Is(err, store.ErrSchemaTooNew) {
				t.Fatalf("older image open after a refused merge = %v, want ErrSchemaTooNew", err)
			}
			if err := releaseHubColonyOnlyGuard(hubCustomerStorePath(), hubCustomerStoreLockTimeout); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(hubCustomerStorePath(), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			rec, ok, err := crmGetRecord(st, crmPersonID(users[0]))
			if err != nil || !ok {
				t.Fatalf("colony record = %v, %v", ok, err)
			}
			if c := rec.Consents["own"]; c.WithdrawnAt != nil || !c.Fields.RealName {
				t.Errorf("a refused merge wrote to colony: %+v", c)
			}
			if _, ok := rec.Consents["other"]; ok {
				t.Error("a refused merge added a record to colony")
			}
		})
	}
}
