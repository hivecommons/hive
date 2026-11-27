package hub

// Tests for HIVE_HUB_CUSTOMER_STORE=colony-only (#19972). Every person here
// is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/colony/store"
)

func userFileJSON(t *testing.T, u *SaaSUser) map[string]any {
	t.Helper()
	path, err := saveSaaSUserPath(u)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func migratedDir(name string) string {
	return filepath.Join(filepath.Dir(saasUsersDir), name+hubMigratedSuffix)
}

// The first colony-only boot verifies every record, keeps copies of the
// users and consents, and a save then leaves the CRM fields out of the
// user JSON while reads still see them.
func TestHubColonyOnlyMigratesAndStrips(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)

	if _, err := os.Stat(customerConsentDir()); !os.IsNotExist(err) {
		t.Fatalf("consents directory still in place after the migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(migratedDir("consents"), "ada-example.json")); err != nil {
		t.Fatalf("consent file not kept in consents.migrated: %v", err)
	}
	if entries, err := os.ReadDir(migratedDir("users")); err != nil || len(entries) != len(users) {
		t.Fatalf("users.migrated = %d entries, %v; want a copy of every user", len(entries), err)
	}
	if ok, err := hubColonyOnlyMigrated(st); err != nil || !ok {
		t.Fatalf("colony-only document = %v, %v", ok, err)
	}

	// Reads before any save: the JSON still holds the fields.
	ada := loadSaaSUser("ada-example")
	if ada == nil || ada.Notes != users[0].Notes || ada.FullName != "Ada Example" {
		t.Fatalf("ada before a save = %+v", ada)
	}
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.RealName || !c.Fields.Country {
		t.Fatalf("consent read from colony = %+v", c)
	}

	ada.Notes = "colony-only synthetic note"
	if err := saveSaaSUser(ada); err != nil {
		t.Fatal(err)
	}
	onDisk := userFileJSON(t, users[0])
	for _, field := range []string{"full_name", "notes", "company", "slack_id", "affiliation", "top_repo"} {
		if _, ok := onDisk[field]; ok {
			t.Errorf("colony-only wrote %s into the user JSON", field)
		}
	}
	if onDisk["crm_in_colony"] != true || onDisk["hives"] == nil {
		t.Errorf("stripped record = %v, want crm_in_colony and the account data", onDisk)
	}
	got := loadSaaSUser("ada-example")
	if got.Notes != "colony-only synthetic note" || got.FullName != "Ada Example" || got.Company != "Example Org" {
		t.Errorf("read after a stripped save = %+v", got)
	}

	// A consent change goes to colony only.
	writeMatchConsent(t, users[0], "own", func(c *customerConsent) { c.Fields.Organization = true })
	if _, err := os.Stat(customerConsentDir()); !os.IsNotExist(err) {
		t.Errorf("a colony-only consent write created the consents directory: %v", err)
	}
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.Organization {
		t.Errorf("consent after a colony-only write = %+v", c)
	}
	if paths, err := customerConsentPaths(); err != nil || len(paths) != 1 {
		t.Errorf("consent listing in colony-only = %v, %v", paths, err)
	}
}

// A colony write that fails keeps the full record in the JSON: nothing is
// lost, and the record is read from the JSON.
func TestHubColonyOnlyFailedWriteKeepsFullJSON(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	ada := loadSaaSUser("ada-example")
	// Larger than colony's attachment limit: the colony write fails.
	ada.Notes = strings.Repeat("n", store.MaxAttachmentBytes+1)
	if err := saveSaaSUser(ada); err != nil {
		t.Fatal(err)
	}
	onDisk := userFileJSON(t, users[0])
	if onDisk["crm_in_colony"] == true || onDisk["notes"] != ada.Notes || onDisk["full_name"] != "Ada Example" {
		t.Fatal("a failed colony write stripped the user JSON")
	}
	if got := loadSaaSUser("ada-example"); got.Notes != ada.Notes {
		t.Error("the record is not read from the JSON after a failed colony write")
	}
}

// A save of a record read without the overlay (a sweep that only changes
// account data) never wipes the CRM fields kept in colony.
func TestHubColonyOnlyRawSaveKeepsColonyFields(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
		t.Fatal(err)
	}
	raw, err := readHubUserForSync(mustUserPath(t, users[0]))
	if err != nil {
		t.Fatal(err)
	}
	raw.SaaSQuota = 7
	if err := saveSaaSUser(raw); err != nil {
		t.Fatal(err)
	}
	got := loadSaaSUser("ada-example")
	if got.SaaSQuota != 7 || got.FullName != "Ada Example" || got.Notes != users[0].Notes {
		t.Errorf("after a raw save = quota %d name %q notes %q", got.SaaSQuota, got.FullName, got.Notes)
	}
}

func mustUserPath(t *testing.T, u *SaaSUser) string {
	t.Helper()
	p, err := saveSaaSUserPath(u)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Rollback is a mode change: a colony (or json) boot writes the CRM fields
// and the consent files back from colony.
func TestHubColonyOnlyRollback(t *testing.T) {
	for _, mode := range []string{hubCustomerStoreColony, hubCustomerStoreJSON} {
		t.Run(mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users := fixtureUsers(t)
			useHubCustomerStore(t, hubCustomerStoreColonyOnly)
			ada := loadSaaSUser("ada-example")
			ada.Notes = "note before rollback"
			if err := saveSaaSUser(ada); err != nil {
				t.Fatal(err)
			}
			writeMatchConsent(t, users[0], "own", func(c *customerConsent) { c.DeliveredRev = "rev-synthetic" })
			closeHubCustomerStore()

			st := useHubCustomerStore(t, mode)
			onDisk := userFileJSON(t, users[0])
			if onDisk["crm_in_colony"] == true || onDisk["notes"] != "note before rollback" || onDisk["full_name"] != "Ada Example" {
				t.Fatalf("user JSON after rollback to %s = %v", mode, onDisk)
			}
			if c := readMatchConsent(t, users[0], "own"); c.DeliveredRev != "rev-synthetic" || !c.Fields.RealName {
				t.Fatalf("consent file after rollback = %+v", c)
			}
			if st != nil {
				if ok, _ := hubColonyOnlyMigrated(st); ok {
					t.Error("the colony-only document survived the rollback")
				}
				if _, ok := colonyPerson(t, st, users[0]).Attachments[crmAttachRecord]; ok {
					t.Error("the colony-only copy outlived the rollback")
				}
			}
			if _, err := os.Stat(migratedDir("users")); err != nil {
				t.Errorf("users.migrated must be kept: %v", err)
			}
		})
	}
}

// colony export writes the same files elsewhere, and never overwrites
// without force.
func TestHubColonyOnlyExport(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	ada := loadSaaSUser("ada-example")
	ada.Notes = "exported synthetic note"
	if err := saveSaaSUser(ada); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()

	out := t.TempDir()
	rep, err := ExportHubCustomerStore(filepath.Dir(saasUsersDir), out, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if rep.Users != len(users) || rep.Consents != 1 {
		t.Errorf("report = %+v", rep)
	}
	data, err := os.ReadFile(filepath.Join(rep.UsersDir, "ada-example.json"))
	if err != nil || !strings.Contains(string(data), "exported synthetic note") || strings.Contains(string(data), "crm_in_colony") {
		t.Errorf("exported user = %s, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(rep.Consent, "ada-example.json")); err != nil {
		t.Errorf("exported consent: %v", err)
	}
	if _, err := ExportHubCustomerStore(filepath.Dir(saasUsersDir), out, false); err == nil {
		t.Error("a second export overwrote files without force")
	}
	if _, err := ExportHubCustomerStore(filepath.Dir(saasUsersDir), out, true); err != nil {
		t.Errorf("export with force: %v", err)
	}
}

// A boot sync without parity never migrates: the hub runs in colony mode
// and every file stays where it was.
func TestHubColonyOnlyNeedsParity(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	if err := os.WriteFile(filepath.Join(saasUsersDir, "broken-example.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColonyOnly)
	t.Cleanup(closeHubCustomerStore)
	if got != hubCustomerStoreColony {
		t.Fatalf("mode = %q, want colony", got)
	}
	if _, err := os.Stat(customerConsentDir()); err != nil {
		t.Errorf("consents moved without a verified migration: %v", err)
	}
	if _, err := os.Stat(migratedDir("users")); !os.IsNotExist(err) {
		t.Errorf("users.migrated written without a verified migration: %v", err)
	}
}

// The trust domain still gates every answer in colony-only.
func TestHubColonyOnlyMatchKeepsTrustDomain(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)

	if m := matchOwn(h); len(m) != 1 || m[0].RealName != "Ada Example" || m[0].Country != "GB" {
		t.Fatalf("trusted hive answer in colony-only = %+v", m)
	}
	resetCustomerMatchRates()
	writeMatchConsent(t, &SaaSUser{GitHubUsername: "ada-example"}, "stranger", func(c *customerConsent) {
		c.Fields = customerConsentFields{RealName: true}
	})
	if got := h.matchLabeled("stranger", matchHash("stranger", "ada-example")); len(got.Matches) != 0 {
		t.Fatalf("a hive outside the trust domain heard in colony-only: %+v", got.Matches)
	}
}

// Colony mode now writes the delivery bookkeeping and a long note.
func TestHubColonyModeWritesNewSlots(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	if m := matchOwn(h); len(m) != 1 {
		t.Fatalf("answer = %+v", m)
	}
	c, ok := colonyPerson(t, st, users[0]).Consent("own")
	if !ok || !c.Delivered() || c.DeliveredRev == "" || !slices.Contains(c.DeliveredFields, store.FieldRealName) {
		t.Errorf("colony consent after a delivery = %+v, want the bookkeeping", c)
	}
	ada := loadSaaSUser("ada-example")
	ada.Notes = strings.Repeat("long synthetic note line\n", 400)
	if err := saveSaaSUser(ada); err != nil {
		t.Fatal(err)
	}
	if f := effectiveFact(t, colonyPerson(t, st, users[0]), store.FieldNotes); f.Value != strings.TrimSpace(ada.Notes) {
		t.Errorf("a long note did not reach colony whole (%d bytes)", len(f.Value))
	}
	// A pruned consent is deleted from colony, not emptied.
	path, _ := customerConsentPath(users[0])
	if err := saveCustomerConsents(path, map[string]customerConsent{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := colonyPerson(t, st, users[0]).Consent("own"); ok {
		t.Error("a pruned consent is still in colony")
	}
}

func TestScrubHubMigrated(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := scrubHubMigrated(users[0]); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{migratedDir("users"), migratedDir("consents")} {
		if _, err := os.Stat(filepath.Join(d, "ada-example.json")); !os.IsNotExist(err) {
			t.Errorf("%s still holds the deleted user: %v", filepath.Base(d), err)
		}
	}
	if _, err := os.Stat(filepath.Join(migratedDir("users"), "bob-example.json")); err != nil {
		t.Errorf("another user's copy was removed: %v", err)
	}
}

func TestUniqueSiblingNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "consents")
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	first, err := uniqueSibling(base, hubMigratedSuffix, now)
	if err != nil || first != base+hubMigratedSuffix {
		t.Fatalf("first = %q, %v", first, err)
	}
	if err := os.Mkdir(first, 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := uniqueSibling(base, hubMigratedSuffix, now)
	if err != nil || second == first {
		t.Fatalf("second = %q, %v", second, err)
	}
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := uniqueSibling(base, hubMigratedSuffix, now); err == nil {
		t.Error("a taken name was handed out again")
	}
}

// writeReappearedConsents puts a consents directory (one file for ada)
// beside a migrated store, last changed at mtime.
func writeReappearedConsents(t *testing.T, body string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(customerConsentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(customerConsentDir(), "ada-example.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, customerConsentDir()} {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func hubMigratedAt(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	doc, ok, err := hubColonyOnlyDocOf(st)
	if err != nil || !ok {
		t.Fatalf("colony-only document = %v, %v", ok, err)
	}
	return doc.MigratedAt
}

// A consents directory older than the migration that reappears beside a
// colony-only store (a stale copy put back) is set aside on the next boot,
// kept, and never read as the records.
func TestHubColonyOnlySetsAsideReappearedConsents(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	at := hubMigratedAt(t, st)
	closeHubCustomerStore()
	writeReappearedConsents(t, `{}`, at.Add(-time.Hour))
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if _, err := os.Stat(customerConsentDir()); !os.IsNotExist(err) {
		t.Fatalf("the reappeared consents directory is still in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(customerConsentDir()+hubIgnoredSuffix, "ada-example.json")); err != nil {
		t.Fatalf("the reappeared consents were not kept aside: %v", err)
	}
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.RealName {
		t.Errorf("colony consent lost after a stale directory reappeared: %+v", c)
	}
}

// A consents directory changed after the migration (an older binary, or a
// json-mode fallback, wrote it) that the narrow-only merge cannot classify
// refuses colony-only, and refuses a rollback, with nothing moved or
// written: neither copy can be chosen safely. The hub reads that directory in json
// mode until an operator moves it aside.
func TestHubColonyOnlyRefusesNewerConsents(t *testing.T) {
	for _, mode := range []string{hubCustomerStoreColonyOnly, hubCustomerStoreColony, hubCustomerStoreShadow, hubCustomerStoreJSON} {
		t.Run(mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users := fixtureUsers(t)
			st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
			if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
				t.Fatal(err)
			}
			at := hubMigratedAt(t, st)
			closeHubCustomerStore()
			// A withdrawal made while colony-only was not running, under a
			// key that does not match its hive: the merge cannot classify it.
			newer := `{"own":{"person":"ada-example","hive_id":"other","withdrawn_at":"2026-11-20T00:00:00Z"}}`
			path := writeReappearedConsents(t, newer, at.Add(time.Hour))
			before, err := os.ReadFile(mustUserPath(t, users[0]))
			if err != nil {
				t.Fatal(err)
			}

			got := openHubCustomerStore(context.Background(), nil, mode)
			t.Cleanup(closeHubCustomerStore)
			if got != hubCustomerStoreJSON {
				t.Fatalf("mode = %q, want json (refused)", got)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != newer {
				t.Fatalf("the newer consents directory was moved or rewritten: %v", err)
			}
			for _, aside := range []string{hubIgnoredSuffix, hubRestoredAsideSuffix} {
				if _, err := os.Stat(customerConsentDir() + aside); !os.IsNotExist(err) {
					t.Errorf("consents%s written by a refused boot: %v", aside, err)
				}
			}
			if after, err := os.ReadFile(mustUserPath(t, users[0])); err != nil || string(after) != string(before) {
				t.Errorf("a refused boot rewrote a user record: %v", err)
			}
			closeHubCustomerStore()
			if err := releaseHubColonyOnlyGuard(hubCustomerStorePath(), time.Second); err != nil {
				t.Fatal(err)
			}
			st2, err := store.Open(hubCustomerStorePath(), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st2.Close() }()
			if ok, _ := hubColonyOnlyMigrated(st2); !ok {
				t.Error("a refused boot dropped the colony-only document")
			}
			if _, ok, _ := crmGetRecord(st2, crmPersonID(users[0])); !ok {
				t.Error("a refused boot dropped the colony record")
			}
		})
	}
}

// The by-hand rollback copies the export into place: a consents directory
// that holds only colony's own files is not a conflict (it is set aside,
// kept), and the restore finishes, drops the document and clears the
// colony-only copies.
func TestHubColonyOnlyRestoreAcceptsMatchingConsents(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	out := t.TempDir()
	rep, err := ExportHubCustomerStore(filepath.Dir(saasUsersDir), out, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customerConsentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rep.Consent, "ada-example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customerConsentDir(), "ada-example.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	st := useHubCustomerStore(t, hubCustomerStoreColony)
	if ok, _ := hubColonyOnlyMigrated(st); ok {
		t.Fatal("the colony-only document survived a restore over the exported files")
	}
	if _, ok := colonyPerson(t, st, users[0]).Attachments[crmAttachRecord]; ok {
		t.Error("the colony-only copy outlived the restore")
	}
	if _, err := os.Stat(filepath.Join(customerConsentDir()+hubRestoredAsideSuffix, "ada-example.json")); err != nil {
		t.Errorf("the matching consents directory was not kept aside: %v", err)
	}
	if _, err := os.Stat(customerConsentDir() + hubRestoringSuffix); !os.IsNotExist(err) {
		t.Errorf("the restore left its staging directory: %v", err)
	}
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.RealName {
		t.Errorf("consent after the restore = %+v", c)
	}
	if u := loadSaaSUser("ada-example"); u.CRMInColony || u.FullName != "Ada Example" {
		t.Errorf("user after the restore = %+v", u)
	}
}

// An older consents directory is set aside by a rollback, never
// overwritten, and the records come back from colony.
func TestHubColonyOnlyRestoreSetsAsideOlderConsents(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	at := hubMigratedAt(t, st)
	closeHubCustomerStore()
	writeReappearedConsents(t, `{}`, at.Add(-time.Hour))
	useHubCustomerStore(t, hubCustomerStoreColony)
	if data, err := os.ReadFile(filepath.Join(customerConsentDir()+hubRestoredAsideSuffix, "ada-example.json")); err != nil || string(data) != `{}` {
		t.Fatalf("the older consents directory was not kept aside: %v", err)
	}
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.RealName {
		t.Errorf("consent after the restore = %+v", c)
	}
}

// A migration that crashed after renaming the consents directory, before
// the document, is undone on the next boot: the directory comes back and
// no boot reads the consents as gone.
func TestHubColonyOnlyRecoversInterruptedMigration(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	to := customerConsentDir() + hubMigratedSuffix
	note, _ := json.Marshal(hubColonyOnlyPending{Version: crmOnlyVersion, ConsentsTo: to})
	if err := st.SetStoreAttachment(crmAttachPending, note); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	if err := os.Rename(customerConsentDir(), to); err != nil {
		t.Fatal(err)
	}

	st = useHubCustomerStore(t, hubCustomerStoreColony)
	if _, err := os.Stat(filepath.Join(customerConsentDir(), "ada-example.json")); err != nil {
		t.Fatalf("the consents directory was not put back: %v", err)
	}
	if _, ok, _ := st.StoreAttachment(crmAttachPending); ok {
		t.Error("the pending note survived the recovery")
	}
	if c, ok := colonyPerson(t, st, users[0]).Consent("own"); !ok || len(c.Fields) == 0 {
		t.Errorf("the boot sync after the recovery lost the consent: %+v, %v", c, ok)
	}
}

// With both the renamed and a new consents directory, nothing is chosen.
func TestHubColonyOnlyInterruptedMigrationWithBothDirsRefused(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	to := customerConsentDir() + hubMigratedSuffix
	note, _ := json.Marshal(hubColonyOnlyPending{Version: crmOnlyVersion, ConsentsTo: to})
	if err := st.SetStoreAttachment(crmAttachPending, note); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	if err := os.Mkdir(to, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q, want json", got)
	}
	closeHubCustomerStore()
	if _, err := os.Stat(to); err != nil {
		t.Errorf("the renamed directory was touched: %v", err)
	}
}

// A pending note beside a finished migration is only dropped.
func TestHubColonyOnlyPendingAfterFinishedMigration(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	note, _ := json.Marshal(hubColonyOnlyPending{Version: crmOnlyVersion, ConsentsTo: migratedDir("consents")})
	if err := st.SetStoreAttachment(crmAttachPending, note); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	st = useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if _, err := os.Stat(customerConsentDir()); !os.IsNotExist(err) {
		t.Errorf("a finished migration was undone: %v", err)
	}
	if _, ok, _ := st.StoreAttachment(crmAttachPending); ok {
		t.Error("the pending note survived")
	}
}

// A migration that fails after writing the records clears them and the
// pending note, leaves the consents directory in place, and removes only
// the users copy it made.
func TestHubColonyOnlyFailedMigrationClearsWhatItWrote(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	// Both names for the consents copy are taken: the migration cannot
	// rename the directory and must undo everything.
	for _, name := range []string{customerConsentDir() + hubMigratedSuffix, customerConsentDir() + hubMigratedSuffix + "-" + now.Format(hubMigratedStamp)} {
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateHubColonyOnly(st, now); err == nil {
		t.Fatal("the migration did not fail")
	}
	for _, u := range users {
		if p, err := st.Get(crmPersonID(u)); err == nil {
			if _, ok := p.Attachments[crmAttachRecord]; ok {
				t.Errorf("a failed migration left a colony-only record for %s", u.GitHubUsername)
			}
		}
	}
	if ok, _ := hubColonyOnlyMigrated(st); ok {
		t.Error("a failed migration wrote the document")
	}
	if _, ok, _ := st.StoreAttachment(crmAttachPending); ok {
		t.Error("a failed migration left the pending note")
	}
	if _, err := os.Stat(filepath.Join(customerConsentDir(), "ada-example.json")); err != nil {
		t.Errorf("a failed migration moved the consents: %v", err)
	}
	if _, err := os.Stat(migratedDir("users")); !os.IsNotExist(err) {
		t.Errorf("a failed migration left its users copy: %v", err)
	}
}

// A rename that fails after the users copy is undone the same way: the
// copy the migration made goes, the records and the pending note are
// cleared, and the consents directory stays where it was.
func TestHubColonyOnlyFailedRenameUndoesCopy(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	orig := hubMigrateRename
	hubMigrateRename = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { hubMigrateRename = orig })
	if err := migrateHubColonyOnly(st, time.Now()); err == nil {
		t.Fatal("the migration did not fail")
	}
	if _, err := os.Stat(migratedDir("users")); !os.IsNotExist(err) {
		t.Errorf("a failed migration left its users copy: %v", err)
	}
	if _, ok, _ := st.StoreAttachment(crmAttachPending); ok {
		t.Error("a failed migration left the pending note")
	}
	if ok, _ := hubColonyOnlyMigrated(st); ok {
		t.Error("a failed migration wrote the document")
	}
	if _, ok := colonyPerson(t, st, users[0]).Attachments[crmAttachRecord]; ok {
		t.Error("a failed migration left a colony-only record")
	}
	if _, err := os.Stat(filepath.Join(customerConsentDir(), "ada-example.json")); err != nil {
		t.Errorf("the consents directory moved: %v", err)
	}
}

// Removing the consents of a person colony holds no record for succeeds:
// a delete must never be blocked by nothing to remove.
func TestHubColonyOnlyRemoveConsentsWithoutRecord(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	// Written beside the store, not through it: colony holds nothing.
	fresh := &SaaSUser{GitHubUsername: "fresh-example", Hives: map[string]string{}}
	data, _ := json.Marshal(fresh)
	if err := os.WriteFile(filepath.Join(saasUsersDir, "fresh-example.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := customerConsentPath(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeCustomerConsents(path); err != nil {
		t.Errorf("removing nothing failed: %v", err)
	}
}

// A deleted user's copies go from every directory colony-only sets aside.
func TestScrubHubMigratedSetAsideDirs(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	saasDir := filepath.Dir(saasUsersDir)
	var dirs []string
	for _, name := range []string{"consents" + hubIgnoredSuffix, "consents" + hubRestoredAsideSuffix + "-x"} {
		d := filepath.Join(saasDir, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"ada-example.json", "bob-example.json"} {
			if err := os.WriteFile(filepath.Join(d, f), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dirs = append(dirs, d)
	}
	if err := scrubHubMigrated(users[0]); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "ada-example.json")); !os.IsNotExist(err) {
			t.Errorf("%s still holds the deleted user: %v", filepath.Base(d), err)
		}
		if _, err := os.Stat(filepath.Join(d, "bob-example.json")); err != nil {
			t.Errorf("another user's copy was removed from %s: %v", filepath.Base(d), err)
		}
	}
}

// A record rewritten without crm_in_colony (an image that predates
// colony-only saves it with empty CRM fields) never empties the colony copy:
// colony fills every field the JSON lacks, in reads, saves and a restore.
func TestHubColonyOnlyFlaglessRewriteKeepsColonyFields(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
		t.Fatal(err)
	}
	// What an older image writes: account data only, no flag.
	raw, err := readHubUserForSync(mustUserPath(t, users[0]))
	if err != nil {
		t.Fatal(err)
	}
	raw.CRMInColony = false
	raw.LastLogin = "2026-11-25T00:00:00Z"
	data, _ := json.Marshal(raw)
	if err := os.WriteFile(mustUserPath(t, users[0]), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadSaaSUser("ada-example")
	if got.FullName != "Ada Example" || got.Notes != users[0].Notes || got.Company != "Example Org" {
		t.Fatalf("read of a flagless rewrite = %+v", got)
	}
	// A raw save (no overlay) must not empty colony either.
	raw2, _ := readHubUserForSync(mustUserPath(t, users[0]))
	if err := saveSaaSUser(raw2); err != nil {
		t.Fatal(err)
	}
	if rec, ok, _ := crmGetRecord(st, crmPersonID(users[0])); !ok || rec.Fields.FullName != "Ada Example" || !rec.Stripped {
		t.Fatalf("colony record after a flagless raw save = %+v, %v", rec, ok)
	}
	// A value the JSON holds still wins.
	got = loadSaaSUser("ada-example")
	got.Company = "Changed Org"
	if err := saveSaaSUser(got); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	// Rewritten without the flag again, then rolled back: the restore
	// writes colony's fields into the JSON before dropping colony's copy.
	raw3, _ := readHubUserForSync(mustUserPath(t, users[0]))
	raw3.CRMInColony = false
	data, _ = json.Marshal(raw3)
	if err := os.WriteFile(mustUserPath(t, users[0]), data, 0o644); err != nil {
		t.Fatal(err)
	}
	useHubCustomerStore(t, hubCustomerStoreColony)
	onDisk := userFileJSON(t, users[0])
	if onDisk["full_name"] != "Ada Example" || onDisk["company"] != "Changed Org" || onDisk["notes"] != users[0].Notes {
		t.Errorf("restored record = %v", onDisk)
	}
}

// While colony-only is not running, a CRM change to a stripped record is
// refused (colony-only would later read over it); an account-only save of
// the same record goes through.
func TestHubColonyOnlyFallbackRefusesCRMEdits(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore() // a fallback: json mode with no store open
	u := loadSaaSUser("ada-example")
	if !u.CRMInColony {
		t.Fatal("fixture: record not stripped")
	}
	u.LastLogin = "2026-11-25T00:00:00Z"
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("account-only save refused: %v", err)
	}
	u.Company = "Fallback Org"
	if err := saveSaaSUser(u); !errors.Is(err, errCRMHeldInColony) {
		t.Fatalf("CRM edit in a fallback = %v, want errCRMHeldInColony", err)
	}
}

// A delete while colony.db exists but is not open is refused, so the
// person is never left in colony to come back on a new sign-up.
func TestHubColonyOnlyDeleteRefusedWithStoreClosed(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()
	srv := newHandlerHub()
	req := httptest.NewRequest(http.MethodDelete, "/api/saas/admin/users/ada-example", nil)
	req.SetPathValue("username", "ada-example")
	w := httptest.NewRecorder()
	srv.handleAdminDeleteUser(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete with the store closed = %d, want 503", w.Code)
	}
	if loadSaaSUser("ada-example") == nil {
		t.Fatal("the user was deleted without forgetting them in colony")
	}
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	deleteAdminUser(t, srv, "ada-example")
	if _, err := st.Get(crmPersonID(users[0])); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user still in colony: %v", err)
	}
}

// A consents directory left part-written (only colony's own files) is not
// a conflict, so a crashed restore never wedges the next boot.
func TestHubColonyOnlyPartialRestoreDirAccepted(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	_, consents, err := hubCustomerFiles(st, saasUsersDir)
	if err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	if err := os.MkdirAll(customerConsentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customerConsentDir(), "ada-example.json"), consents["ada-example.json"], 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover staging directory from the crash is the restore's own.
	if err := os.MkdirAll(customerConsentDir()+hubRestoringSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	useHubCustomerStore(t, hubCustomerStoreColony)
	if c := readMatchConsent(t, users[0], "own"); !c.Fields.RealName {
		t.Errorf("consent after the restore = %+v", c)
	}
}

// A stripped record whose colony copy is gone (a delete whose last step
// failed) does not block the rollback: it is restored without CRM fields.
func TestHubColonyOnlyRestoreSkipsMissingCopy(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	if err := saveSaaSUser(loadSaaSUser("ada-example")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Forget(crmPersonID(users[0])); err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()
	st = useHubCustomerStore(t, hubCustomerStoreColony)
	if ok, _ := hubColonyOnlyMigrated(st); ok {
		t.Fatal("the rollback did not finish")
	}
	if onDisk := userFileJSON(t, users[0]); onDisk["crm_in_colony"] == true {
		t.Errorf("record still flagged after the rollback: %v", onDisk)
	}
}

// The export never writes over the live hub data directory.
func TestHubColonyOnlyExportRefusesInPlace(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()
	dir := filepath.Dir(saasUsersDir)
	if _, err := ExportHubCustomerStore(dir, dir+string(filepath.Separator), true); err == nil {
		t.Fatal("an export over the live files was allowed")
	}
}
