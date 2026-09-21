package hivectl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared mutations (#8128). They were extracted from the cobra command
// bodies so `hivectl hives` and the TUI's Hives pane cannot drift; these pin
// the rules that were easy to get subtly wrong in a copy — re-electing an
// active profile on removal, carrying the active marker through a rename, and
// writing profiles.yml before its projection.

func opsSet() *ProfileSet {
	return &ProfileSet{
		Version: ProfilesVersion,
		Active:  "acme",
		Profiles: []Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "c1", RegistrationToken: "t1"},
			{Name: "other", Hub: "wss://other.example/contribute", ContributorID: "c2", RegistrationToken: "t2"},
		},
	}
}

func TestUseMarksActiveAndReportsANoOp(t *testing.T) {
	set := opsSet()

	profile, already, err := set.Use("other")
	if err != nil || already || profile.Name != "other" {
		t.Fatalf("Use(other) = %+v, %v, %v", profile, already, err)
	}
	if set.Active != "other" {
		t.Errorf("active = %q, want other", set.Active)
	}

	// Case-insensitive, matching the uniqueness rule, and the RESOLVED name is
	// what is recorded — not the spelling that was typed.
	profile, already, err = set.Use("OTHER")
	if err != nil || !already {
		t.Fatalf("re-using the active hive: %+v, already=%v, err=%v", profile, already, err)
	}
	if set.Active != "other" {
		t.Errorf("active = %q, want the profile's own spelling", set.Active)
	}

	if _, _, err := set.Use("nope"); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("Use of an unknown hive = %v, want ErrProfileNotFound", err)
	}
}

func TestAddAppendsAndActivatesTheFirst(t *testing.T) {
	set := &ProfileSet{}
	first := Profile{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "t1"}
	if err := set.Add(first, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The first hive is active whether or not it was asked for: a set with
	// profiles and no active one resolves to the first anyway, so recording it
	// is the difference between a stated fact and an inferred one.
	if set.Active != "acme" || set.Version != ProfilesVersion {
		t.Errorf("set after the first add = %+v", set)
	}

	second := Profile{Name: "other", Hub: "wss://other.example/contribute", RegistrationToken: "t2"}
	if err := set.Add(second, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if set.Active != "acme" {
		t.Errorf("active = %q, want the add to leave it alone", set.Active)
	}

	if err := set.Add(Profile{Name: "ACME", Hub: "wss://x.example/contribute"}, false); !errors.Is(err, ErrProfileExists) {
		t.Errorf("a case-insensitive duplicate = %v, want ErrProfileExists", err)
	}
	if err := set.Add(Profile{Name: "bad name", Hub: "wss://x.example/contribute"}, false); err == nil {
		t.Error("an invalid profile name was accepted")
	}
	if len(set.Profiles) != 2 {
		t.Errorf("a refused add still appended: %+v", set.Profiles)
	}
}

func TestRenameCarriesTheActiveMarker(t *testing.T) {
	set := opsSet()
	if _, err := set.Rename("acme", "acme-prod"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if set.Active != "acme-prod" {
		t.Errorf("active = %q, want the rename carried through", set.Active)
	}

	// A pure case change collides with itself under the case-insensitive
	// uniqueness rule, so it must be allowed.
	if _, err := set.Rename("acme-prod", "Acme-Prod"); err != nil {
		t.Errorf("a case-only rename was refused: %v", err)
	}
	if _, err := set.Rename("other", "Acme-Prod"); !errors.Is(err, ErrProfileExists) {
		t.Errorf("renaming onto another profile = %v, want ErrProfileExists", err)
	}
	if _, err := set.Rename("nope", "x"); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("renaming an unknown hive = %v, want ErrProfileNotFound", err)
	}
}

func TestRemoveReElectsTheActiveProfile(t *testing.T) {
	set := opsSet()
	removed, wasActive, err := set.Remove("acme")
	if err != nil || !wasActive {
		t.Fatalf("Remove(acme) = %+v, %v, %v", removed, wasActive, err)
	}
	// Returned by value, before the splice, so a caller can still name the hive
	// whose credential no longer exists anywhere.
	if removed.Hub != "wss://acme.example/contribute" || removed.RegistrationToken != "t1" {
		t.Errorf("removed = %+v, want the profile as it was", removed)
	}
	if set.Active != "other" {
		t.Errorf("active = %q, want the survivor", set.Active)
	}

	// Removing the last one leaves Active EMPTY rather than dangling: Validate
	// refuses an active that names no profile, so a dangling name would make
	// the very next Save fail.
	if _, _, err := set.Remove("other"); err != nil {
		t.Fatalf("Remove(other): %v", err)
	}
	if set.Active != "" || len(set.Profiles) != 0 {
		t.Errorf("empty set = %+v", set)
	}
	if err := set.Validate(); err != nil {
		t.Errorf("the emptied set does not validate: %v", err)
	}
	if _, _, err := set.Remove("acme"); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("removing from an empty set = %v, want ErrProfileNotFound", err)
	}
}

// Removing a NON-active profile must not disturb the active marker.
func TestRemoveLeavesAnUnrelatedActiveAlone(t *testing.T) {
	set := opsSet()
	if _, wasActive, err := set.Remove("other"); err != nil || wasActive {
		t.Fatalf("Remove(other) = wasActive %v, err %v", wasActive, err)
	}
	if set.Active != "acme" {
		t.Errorf("active = %q, want acme untouched", set.Active)
	}
}

func TestCommitWritesProfilesThenTheProjection(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	set := opsSet()
	if _, _, err := set.Use("other"); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if err := store.Commit(set); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	reloaded, err := store.Load()
	if err != nil || reloaded == nil || reloaded.Active != "other" {
		t.Fatalf("profiles.yml after Commit = %+v, %v", reloaded, err)
	}
	env, err := os.ReadFile(filepath.Join(dir, "contributor.env"))
	if err != nil {
		t.Fatalf("read contributor.env: %v", err)
	}
	for _, want := range []string{
		"HIVE_HUB=wss://other.example/contribute,wss://acme.example/contribute",
		"HIVE_REGISTRATION_TOKEN=t2,t1",
		"CONTRIBUTOR_ID=c2,c1",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("projection is missing %q:\n%s", want, env)
		}
	}
	// Both files are credential files and must be owner-only from their first
	// byte, whichever path wrote them.
	for _, name := range []string{"profiles.yml", "contributor.env"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, got)
		}
	}
}

// An invalid set is refused BEFORE anything is written, so a bad mutation
// cannot leave a half-applied change on disk.
func TestCommitRefusesAnInvalidSetWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	bad := &ProfileSet{
		Version:  ProfilesVersion,
		Active:   "ghost",
		Profiles: []Profile{{Name: "acme", Hub: "wss://acme.example/contribute"}},
	}
	if err := store.Commit(bad); err == nil {
		t.Fatal("Commit accepted an active profile that does not exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Commit still wrote profiles.yml (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "contributor.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Commit still wrote contributor.env (%v)", err)
	}
}
