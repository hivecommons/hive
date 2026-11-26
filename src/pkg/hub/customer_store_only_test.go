package hub

// Tests for HIVE_HUB_CUSTOMER_STORE=colony-only (#19972). Every person here
// is synthetic.

import (
	"context"
	"encoding/json"
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

// A consents directory that reappears beside a colony-only store is set
// aside on the next boot, never read as the records.
func TestHubColonyOnlySetsAsideReappearedConsents(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreColonyOnly)
	closeHubCustomerStore()
	if err := os.MkdirAll(customerConsentDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(customerConsentDir(), "ada-example.json")
	if err := os.WriteFile(stale, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
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
