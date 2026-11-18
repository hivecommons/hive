package customers

import (
	"strings"
	"testing"
)

func TestTidyCompany(t *testing.T) {
	for in, want := range map[string]string{
		"acme": "Acme", "@acme": "Acme", "ibm": "IBM", "IBM": "IBM", "IBM Corp.": "IBM", "Acme, Inc.": "Acme",
		"Red Hat": "Red Hat", "Globex GmbH": "Globex", "Inc": "Inc", "": "",
	} {
		if got := TidyCompany(in); got != want {
			t.Errorf("TidyCompany(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrgFromDomain(t *testing.T) {
	for in, want := range map[string]string{
		"ibm.com": "IBM", "research.ibm.com": "IBM", "redhat.com": "Redhat", "example.co.uk": "Example",
		"gmail.com": "", "users.noreply.github.com": "", "localhost": "", "": "",
	} {
		if got := orgFromDomain(in); got != want {
			t.Errorf("orgFromDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func affPerson(org ProfileField, ids ...Identity) Person {
	return Person{Profile: Profile{Organization: org}, Identities: ids}
}

func TestSuggestAffiliationRules(t *testing.T) {
	gh := ProfileField{Value: "acme", Source: SourceAuto, Rule: ProfileSourceGitHub}
	hand := Identity{Kind: KindEmail, Value: "ada@ibm.com", Manual: true}

	s := SuggestAffiliation(affPerson(gh))
	if s == nil || s.Value != "Acme" || s.Rule != AffiliationFromCompany || !strings.Contains(s.Evidence, "GitHub company field") {
		t.Fatalf("company rule = %+v", s)
	}
	if s := SuggestAffiliation(affPerson(ProfileField{Value: "IBM", Source: SourceAuto, Rule: ProfileSourceGitHub})); s != nil {
		t.Fatalf("an already tidy company was suggested: %+v", s)
	}
	s = SuggestAffiliation(affPerson(ProfileField{}, hand))
	if s == nil || s.Value != "IBM" || s.Rule != AffiliationFromEmail || s.Evidence != "from email domain ibm.com" || strings.Contains(s.Evidence, "ada") {
		t.Fatalf("email rule = %+v", s)
	}
	// Email never overrides an organization that is already shown.
	if s := SuggestAffiliation(affPerson(gh, hand)); s == nil || s.Rule != AffiliationFromCompany {
		t.Fatalf("company must outrank the email domain: %+v", s)
	}
	for name, p := range map[string]Person{
		"by hand":         affPerson(ProfileField{Value: "Globex", Source: SourceManual}, hand),
		"cleared by hand": affPerson(ProfileField{Source: SourceManual}, hand),
		"claimed email":   affPerson(ProfileField{}, Identity{Kind: KindEmail, Value: "ada@ibm.com", Manual: true, Claimed: true}),
		"unconfirmed":     affPerson(ProfileField{}, Identity{Kind: KindEmail, Value: "ada@ibm.com"}),
		"git email":       affPerson(ProfileField{}, Identity{Kind: KindGitEmail, Value: "ada@ibm.com", Manual: true}),
		"free mail":       affPerson(ProfileField{}, Identity{Kind: KindEmail, Value: "ada@gmail.com", Manual: true}),
		"nothing":         affPerson(ProfileField{}),
	} {
		if s := SuggestAffiliation(p); s != nil {
			t.Errorf("%s: suggested %+v", name, s)
		}
	}
}

func TestAffiliationDismissAcceptAndBounds(t *testing.T) {
	s := newService(t)
	p, err := s.Add(ownerV, "Ada", []Identity{{Kind: KindEmail, Value: "ada@ibm.com"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ownerV, p.ID)
	if got.Affiliation == nil || got.Affiliation.Value != "IBM" {
		t.Fatalf("view suggestion = %+v", got.Affiliation)
	}
	// A list view carries no suggestion.
	if l, _ := s.List(ownerV); len(l.People) != 1 || l.People[0].Affiliation != nil {
		t.Fatalf("list view carried a suggestion: %+v", l.People)
	}
	// Dismiss remembers the refusal, case-insensitively and once.
	for _, v := range []string{"IBM", "ibm"} {
		if _, err := s.Edit(ownerV, p.ID, Edit{Op: OpAffiliationDismiss, Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.Get(ownerV, p.ID)
	if got.Affiliation != nil || len(got.Profile.DismissedAffiliations) != 1 {
		t.Fatalf("after dismiss: %+v %v", got.Affiliation, got.Profile.DismissedAffiliations)
	}
	if _, err := s.Edit(ownerV, p.ID, Edit{Op: OpAffiliationDismiss, Value: " "}); err == nil {
		t.Fatal("an empty dismissal was accepted")
	}
	for i := 0; i < MaxDismissedAffiliations+5; i++ {
		if _, err := s.Edit(ownerV, p.ID, Edit{Op: OpAffiliationDismiss, Value: "org" + strings.Repeat("x", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.Get(ownerV, p.ID)
	if n := len(got.Profile.DismissedAffiliations); n != MaxDismissedAffiliations {
		t.Fatalf("dismissals are not bounded: %d", n)
	}
	// Accept is an ordinary by-hand organization edit.
	got, err = s.Edit(ownerV, p.ID, Edit{Op: OpProfile, Field: FieldOrganization, Value: "IBM"})
	if err != nil || got.Profile.Organization.Source != SourceManual || got.Affiliation != nil {
		t.Fatalf("accept = %+v, %v", got.Profile.Organization, err)
	}
	// A read-only user may dismiss on their own profile only.
	if _, err := s.Edit(readV, MeID, Edit{Op: OpAffiliationDismiss, Value: "x"}); err != nil {
		t.Fatalf("self dismiss: %v", err)
	}
	if _, err := s.Edit(readV, p.ID, Edit{Op: OpAffiliationDismiss, Value: "x"}); err == nil {
		t.Fatal("a read-only user dismissed on someone else")
	}
}

func TestNotesBoundsVisibilityExportAndForget(t *testing.T) {
	s := newService(t)
	p, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada-gh"}})
	if _, err := s.Edit(ownerV, p.ID, Edit{Op: OpNotes, Notes: strings.Repeat("n", MaxNotesLen+1)}); err == nil {
		t.Fatal("notes over the bound were accepted")
	}
	if got, err := s.Edit(rwV, p.ID, Edit{Op: OpNotes, Notes: strings.Repeat("n", MaxNotesLen)}); err != nil || len(got.Notes) != MaxNotesLen {
		t.Fatalf("read-write notes at the bound = %d, %v", len(got.Notes), err)
	}
	if _, err := s.Edit(ownerV, p.ID, Edit{Op: OpProfile, Field: FieldCountry, Value: "fr"}); err != nil {
		t.Fatal(err)
	}
	ex, err := s.Export(ownerV, p.ID)
	if err != nil || len(ex.Person.Notes) != MaxNotesLen || ex.Person.Profile.Country.Value != "FR" {
		t.Fatalf("export lacks notes or country: %v %+v", err, ex.Person.Profile.Country)
	}
	if _, err := s.Forget(ownerV, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ownerV, p.ID); err == nil {
		t.Fatal("a forgotten customer is still readable")
	}
	for _, q := range s.Store().People() {
		if q.ID == p.ID {
			t.Fatalf("forgotten person left behind: %+v", q)
		}
	}
}
