package customers

import (
	"errors"
	"testing"
)

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
