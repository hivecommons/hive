package hub

// Tests for the colony-mode consent reads (#19857). Every person here is
// synthetic.

import (
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
	for _, e := range m {
		if e.RealName != "" || e.Country != "" || e.Organization != "" {
			t.Fatalf("withdrawn in colony still sent values: %+v", m)
		}
	}
}

// The overlay takes the grant from colony and keeps the hub-only delivery
// bookkeeping from the consent file; it changes nothing outside colony mode,
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
	if got.Fields != (customerConsentFields{Organization: true}) || got.TextVersion != "text-example" ||
		!got.GrantedAt.Equal(colonyGrant.GrantedAt) || got.WithdrawnAt == nil || !got.WithdrawnAt.Equal(colonyGrant.WithdrawnAt) {
		t.Fatalf("grant not read from colony: %+v", got)
	}
	if got.DeliveredRev != "rev-example" || got.LastDeliveredAt == nil || !got.LastDeliveredAt.Equal(delivered) ||
		got.DeliveredFields == nil || *got.DeliveredFields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatalf("delivery bookkeeping lost: %+v", got)
	}
	if o := recs["other"]; o.Fields != (customerConsentFields{Country: true}) {
		t.Fatalf("a hive colony does not hold changed: %+v", o)
	}

	// A re-grant in colony clears the withdrawal, and an equal time keeps the
	// file's value.
	local := granted.In(time.FixedZone("example", 3600))
	recs = map[string]customerConsent{"own": {HiveID: "own", GrantedAt: local, WithdrawnAt: &delivered}}
	if err := st.SetConsent(crmPersonID(users[0]), store.Consent{Scope: "own", Fields: []string{store.FieldRealName},
		TextVersion: customerConsentTextVersion, GrantedAt: granted}); err != nil {
		t.Fatal(err)
	}
	crmOverlayConsents(users[0], recs)
	if g := recs["own"]; g.WithdrawnAt != nil || g.GrantedAt.Location() != local.Location() || !g.Fields.RealName {
		t.Fatalf("re-grant overlay = %+v", g)
	}

	// A person colony does not hold keeps the file.
	ghost := &SaaSUser{GitHubUsername: "ghost-example"}
	recs = fileRecord()
	crmOverlayConsents(ghost, recs)
	if recs["own"].Fields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatal("a person colony does not hold was overlaid")
	}
}
