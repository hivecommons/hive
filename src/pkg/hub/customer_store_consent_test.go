package hub

// Tests for the colony-mode consent reads (#19857). Every person here is
// synthetic.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/colony/store"
)

// matchOwn asks the match endpoint as hive "own" about ada-example.
func matchOwn(h *matchHarness) []customerMatchEntry {
	h.t.Helper()
	resetCustomerMatchRates()
	return h.answer(h.match("own", "own", matchHash("own", "ada-example"))).Matches
}

// In colony mode the match answer reads the grant from colony: a narrower
// grant there sends less, a withdrawal there sends nothing, and a person
// whose last sync failed is answered from the consent file alone.
func TestHubCustomerStoreColonyConsentReads(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")
	users := fixtureUsers(t) // ada-example consents to real name and country for "own"
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	id := crmPersonID(users[0])

	m := matchOwn(h)
	if len(m) != 1 || m[0].RealName != "Ada Example" || m[0].Country != "GB" {
		t.Fatalf("baseline answer = %+v, want real name and country", m)
	}
	granted := readMatchConsent(t, users[0], "own").GrantedAt

	// Colony narrows the grant to the real name: the country stops crossing
	// and is named as cleared, because it was delivered before.
	if err := st.SetConsent(id, store.Consent{Scope: "own", Fields: []string{store.FieldRealName},
		TextVersion: customerConsentTextVersion, GrantedAt: granted}); err != nil {
		t.Fatal(err)
	}
	m = matchOwn(h)
	if len(m) != 1 || m[0].RealName != "Ada Example" || m[0].Country != "" || !slices.Contains(m[0].Cleared, "country") {
		t.Fatalf("narrowed in colony = %+v, want real name only and country cleared", m)
	}

	// Colony holds a withdrawal. A person whose last sync failed is read from
	// the consent file, which still grants both fields.
	if err := st.SetConsent(id, store.Consent{Scope: "own", Fields: []string{store.FieldRealName, store.FieldCountry},
		TextVersion: customerConsentTextVersion, GrantedAt: granted, WithdrawnAt: granted.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	hubCRM.mu.Lock()
	hubCRM.markCRM(id, errCRMNotStored)
	hubCRM.mu.Unlock()
	if m = matchOwn(h); len(m) != 1 || m[0].RealName != "Ada Example" {
		t.Fatalf("dirty person = %+v, want the consent file's grant", m)
	}
	// The stamp of that answer re-mirrored the file over colony; withdraw
	// there again with the person clean.
	if err := st.SetConsent(id, store.Consent{Scope: "own", Fields: []string{store.FieldRealName, store.FieldCountry},
		TextVersion: customerConsentTextVersion, GrantedAt: granted, WithdrawnAt: granted.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	hubCRM.mu.Lock()
	hubCRM.markCRM(id, nil)
	hubCRM.mu.Unlock()
	m = matchOwn(h)
	if len(m) != 1 || m[0].RealName != "" || m[0].Country != "" || m[0].Organization != "" ||
		!slices.Contains(m[0].Cleared, "real_name") || !slices.Contains(m[0].Cleared, "country") {
		t.Fatalf("withdrawn in colony = %+v, want one entry with no values that clears real_name and country", m)
	}
}

// The Profile sharing page and the match index read through the overlay too.
func TestHubCustomerStoreColonyConsentPageAndIndex(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")
	users := fixtureUsers(t)
	// gus holds no grant on "own", so only his consent record can put him in
	// the index (grant holders are indexed without one, #19306).
	gus := saveMatchUser(t, &SaaSUser{GitHubUsername: "gus-example", Hives: map[string]string{},
		FullName: "Gus Example", FullNameSource: countrySourceUser})
	writeMatchConsent(t, gus, "own", func(c *customerConsent) { c.Fields = customerConsentFields{RealName: true} })
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	granted := readMatchConsent(t, users[0], "own").GrantedAt

	// The page shows the grant colony narrowed.
	if err := st.SetConsent(crmPersonID(users[0]), store.Consent{Scope: "own", Fields: []string{store.FieldCountry},
		TextVersion: customerConsentTextVersion, GrantedAt: granted}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.s.mux.ServeHTTP(w, reqWithUser(http.MethodGet, consentTestAPI, "", "ada-example"))
	if w.Code != http.StatusOK {
		t.Fatalf("consent page: %d %s", w.Code, w.Body)
	}
	var page struct {
		Hives []customerConsentHive `json:"hives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Hives) != 1 || page.Hives[0].Consent == nil ||
		page.Hives[0].Consent.Fields != (customerConsentFields{Country: true}) {
		t.Fatalf("consent page = %+v, want the narrowed grant (country only)", page.Hives)
	}

	// The index drops a never-delivered record withdrawn in colony.
	index := func() map[string]string {
		customerMatchStore.mu.Lock()
		customerMatchStore.indexes = nil
		customerMatchStore.mu.Unlock()
		customerConsentMu.Lock()
		defer customerConsentMu.Unlock()
		return h.s.customerMatchIndexFor("own", h.s.customerMatchKeys("own"), time.Now().UTC())
	}
	gusHash := matchHash("own", "gus-example")
	if _, ok := index()[gusHash]; !ok {
		t.Fatal("index misses a granted record")
	}
	gusGrant := readMatchConsent(t, gus, "own").GrantedAt
	if err := st.SetConsent(crmPersonID(gus), store.Consent{Scope: "own", Fields: []string{store.FieldRealName},
		TextVersion: customerConsentTextVersion, GrantedAt: gusGrant, WithdrawnAt: gusGrant.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := index()[gusHash]; ok {
		t.Fatal("index kept a never-delivered record withdrawn in colony")
	}
}

// The overlay lets colony narrow the grant, never widen it, and keeps the
// hub-only delivery bookkeeping from the consent file; it changes nothing outside colony mode,
// for a person colony does not hold, or for a hive id the file does not list.
func TestCRMOverlayConsents(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	granted := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	delivered := granted.Add(time.Hour)
	fileRecord := func() map[string]customerConsent {
		return map[string]customerConsent{"own": {
			Person: users[0].GitHubUsername, HiveID: "own", TextVersion: customerConsentTextVersion,
			Fields:    customerConsentFields{RealName: true, Country: true},
			GrantedAt: granted, UpdatedAt: granted,
			FirstDeliveredAt: &delivered, LastDeliveredAt: &delivered,
			DeliveredFields: &customerConsentFields{RealName: true, Country: true},
			DeliveredRev:    "rev-example",
		}}
	}
	colonyGrant := store.Consent{Scope: "own", Fields: []string{store.FieldOrganization},
		TextVersion: "text-example", GrantedAt: granted.Add(2 * time.Hour), WithdrawnAt: granted.Add(3 * time.Hour)}

	for _, mode := range []string{hubCustomerStoreJSON, hubCustomerStoreShadow} {
		st := useHubCustomerStore(t, mode)
		if st != nil {
			if err := st.SetConsent(crmPersonID(users[0]), colonyGrant); err != nil {
				t.Fatal(err)
			}
		}
		recs := fileRecord()
		crmOverlayConsents(users[0], recs)
		if got := recs["own"]; got.Fields != (customerConsentFields{RealName: true, Country: true}) || got.WithdrawnAt != nil {
			t.Fatalf("%s mode overlaid the consent: %+v", mode, got)
		}
		closeHubCustomerStore()
	}

	st := useHubCustomerStore(t, hubCustomerStoreColony)
	if err := st.SetConsent(crmPersonID(users[0]), colonyGrant); err != nil {
		t.Fatal(err)
	}
	recs := fileRecord()
	recs["other"] = customerConsent{HiveID: "other", Fields: customerConsentFields{Country: true}, GrantedAt: granted}
	crmOverlayConsents(users[0], recs)
	got := recs["own"]
	// The file grants real name and country, colony only organization: the
	// intersection is nothing. Colony's withdrawal and stale disclosure
	// version apply; the granted time stays the file's.
	if got.Fields != (customerConsentFields{}) || got.TextVersion != "text-example" ||
		!got.GrantedAt.Equal(granted) || got.WithdrawnAt == nil || !got.WithdrawnAt.Equal(colonyGrant.WithdrawnAt) {
		t.Fatalf("colony did not narrow the grant: %+v", got)
	}
	if got.DeliveredRev != "rev-example" || got.LastDeliveredAt == nil || !got.LastDeliveredAt.Equal(delivered) ||
		got.DeliveredFields == nil || *got.DeliveredFields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatalf("delivery bookkeeping lost: %+v", got)
	}
	if o := recs["other"]; o.Fields != (customerConsentFields{Country: true}) {
		t.Fatalf("a hive colony does not hold changed: %+v", o)
	}

	// Colony never widens the file: a withdrawal in the file survives a
	// colony grant, fields are the intersection, and a stale disclosure
	// version on the file side is kept.
	local := granted.In(time.FixedZone("example", 3600))
	recs = map[string]customerConsent{"own": {HiveID: "own", GrantedAt: local, WithdrawnAt: &delivered,
		TextVersion: "0", Fields: customerConsentFields{RealName: true}}}
	if err := st.SetConsent(crmPersonID(users[0]), store.Consent{Scope: "own",
		Fields:      []string{store.FieldRealName, store.FieldOrganization, store.FieldCountry},
		TextVersion: customerConsentTextVersion, GrantedAt: granted}); err != nil {
		t.Fatal(err)
	}
	crmOverlayConsents(users[0], recs)
	if g := recs["own"]; g.WithdrawnAt == nil || !g.WithdrawnAt.Equal(delivered) || g.TextVersion != "0" ||
		g.Fields != (customerConsentFields{RealName: true}) || g.GrantedAt.Location() != local.Location() {
		t.Fatalf("colony widened the file: %+v", g)
	}

	// A person colony does not hold keeps the file.
	ghost := &SaaSUser{GitHubUsername: "ghost-example"}
	recs = fileRecord()
	crmOverlayConsents(ghost, recs)
	if recs["own"].Fields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatal("a person colony does not hold was overlaid")
	}
}
