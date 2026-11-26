package hub

// Tests for the hub CRM fields on the colony store (#19856). Every person
// here is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/colony/store"
)

// useHubCustomerStore opens the store in mode over the current temp dirs and
// closes it when the test ends.
func useHubCustomerStore(t *testing.T, mode string) *store.Store {
	t.Helper()
	got := openHubCustomerStore(context.Background(), nil, mode)
	t.Cleanup(closeHubCustomerStore)
	if got != mode {
		t.Fatalf("store opened in %q mode, want %q", got, mode)
	}
	st, _ := hubCRM.active()
	if mode != hubCustomerStoreJSON && st == nil {
		t.Fatal("store not open")
	}
	return st
}

func colonyPerson(t *testing.T, st *store.Store, u *SaaSUser) store.Person {
	t.Helper()
	p, err := st.Get(crmPersonID(u))
	if err != nil {
		t.Fatalf("colony person %s: %v", crmPersonID(u), err)
	}
	return p
}

func effectiveFact(t *testing.T, p store.Person, field string) store.Fact {
	t.Helper()
	f, ok := p.Effective(field)
	if !ok {
		t.Fatalf("no effective %s fact", field)
	}
	return f
}

// fixtureUsers writes a synthetic /data/saas/users directory covering every
// provenance the hub records, plus one consent file.
func fixtureUsers(t *testing.T) []*SaaSUser {
	t.Helper()
	users := []*SaaSUser{
		{
			GitHubUsername: "ada-example", CreatedAt: "2026-01-02T03:04:05Z", Hives: map[string]string{"own": "viewer"},
			FullName: "Ada Example", FullNameSource: countrySourceUser,
			Company: "Example Org", CompanySource: countrySourceAdmin,
			Country: "GB", CountrySource: countrySourceUser, CountrySetByUser: true,
			Affiliation: "example-org", AffiliationSource: affiliationSourceCompany,
			SlackID: "U000EXAMPLE", Notes: "met at a synthetic conference",
			Email: "ada@corp.example", ProfileLocation: "Example City", ProfileBio: "synthetic bio",
			TopRepo: "example-org/widget", TopRepoURL: "https://github.com/example-org/widget", PublicActivity: publicActivityActive,
		},
		{
			// Legacy record: values with no recorded source, inferred country.
			GitHubUsername: "bob-example", Hives: map[string]string{},
			FullName: "Bob Legacy", Company: "Legacy Co", Country: "FR",
			Affiliation: "example-gitdm", AffiliationSource: "colony:gitdm", AffiliationConfidence: "high",
			CNCFContributions: 42,
		},
		{
			// Non-GitHub primary with an explicit clear of the organization.
			GitHubUsername: "google:1078", CanonicalID: "google:1078", Provider: "google",
			Company: "", CompanySource: countrySourceUser,
			Affiliation: "Example IdP", AffiliationSource: affiliationSourceIDP,
			Email: "carol@gmail.com",
		},
	}
	for _, u := range users {
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
	}
	writeMatchConsent(t, users[0], "own", func(c *customerConsent) {
		c.Fields = customerConsentFields{RealName: true, Country: true}
	})
	return users
}

func TestCRMFactsFromUserProvenance(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	byField := func(u *SaaSUser) map[string]store.Fact {
		out := map[string]store.Fact{}
		for _, f := range crmFactsFromUser(u) {
			if _, dup := out[f.Field]; dup {
				t.Fatalf("two facts for %s: one per field expected", f.Field)
			}
			out[f.Field] = f
		}
		return out
	}
	ada := byField(users[0])
	cases := []struct {
		field    string
		rank     store.Rank
		source   string
		locality store.Locality
	}{
		{store.FieldRealName, store.RankUser, crmSourceUser, store.LocalityShareable},
		{store.FieldOrganization, store.RankAdmin, crmSourceAdmin, store.LocalityShareable},
		{store.FieldCountry, store.RankUser, crmSourceUser, store.LocalityShareable},
		{store.FieldAffiliation, store.RankEnrichment, "hub:" + affiliationSourceCompany, store.LocalityShareable},
		{store.FieldSlackID, store.RankAdmin, crmSourceAdmin, store.LocalityLocalOnly},
		{store.FieldNotes, store.RankAdmin, crmSourceAdmin, store.LocalityLocalOnly},
		{store.FieldLocation, store.RankEnrichment, crmSourceGitHubProfile, store.LocalityLocalOnly},
		{crmFieldBio, store.RankEnrichment, crmSourceGitHubProfile, store.LocalityLocalOnly},
		{crmFieldTopRepo, store.RankEnrichment, crmSourceTopRepo, store.LocalityLocalOnly},
	}
	for _, c := range cases {
		f, ok := ada[c.field]
		if !ok || f.Rank != c.rank || f.Source != c.source || f.Locality != c.locality {
			t.Errorf("%s = %+v, want rank %s source %s locality %s", c.field, f, c.rank, c.source, c.locality)
		}
	}

	bob := byField(users[1])
	if f := bob[store.FieldRealName]; f.Rank != store.RankAdmin || f.Source != crmSourceUnrecorded {
		t.Errorf("legacy name = %+v, want admin rank, unrecorded source", f)
	}
	if f := bob[store.FieldCountry]; f.Rank != store.RankEnrichment || f.Source != crmSourceBrowserLanguage {
		t.Errorf("inferred country = %+v, want enrichment from browser_language", f)
	}
	if f := bob[store.FieldAffiliation]; f.Source != "colony:gitdm" || f.Confidence != "high" || f.Locality != store.LocalityShareable {
		t.Errorf("colony affiliation = %+v", f)
	}

	carol := byField(users[2])
	if f, ok := carol[store.FieldOrganization]; !ok || f.Value != "" || f.Rank != store.RankUser {
		t.Errorf("explicit clear = %+v (present %v), want an empty user-rank fact", f, ok)
	}
	if f := carol[store.FieldAffiliation]; f.Locality != store.LocalityLocalOnly {
		t.Errorf("identity-provider affiliation must stay local_only: %+v", f)
	}
	if p, ok := crmPrimaryIdentity(users[2]); !ok || p.Kind != store.KindOIDC || p.Locality != store.LocalityLocalOnly {
		t.Errorf("oidc primary = %+v", p)
	}
}

// A colony read of what the mirror wrote rebuilds the record byte for byte.
func TestCRMOverlayRoundTrip(t *testing.T) {
	defer helperSetupTempDirs(t)()
	for _, u := range fixtureUsers(t) {
		before, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		var cp SaaSUser
		if err := json.Unmarshal(before, &cp); err != nil {
			t.Fatal(err)
		}
		crmOverlay(&cp, store.Person{Facts: crmFactsFromUser(u)})
		after, _ := json.Marshal(&cp)
		if string(after) != string(before) {
			t.Errorf("%s: overlay changed the record\n got %s\nwant %s", u.GitHubUsername, after, before)
		}

		// Starting from an empty CRM record, the overlay restores every
		// moved field. A legacy inferred country (bob) restores with its
		// source made explicit, which is the same precedence, so it is
		// checked by value instead.
		if u.GitHubUsername == "bob-example" {
			bare := SaaSUser{GitHubUsername: u.GitHubUsername, Hives: u.Hives}
			crmOverlay(&bare, store.Person{Facts: crmFactsFromUser(u)})
			if bare.Country != u.Country || effectiveCountrySource(&bare) != effectiveCountrySource(u) ||
				bare.Company != u.Company || bare.CompanySource != "" || bare.CNCFContributions != u.CNCFContributions {
				t.Errorf("legacy restore = %+v", bare)
			}
			continue
		}
		bare := SaaSUser{GitHubUsername: u.GitHubUsername, CanonicalID: u.CanonicalID, Provider: u.Provider,
			CreatedAt: u.CreatedAt, Hives: u.Hives, Email: u.Email}
		crmOverlay(&bare, store.Person{Facts: crmFactsFromUser(u)})
		restored, _ := json.Marshal(&bare)
		if string(restored) != string(before) {
			t.Errorf("%s: overlay did not restore the CRM fields\n got %s\nwant %s", u.GitHubUsername, restored, before)
		}
	}
}

func TestHubCustomerStoreImportFromFixtures(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreShadow)

	rec, ok, err := st.Migration(hubCustomerStoreMigration)
	if err != nil || !ok || rec.Fingerprint == "" || rec.Report.People != len(users) {
		t.Fatalf("migration record = %+v ok=%v err=%v", rec, ok, err)
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.People != len(users) || stats.Consents != 1 {
		t.Fatalf("stats = %+v, want %d people and 1 consent", stats, len(users))
	}
	ada := colonyPerson(t, st, users[0])
	if f := effectiveFact(t, ada, store.FieldCountry); f.Value != "GB" || f.Rank != store.RankUser {
		t.Fatalf("country = %+v", f)
	}
	if c, ok := ada.Consent("own"); !ok || !c.Covers(store.FieldRealName) || c.Covers(store.FieldOrganization) {
		t.Fatalf("consent = %+v", c)
	}
	// A private-domain email is a local_only identity.
	for _, i := range ada.Identities {
		if i.Kind.IsEmail() && i.Locality != store.LocalityLocalOnly {
			t.Fatalf("email identity is %s", i.Locality)
		}
	}
	// The migration never rewrites the JSON.
	if got := loadSaaSUser("ada-example"); got.Notes != users[0].Notes || got.Company != users[0].Company {
		t.Fatalf("json changed: %+v", got)
	}
}

func TestHubCustomerStoreBootSyncIdempotent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreShadow)
	first, err := st.Export()
	if err != nil {
		t.Fatal(err)
	}
	closeHubCustomerStore()

	for i := 0; i < hubCustomerStoreBackupsKept+2; i++ {
		st = useHubCustomerStore(t, hubCustomerStoreShadow)
		again, err := st.Export()
		if err != nil {
			t.Fatal(err)
		}
		if len(again.People) != len(first.People) {
			t.Fatalf("boot %d: %d people, want %d", i, len(again.People), len(first.People))
		}
		for j := range first.People {
			a, b := first.People[j], again.People[j]
			if a.ID != b.ID || len(a.Facts) != len(b.Facts) || len(a.Identities) != len(b.Identities) || len(a.Consents) != len(b.Consents) {
				t.Fatalf("boot %d changed %s", i, a.ID)
			}
			for k := range a.Facts {
				if !crmFactEqual(a.Facts[k], b.Facts[k]) || !a.Facts[k].At.Equal(b.Facts[k].At) {
					t.Fatalf("boot %d rewrote fact %s of %s", i, a.Facts[k].Field, a.ID)
				}
			}
		}
		closeHubCustomerStore()
	}
	// Unchanged boots write one backup, not one per boot, so they never
	// rotate distinct backups out.
	backups, _ := filepath.Glob(hubCustomerStorePath() + hubCustomerStoreBackupPrefix + "*")
	if len(backups) != 1 {
		t.Fatalf("%d backups after unchanged boots, want 1", len(backups))
	}
	// Each change makes a new backup; only the newest few are kept.
	for i := 0; i < hubCustomerStoreBackupsKept+1; i++ {
		st = useHubCustomerStore(t, hubCustomerStoreShadow)
		u := loadSaaSUser("bob-example")
		u.Notes = "synthetic note " + strings.Repeat("x", i+1)
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
		closeHubCustomerStore()
	}
	useHubCustomerStore(t, hubCustomerStoreShadow)
	backups, _ = filepath.Glob(hubCustomerStorePath() + hubCustomerStoreBackupPrefix + "*")
	if len(backups) != hubCustomerStoreBackupsKept {
		t.Fatalf("%d backups kept, want %d", len(backups), hubCustomerStoreBackupsKept)
	}
	for _, b := range backups {
		fi, err := os.Stat(b)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != hubCustomerStoreBackupMode {
			t.Fatalf("backup mode %v", fi.Mode().Perm())
		}
	}
	_ = users
}

// A burst of people without a JSON record is held, not erased.
func TestHubCustomerStoreBootHoldsMassOrphans(t *testing.T) {
	defer helperSetupTempDirs(t)()
	var users []*SaaSUser
	for i := 0; i < hubCustomerStoreOrphanFloor+2; i++ {
		u := &SaaSUser{GitHubUsername: "orphan-" + strings.Repeat("a", i+1), Hives: map[string]string{}, Company: "Synthetic Co", CompanySource: countrySourceUser}
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	useHubCustomerStore(t, hubCustomerStoreShadow)
	closeHubCustomerStore()
	for _, u := range users {
		for _, p := range saaSUserFilePaths(u.GitHubUsername) {
			_ = os.Remove(p)
		}
	}
	st := useHubCustomerStore(t, hubCustomerStoreShadow)
	if stats, err := st.Stats(); err != nil || stats.People != len(users) || stats.Tombstones != 0 {
		t.Fatalf("mass orphan pass erased people: %+v %v", stats, err)
	}
	// A missing users directory fails closed to json and erases nobody.
	closeHubCustomerStore()
	if err := os.RemoveAll(saasUsersDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreShadow); got != hubCustomerStoreJSON {
		t.Fatalf("missing users dir: mode %q, want json", got)
	}
}

// A person whose colony copy may be behind the JSON reads from the JSON.
func TestHubCustomerStoreDirtyPersonReadsJSON(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	if _, err := st.SetFact(crmPersonID(users[1]), store.Fact{Field: store.FieldOrganization, Value: "Stale Co",
		Rank: store.RankUser, Source: crmSourceUser, Locality: store.LocalityShareable}); err != nil {
		t.Fatal(err)
	}
	hubCRM.mu.Lock()
	hubCRM.markCRM(crmPersonID(users[1]), errCRMNotStored)
	hubCRM.mu.Unlock()
	if got := loadSaaSUser("bob-example"); got.Company != users[1].Company {
		t.Fatalf("a dirty person read colony: %q", got.Company)
	}
	// The next successful mirror makes colony match the JSON again.
	u := loadSaaSUser("bob-example")
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	if hubCRM.isDirty(crmPersonID(users[1])) {
		t.Fatal("person still dirty after a successful mirror")
	}
	if got := loadSaaSUser("bob-example"); got.Company != users[1].Company {
		t.Fatalf("colony read after resync = %q", got.Company)
	}
}

// Two saves of one person in either mirror order leave colony holding the
// JSON that is on disk.
func TestHubCustomerStoreMirrorUsesDiskCopy(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreShadow)
	older := loadSaaSUser("bob-example")
	newer := loadSaaSUser("bob-example")
	newer.Company, newer.CompanySource = "Newer Co", countrySourceAdmin
	if err := saveSaaSUser(newer); err != nil {
		t.Fatal(err)
	}
	older.Company = "Older Co"
	crmMirrorUser(older) // a late mirror carrying a stale copy
	if f := effectiveFact(t, colonyPerson(t, st, users[1]), store.FieldOrganization); f.Value != "Newer Co" {
		t.Fatalf("a stale mirror won: %+v", f)
	}
}

// A person who cleared their organization gets no derived affiliation.
func TestCustomerMatchAffiliationRespectsOrganizationClear(t *testing.T) {
	u := &SaaSUser{GitHubUsername: "fay-example", Hives: map[string]string{"own": "viewer"},
		Affiliation: "derived-org", AffiliationSource: affiliationSourceContributions}
	if e, ok := customerMatchAffiliation(customerMatchEntry{}, u, "own"); !ok || e.Affiliation != "derived-org" {
		t.Fatalf("public affiliation = %+v %v", e, ok)
	}
	u.CompanySource = countrySourceUser
	if e, ok := customerMatchAffiliation(customerMatchEntry{}, u, "own"); ok || e.Affiliation != "" {
		t.Fatalf("affiliation sent after an organization clear: %+v", e)
	}
	if _, ok := customerMatchAffiliation(customerMatchEntry{}, u, "other"); ok {
		t.Fatal("affiliation for a non-grant-holder")
	}
}

func TestHubCustomerStoreShadowDualWrite(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreShadow)

	u := loadSaaSUser("bob-example")
	u.Company, u.CompanySource = "New Co", countrySourceAdmin
	u.Notes = "second synthetic note"
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	bob := colonyPerson(t, st, users[1])
	if f := effectiveFact(t, bob, store.FieldOrganization); f.Value != "New Co" || f.Rank != store.RankAdmin || f.Source != crmSourceAdmin {
		t.Fatalf("organization = %+v", f)
	}
	if f := effectiveFact(t, bob, store.FieldNotes); f.Locality != store.LocalityLocalOnly {
		t.Fatalf("notes locality = %s", f.Locality)
	}
	// One fact per field: the unrecorded admin-rank record was replaced.
	n := 0
	for _, f := range bob.Facts {
		if f.Field == store.FieldOrganization {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d organization facts, want 1", n)
	}

	// A consent written after boot is mirrored too.
	writeMatchConsent(t, users[1], "own", func(c *customerConsent) { c.Fields = customerConsentFields{Country: true} })
	if c, ok := colonyPerson(t, st, users[1]).Consent("own"); !ok || !c.Covers(store.FieldCountry) {
		t.Fatalf("consent not mirrored: %+v", c)
	}
	// Removing every consent withdraws it in colony.
	path, _ := customerConsentPath(users[1])
	if err := saveCustomerConsents(path, map[string]customerConsent{}); err != nil {
		t.Fatal(err)
	}
	if c, ok := colonyPerson(t, st, users[1]).Consent("own"); !ok || c.Active() {
		t.Fatalf("removed consent still active in colony: %+v", c)
	}
}

func putAdminUser(t *testing.T, srv *HubServer, username, body string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.handleAdminUpdateUser)
	req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/users/"+username, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin update: %d %s", w.Code, w.Body)
	}
}

// Admin edits land as admin-set facts, and never overwrite what the person
// set: a user-set country survives an admin edit and a login inference.
func TestHubCustomerStorePrecedenceAndAdminEdits(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	srv := newHandlerHub()

	putAdminUser(t, srv, "ada-example", `{"country":"DE","company":"Admin Co","slack_id":"U000OTHER"}`)
	ada := colonyPerson(t, st, users[0])
	if f := effectiveFact(t, ada, store.FieldCountry); f.Value != "GB" || f.Rank != store.RankUser {
		t.Fatalf("an admin edit overwrote a user-set country: %+v", f)
	}
	if f := effectiveFact(t, ada, store.FieldOrganization); f.Value != "Admin Co" || f.Rank != store.RankAdmin {
		t.Fatalf("admin company = %+v", f)
	}
	if f := effectiveFact(t, ada, store.FieldSlackID); f.Value != "U000OTHER" || f.Rank != store.RankAdmin {
		t.Fatalf("admin slack id = %+v", f)
	}

	// Login inference never replaces a user-set country.
	u := loadSaaSUser("ada-example")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "fr-FR")
	if applyInferredCountry(u, r) {
		t.Fatal("inference replaced a user-set country")
	}
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	if f := effectiveFact(t, colonyPerson(t, st, users[0]), store.FieldCountry); f.Value != "GB" || f.Rank != store.RankUser {
		t.Fatalf("country after login = %+v", f)
	}

	// The store itself refuses to let a weaker rank win.
	res, err := st.SetFact(crmPersonID(users[0]), store.Fact{Field: store.FieldCountry, Value: "FR", Rank: store.RankAdmin,
		Source: crmSourceAdmin, Locality: store.LocalityShareable})
	if err != nil || res.Effective {
		t.Fatalf("admin-rank fact became effective over a user-rank one: %+v %v", res, err)
	}

	// An admin may set a country the user never chose.
	putAdminUser(t, srv, "bob-example", `{"country":"ES"}`)
	if f := effectiveFact(t, colonyPerson(t, st, users[1]), store.FieldCountry); f.Value != "ES" || f.Rank != store.RankAdmin || f.Source != crmSourceAdmin {
		t.Fatalf("admin country = %+v", f)
	}
}

// Colony mode reads from colony and keeps writing the JSON, so setting json
// again loses nothing.
func TestHubCustomerStoreColonyReadsAndRollback(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)

	// A value only colony holds is what a colony-mode read returns.
	if _, err := st.SetFact(crmPersonID(users[1]), store.Fact{Field: store.FieldOrganization, Value: "Colony Co",
		Rank: store.RankUser, Source: crmSourceUser, Locality: store.LocalityShareable}); err != nil {
		t.Fatal(err)
	}
	if got := loadSaaSUser("bob-example"); got.Company != "Colony Co" || got.CompanySource != countrySourceUser {
		t.Fatalf("colony read = %q/%q", got.Company, got.CompanySource)
	}
	// A person colony does not hold reads from the JSON.
	if err := os.WriteFile(filepath.Join(saasUsersDir, "dave-example.json"),
		[]byte(`{"github_username":"dave-example","hives":{},"company":"Json Co","company_source":"user"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadSaaSUser("dave-example"); got == nil || got.Company != "Json Co" {
		t.Fatalf("dual-read fallback = %+v", got)
	}

	// A write in colony mode lands in the JSON too.
	putAdminUser(t, newHandlerHub(), "bob-example", `{"notes":"rollback synthetic note"}`)
	closeHubCustomerStore()
	useHubCustomerStore(t, hubCustomerStoreJSON)
	got := loadSaaSUser("bob-example")
	if got.Notes != "rollback synthetic note" || got.Company != "Colony Co" {
		t.Fatalf("after rollback to json: notes %q company %q", got.Notes, got.Company)
	}
}

func deleteAdminUser(t *testing.T, srv *HubServer, username string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/saas/admin/users/"+username, nil)
	req.SetPathValue("username", username)
	w := httptest.NewRecorder()
	srv.handleAdminDeleteUser(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
}

func TestHubCustomerStoreForgetOnDelete(t *testing.T) {
	for _, mode := range []string{hubCustomerStoreShadow, hubCustomerStoreColony, hubCustomerStoreJSON} {
		t.Run(mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			users := fixtureUsers(t)
			// Create colony.db first, so json mode has one to erase from.
			useHubCustomerStore(t, hubCustomerStoreShadow)
			closeHubCustomerStore()
			st := useHubCustomerStore(t, mode)
			if st == nil {
				t.Fatal("json mode must open an existing colony.db for erasure")
			}

			deleteAdminUser(t, newHandlerHub(), "ada-example")
			if _, err := st.Get(crmPersonID(users[0])); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("deleted user still in colony: %v", err)
			}
			if tomb, err := st.IsTombstoned(store.IdentityRef{Kind: store.KindGitHub, Value: "ada-example"}); err != nil || !tomb {
				t.Fatalf("no tombstone for the deleted login: %v %v", tomb, err)
			}
			if loadSaaSUser("ada-example") != nil {
				t.Fatal("json record survived")
			}
		})
	}
}

// A user deleted while colony.db was not open is erased on the next boot.
func TestHubCustomerStoreBootErasesDeletedUsers(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	useHubCustomerStore(t, hubCustomerStoreShadow)
	closeHubCustomerStore()
	for _, p := range saaSUserFilePaths("bob-example") {
		_ = os.Remove(p)
	}
	// A record that exists but does not parse is never treated as deleted.
	if err := os.WriteFile(filepath.Join(saasUsersDir, "ada-example.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := useHubCustomerStore(t, hubCustomerStoreShadow)
	if _, err := st.Get(crmPersonID(users[1])); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user kept: %v", err)
	}
	if _, err := st.Get(crmPersonID(users[0])); err != nil {
		t.Fatalf("unparsable record erased: %v", err)
	}
}

// A colony.db the store cannot open fails closed to json and is untouched.
func TestHubCustomerStoreUnreadableFileFailsClosed(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	path := hubCustomerStorePath()
	junk := []byte("not a bbolt file, synthetic")
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q, want json", got)
	}
	if data, _ := os.ReadFile(path); string(data) != string(junk) {
		t.Fatal("unreadable colony.db was rewritten")
	}
	if got := loadSaaSUser("ada-example"); got == nil || got.FullName != "Ada Example" {
		t.Fatalf("json read after fail-closed = %+v", got)
	}
}

func TestParseHubCustomerStoreMode(t *testing.T) {
	for raw, want := range map[string]string{"": "json", "JSON": "json", " shadow ": "shadow", "colony": "colony", "colony-only": "json"} {
		if got, _ := parseHubCustomerStoreMode(raw); got != want {
			t.Errorf("%q -> %q, want %q", raw, got, want)
		}
	}
}

// Nothing local_only reaches a spoke: not through the match answer to a
// trusted hive with every switch on, not through colony's own Shareable
// export, and nothing at all reaches a hive outside the trust domain.
func TestHubCustomerStoreLocalOnlyNeverReachesSpokes(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")
	users := fixtureUsers(t)
	writeMatchConsent(t, users[0], "own", func(c *customerConsent) {
		c.Fields = customerConsentFields{RealName: true, Organization: true, Country: true}
	})
	ghe := saveMatchUser(t, &SaaSUser{GitHubUsername: "erin-example", Hives: map[string]string{"own": "viewer", "third": "viewer"},
		Affiliation: "Example GHE", AffiliationSource: affiliationSourceGHE, Notes: "synthetic erin note"})
	st := useHubCustomerStore(t, hubCustomerStoreColony)

	secrets := []string{users[0].SlackID, users[0].Notes, users[0].Email, "corp.example", users[0].ProfileLocation,
		users[0].ProfileBio, ghe.Notes, ghe.Affiliation}

	resp := h.matchLabeled("own", matchHash("own", "ada-example"), matchHash("own", "erin-example"))
	body, _ := json.Marshal(resp)
	for _, s := range secrets {
		if strings.Contains(string(body), s) {
			t.Fatalf("a local_only value reached a trusted hive: %s", body)
		}
	}
	var ada *customerMatchEntry
	for i := range resp.Matches {
		if resp.Matches[i].MatchHash == matchHash("own", "ada-example") {
			ada = &resp.Matches[i]
		}
	}
	if ada == nil || ada.Affiliation != users[0].Affiliation || ada.Sources[customerMatchFieldAffiliation] != affiliationSourceCompany {
		t.Fatalf("trusted hive must hear the labeled public affiliation: %+v", resp.Matches)
	}

	// Outside the trust domain: nothing, with the store in colony mode.
	if third := h.matchLabeled("third", matchHash("third", "erin-example")); len(third.Matches) != 0 {
		t.Fatalf("an untrusted hive heard: %+v", third.Matches)
	}

	// Colony's own export for any spoke-bound path carries none of them.
	snap, err := st.Export(store.Shareable())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snap)
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Fatalf("Shareable export leaked a local_only value: %s", raw)
		}
	}
}
