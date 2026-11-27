package hub

// syncCRMConsents replaces a person's consents in one colony transaction
// (#20113). Every person here is synthetic.

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/colony/store"
)

// A replace that colony refuses leaves the stored consent as it was: a
// delete followed by a failed write would leave no consent for the scope,
// which reads as a withdrawal.
func TestSyncCRMConsentsFailedReplaceKeepsConsent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	id := crmPersonID(users[0])
	before, ok := colonyPerson(t, st, users[0]).Consent("own")
	if !ok {
		t.Fatal("no colony consent for the fixture scope")
	}

	c := readMatchConsent(t, users[0], "own")
	c.Fields.Organization = true
	// Longer than colony allows, so the write of the changed consent fails.
	c.DeliveredRev = strings.Repeat("r", 512)
	hubCRM.mu.Lock()
	err := syncCRMConsents(st, id, map[string]customerConsent{"own": c})
	hubCRM.mu.Unlock()
	if err == nil {
		t.Fatal("an invalid consent was accepted")
	}

	after, ok := colonyPerson(t, st, users[0]).Consent("own")
	if !ok {
		t.Fatal("a failed consent replace left no consent for the scope")
	}
	if !crmConsentEqual(after, before) {
		t.Fatalf("consent after a failed replace = %+v, want %+v", after, before)
	}
}

// A replace overwrites delivery bookkeeping the hub's record does not name,
// drops a scope the hub pruned, and keeps the rest of the person.
func TestSyncCRMConsentsReplacesInOneWrite(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	st := useHubCustomerStore(t, hubCustomerStoreColony)
	id := crmPersonID(users[0])
	if err := st.RecordDelivery(id, "own", store.Delivery{Fields: []string{"real_name"}, Rev: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SetConsent(id, store.Consent{Scope: "pruned", Fields: []string{}, GrantedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	beforeP := colonyPerson(t, st, users[0])

	c := readMatchConsent(t, users[0], "own")
	c.Fields.Organization = true
	hubCRM.mu.Lock()
	err := syncCRMConsents(st, id, map[string]customerConsent{"own": c})
	hubCRM.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	p := colonyPerson(t, st, users[0])
	got, ok := p.Consent("own")
	if !ok {
		t.Fatal("consent missing after the replace")
	}
	want := crmConsentsFromRecords(map[string]customerConsent{"own": c})[0]
	if !crmConsentEqual(got, want) {
		t.Fatalf("consent after the replace = %+v, want %+v", got, want)
	}
	if got.DeliveredRev != "" || len(got.DeliveredFields) != 0 || got.Delivered() {
		t.Fatalf("stale delivery bookkeeping kept: %+v", got)
	}
	if _, ok := p.Consent("pruned"); ok {
		t.Fatal("a consent the hub pruned was kept")
	}
	if len(p.Identities) != len(beforeP.Identities) || len(p.Facts) != len(beforeP.Facts) {
		t.Fatalf("replace changed the person: %d identities, %d facts; want %d, %d",
			len(p.Identities), len(p.Facts), len(beforeP.Identities), len(beforeP.Facts))
	}
	if f := effectiveFact(t, p, store.FieldCountry); f.Value != "GB" || f.Rank != store.RankUser {
		t.Fatalf("country after the replace = %q at %q", f.Value, f.Rank)
	}
}
