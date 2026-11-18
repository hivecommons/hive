package customers

import (
	"context"
	"errors"
	"testing"
)

type fakeProfiles struct {
	calls int
	prof  PublicProfile
	err   error
}

func (f *fakeProfiles) PublicProfile(_ context.Context, _ string) (PublicProfile, error) {
	f.calls++
	return f.prof, f.err
}

func profileFixture(t *testing.T) (*mentionFixture, *fakeProfiles) {
	t.Helper()
	x := newMentionFixture(t)
	// The fixture's index-only store is reused through a second handle.
	fp := &fakeProfiles{prof: PublicProfile{Name: "Ada Lovelace", Company: "@acme", Location: "London, UK"}}
	x.m.Profiles = fp
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	x.m.Store = st
	x.person = mustAdd(t, st, "ada", adaLogin)
	return x, fp
}

func TestFetchProfileFillsAutoFieldsWithProvenance(t *testing.T) {
	x, fp := profileFixture(t)
	res := x.m.FetchProfile(context.Background(), x.person.ID, adaLogin)
	if res.Skipped != "" || res.Filled != 1 || fp.calls != 1 {
		t.Fatalf("result = %+v calls=%d", res, fp.calls)
	}
	p, _ := x.m.Store.Get(x.person.ID)
	if p.Profile.RealName.Value != "Ada Lovelace" || p.Profile.Organization.Value != "acme" || p.Profile.Location.Value != "London, UK" {
		t.Fatalf("profile = %+v", p.Profile)
	}
	for _, name := range ProfileFields {
		f := p.Profile.Field(name)
		want := ProfileSourceGitHub
		if name == FieldCountry {
			want = CountryRuleLocation
		}
		if f.Source != SourceAuto || f.Rule != want {
			t.Fatalf("%s provenance = %+v", name, f)
		}
	}
}

func TestFetchProfileNeverOverwritesManualOrClearedFields(t *testing.T) {
	x, _ := profileFixture(t)
	if _, err := x.m.Store.SetProfileField(x.person.ID, FieldOrganization, "Globex", "olivia"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.m.Store.SetProfileField(x.person.ID, FieldLocation, "", "olivia"); err != nil {
		t.Fatal(err)
	}
	x.m.FetchProfile(context.Background(), x.person.ID, adaLogin)
	p, _ := x.m.Store.Get(x.person.ID)
	if p.Profile.Organization.Value != "Globex" || p.Profile.Organization.Source != SourceManual {
		t.Fatalf("manual organization overwritten: %+v", p.Profile.Organization)
	}
	if p.Profile.Location.Value != "" {
		t.Fatalf("a field a person cleared was refilled: %+v", p.Profile.Location)
	}
	if p.Profile.RealName.Value != "Ada Lovelace" {
		t.Fatalf("untouched field not filled: %+v", p.Profile.RealName)
	}
	// Auto hands it back, and the next fetch may fill it.
	if _, err := x.m.Store.ResetProfileField(x.person.ID, FieldLocation); err != nil {
		t.Fatal(err)
	}
	x.m.FetchProfile(context.Background(), x.person.ID, adaLogin)
	p, _ = x.m.Store.Get(x.person.ID)
	if p.Profile.Location.Value != "London, UK" {
		t.Fatalf("reset did not hand the field back: %+v", p.Profile.Location)
	}
}

func TestFetchProfileGatesAndBudget(t *testing.T) {
	x, fp := profileFixture(t)
	x.m.Enabled = func() bool { return false }
	if r := x.m.FetchProfile(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipDisabled || fp.calls != 0 {
		t.Fatalf("disabled: %+v calls=%d", r, fp.calls)
	}
	x.m.Enabled = func() bool { return true }
	if r := x.m.FetchProfile(context.Background(), x.person.ID, Identity{Kind: KindEmail, Value: "ada@example.com"}); r.Skipped != SkipNotLogin || fp.calls != 0 {
		t.Fatalf("an email must never be sent to GitHub: %+v calls=%d", r, fp.calls)
	}
	if r := x.m.FetchProfile(context.Background(), x.person.ID, Identity{Kind: KindGitHub, Value: "dependabot[bot]"}); r.Skipped != SkipNotLogin || fp.calls != 0 {
		t.Fatalf("bot: %+v", r)
	}
	*x.quota = "core_low"
	if r := x.m.FetchProfile(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipQuotaLow || fp.calls != 0 {
		t.Fatalf("quota low: %+v", r)
	}
	*x.quota = ""
	x.m.HourlyLimit = func() int { return 0 }
	if r := x.m.FetchProfile(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipHourlyBudget || fp.calls != 0 {
		t.Fatalf("hourly budget: %+v", r)
	}
	x.m.HourlyLimit = func() int { return 10 }
	fp.err = errors.New("boom")
	if r := x.m.FetchProfile(context.Background(), x.person.ID, adaLogin); r.Skipped != "fetch_failed" {
		t.Fatalf("failure: %+v", r)
	}
	p, _ := x.m.Store.Get(x.person.ID)
	if p.Profile.RealName.Value != "" {
		t.Fatalf("a failed fetch wrote %+v", p.Profile)
	}
}

func TestProfileEditsRolesExportAndForget(t *testing.T) {
	s := newService(t)
	ada, err := s.Add(rwV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	// Read-write edits anyone; the value is stored as manual.
	got, err := s.Edit(rwV, ada.ID, Edit{Op: OpProfile, Field: FieldOrganization, Value: "Acme"})
	if err != nil || got.Profile.Organization.Value != "Acme" || got.Profile.Organization.Source != SourceManual || got.Profile.Organization.By != "rita" {
		t.Fatalf("rw edit = %+v, %v", got.Profile, err)
	}
	if _, err := s.Edit(rwV, ada.ID, Edit{Op: OpProfile, Field: "bio", Value: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown field err = %v", err)
	}
	// A read-only user edits their own "me" and cannot reach anyone else.
	me, err := s.Edit(readV, MeID, Edit{Op: OpProfile, Field: FieldLocation, Value: "Berlin"})
	if err != nil || me.Profile.Location.Value != "Berlin" {
		t.Fatalf("read self edit = %+v, %v", me.Profile, err)
	}
	if _, err := s.Edit(readV, ada.ID, Edit{Op: OpProfile, Field: FieldLocation, Value: "x"}); err == nil {
		t.Fatal("read-only user edited another person's profile")
	}
	// Auto hands the field back.
	got, err = s.Edit(rwV, ada.ID, Edit{Op: OpProfile, Field: FieldOrganization, State: TagAuto})
	if err != nil || got.Profile.Organization.Value != "" || got.Profile.Organization.Source != "" {
		t.Fatalf("reset = %+v, %v", got.Profile, err)
	}
	// Export carries the fields; forget removes them.
	if _, err := s.Edit(rwV, ada.ID, Edit{Op: OpProfile, Field: FieldRealName, Value: "Ada Lovelace"}); err != nil {
		t.Fatal(err)
	}
	ex, err := s.Export(ownerV, ada.ID)
	if err != nil || ex.Person.Profile.RealName.Value != "Ada Lovelace" {
		t.Fatalf("export = %+v, %v", ex.Person.Profile, err)
	}
	if _, err := s.Forget(ownerV, ada.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ownerV, ada.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forgotten person still readable: %v", err)
	}
}

func TestNormalizeCompany(t *testing.T) {
	for in, want := range map[string]string{"@ibm": "ibm", "  Red   Hat ": "Red Hat", "": ""} {
		if got := NormalizeCompany(in); got != want {
			t.Errorf("NormalizeCompany(%q) = %q, want %q", in, got, want)
		}
	}
}
