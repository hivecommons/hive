package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() *File {
	return &File{
		Active:   "alpha",
		Username: "octocat",
		Profiles: []Profile{
			{Name: "alpha", Hub: "wss://alpha.example.dev/contribute", ContributorID: "c-1", RegistrationToken: "tok-1", Backend: "claude"},
			{Name: "beta", Hub: "wss://beta.example.dev/contribute", ContributorID: "c-2", RegistrationToken: "tok-2", Session: "goose"},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sample()
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("profiles.yml mode = %o, want 600 — it holds registration tokens", perm)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Active != "alpha" || got.Username != "octocat" || len(got.Profiles) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Profiles[1].RegistrationToken != "tok-2" || got.Profiles[1].Session != "goose" {
		t.Errorf("profile fields lost: %+v", got.Profiles[1])
	}
}

func TestLoadMissingFileIsNotExist(t *testing.T) {
	if _, err := Load(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("Load on empty dir = %v, want os.ErrNotExist", err)
	}
}

func TestLoadRejectsMalformedAndInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(":\tnot yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load accepted malformed yaml")
	}
	if err := os.WriteFile(Path(dir), []byte("profiles:\n  - name: \"bad name\"\n    hub: wss://x/contribute\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load accepted an invalid profile name")
	}
}

func TestSaveRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []*File{
		{Profiles: []Profile{{Name: "a,b", Hub: "wss://x"}}},
		{Profiles: []Profile{{Name: "a", Hub: ""}}},
		{Profiles: []Profile{{Name: "a", Hub: "wss://x"}, {Name: "a", Hub: "wss://y"}}},
		{Active: "ghost", Profiles: []Profile{{Name: "a", Hub: "wss://x"}}},
	} {
		if err := Save(dir, f); err == nil {
			t.Errorf("Save accepted invalid file %+v", f)
		}
	}
}

func TestActiveProfileResolution(t *testing.T) {
	f := sample()
	if got := f.ActiveProfile(); got.Name != "alpha" {
		t.Errorf("ActiveProfile = %q, want alpha", got.Name)
	}
	f.Active = ""
	if got := f.ActiveProfile(); got.Name != "alpha" {
		t.Errorf("ActiveProfile with empty Active = %q, want first", got.Name)
	}
	if (&File{}).ActiveProfile() != nil {
		t.Error("ActiveProfile on empty file should be nil")
	}
}

func TestUseAddRemoveRename(t *testing.T) {
	f := sample()
	if err := f.Use("beta"); err != nil || f.Active != "beta" {
		t.Fatalf("Use(beta) = %v, active %q", err, f.Active)
	}
	if err := f.Use("ghost"); err == nil {
		t.Error("Use(ghost) succeeded")
	}
	if err := f.Add(Profile{Name: "alpha", Hub: "wss://dup"}); err == nil {
		t.Error("Add duplicate name succeeded")
	}
	if err := f.Add(Profile{Name: "bad,name", Hub: "wss://x"}); err == nil {
		t.Error("Add invalid name succeeded")
	}
	if err := f.Add(Profile{Name: "gamma", Hub: "wss://gamma.example.dev/contribute"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if f.Profiles[2].AddedAt == "" {
		t.Error("Add did not stamp AddedAt")
	}
	if err := f.Remove("beta"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if f.Active != "alpha" {
		t.Errorf("removing the active profile should fall back to the first; active = %q", f.Active)
	}
	if err := f.Remove("ghost"); err == nil {
		t.Error("Remove(ghost) succeeded")
	}
	if err := f.Rename("alpha", "gamma"); err == nil {
		t.Error("Rename onto an existing name succeeded")
	}
	if err := f.Rename("alpha", "bad name"); err == nil {
		t.Error("Rename to invalid name succeeded")
	}
	f.Active = "alpha"
	if err := f.Rename("alpha", "prod"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if f.Active != "prod" {
		t.Errorf("Rename should carry the active marker; active = %q", f.Active)
	}
	if err := f.Rename("ghost", "x"); err == nil {
		t.Error("Rename(ghost) succeeded")
	}
}

func TestRemoveLastProfileClearsActive(t *testing.T) {
	f := &File{Active: "only", Profiles: []Profile{{Name: "only", Hub: "wss://x/contribute"}}}
	if err := f.Remove("only"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if f.Active != "" || len(f.Profiles) != 0 {
		t.Errorf("after removing the last profile: active %q, %d profiles", f.Active, len(f.Profiles))
	}
}

func writeEnv(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, EnvFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateMultiHub(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, `HIVE_REGISTRATION_TOKEN=tok-1,tok-2
HIVE_HUB=wss://alpha.example.dev/contribute,wss://beta.example.dev/contribute
CONTRIBUTOR_ID=c-1,c-2
CONTRIBUTOR_USERNAME=octocat
AGENT_BACKEND=claude
`)
	f, err := Migrate(dir)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(f.Profiles) != 2 || f.Username != "octocat" {
		t.Fatalf("migrated file: %+v", f)
	}
	if f.Profiles[0].Name != "alpha.example.dev" || f.Profiles[1].Name != "beta.example.dev" {
		t.Errorf("names = %q, %q", f.Profiles[0].Name, f.Profiles[1].Name)
	}
	if f.Profiles[0].RegistrationToken != "tok-1" || f.Profiles[1].ContributorID != "c-2" {
		t.Errorf("pairing lost: %+v", f.Profiles)
	}
	if f.Active != "alpha.example.dev" {
		t.Errorf("active = %q, want the first hub", f.Active)
	}
	if f.Profiles[0].Backend != "claude" {
		t.Errorf("backend = %q", f.Profiles[0].Backend)
	}
	// The migration must have persisted the file.
	if _, err := Load(dir); err != nil {
		t.Errorf("Load after Migrate: %v", err)
	}
	// And contributor.env is untouched by migration itself.
	if _, err := os.Stat(EnvPath(dir)); err != nil {
		t.Errorf("contributor.env gone after migration: %v", err)
	}
}

func TestMigrateSingleSharedContributorID(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, `HIVE_REGISTRATION_TOKEN=tok-1,tok-2
HIVE_HUB=wss://a.dev/contribute,wss://b.dev/contribute
CONTRIBUTOR_ID=c-1
`)
	f, err := Migrate(dir)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if f.Profiles[0].ContributorID != "c-1" || f.Profiles[1].ContributorID != "c-1" {
		t.Errorf("single id not shared: %+v", f.Profiles)
	}
}

func TestMigrateRefusesMisalignedLists(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, `HIVE_REGISTRATION_TOKEN=tok-1
HIVE_HUB=wss://a.dev/contribute,wss://b.dev/contribute
CONTRIBUTOR_ID=c-1,c-2
`)
	if _, err := Migrate(dir); err == nil || !strings.Contains(err.Error(), "misaligned") {
		t.Fatalf("Migrate on misaligned tokens = %v, want misaligned error", err)
	}
	writeEnv(t, dir, `HIVE_REGISTRATION_TOKEN=tok-1,tok-2,tok-3
HIVE_HUB=wss://a.dev/contribute,wss://b.dev/contribute,wss://c.dev/contribute
CONTRIBUTOR_ID=c-1,c-2
`)
	if _, err := Migrate(dir); err == nil || !strings.Contains(err.Error(), "misaligned") {
		t.Fatalf("Migrate on misaligned ids = %v, want misaligned error", err)
	}
	writeEnv(t, dir, "CONTRIBUTOR_USERNAME=octocat\n")
	if _, err := Migrate(dir); err == nil {
		t.Fatal("Migrate with no HIVE_HUB succeeded")
	}
}

func TestMigrateDeduplicatesDerivedNames(t *testing.T) {
	dir := t.TempDir()
	// Same host twice (two sessions against one hive written positionally).
	writeEnv(t, dir, `HIVE_REGISTRATION_TOKEN=tok-1,tok-2
HIVE_HUB=wss://a.dev/contribute,wss://a.dev/contribute
CONTRIBUTOR_ID=c-1,c-2
`)
	f, err := Migrate(dir)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if f.Profiles[0].Name == f.Profiles[1].Name {
		t.Errorf("duplicate names after migration: %q", f.Profiles[0].Name)
	}
}

func TestLoadOrMigrate(t *testing.T) {
	// Existing profiles.yml wins.
	dir := t.TempDir()
	if err := Save(dir, sample()); err != nil {
		t.Fatal(err)
	}
	f, err := LoadOrMigrate(dir)
	if err != nil || len(f.Profiles) != 2 {
		t.Fatalf("LoadOrMigrate with profiles.yml: %v, %+v", err, f)
	}
	// Nothing at all: empty file, no error.
	f, err = LoadOrMigrate(t.TempDir())
	if err != nil || len(f.Profiles) != 0 {
		t.Fatalf("LoadOrMigrate on empty dir: %v, %+v", err, f)
	}
	// Only contributor.env: migrates.
	dir = t.TempDir()
	writeEnv(t, dir, "HIVE_REGISTRATION_TOKEN=t\nHIVE_HUB=wss://a.dev/contribute\nCONTRIBUTOR_ID=c\n")
	f, err = LoadOrMigrate(dir)
	if err != nil || len(f.Profiles) != 1 {
		t.Fatalf("LoadOrMigrate migration: %v, %+v", err, f)
	}
	// Malformed profiles.yml is an error, not a silent re-migration.
	dir = t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(":\tnope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrMigrate(dir); err == nil {
		t.Fatal("LoadOrMigrate accepted a malformed profiles.yml")
	}
}

func TestNameForHubSanitizesAndFallsBack(t *testing.T) {
	taken := map[string]bool{}
	if got := nameForHub("wss://Hive.Example.dev:8443/contribute", taken); got != "Hive.Example.dev" {
		t.Errorf("nameForHub = %q", got)
	}
	if got := nameForHub("!!!", taken); got != "hive" {
		t.Errorf("nameForHub on garbage = %q, want hive", got)
	}
	if got := nameForHub("!!!", taken); got != "hive-2" {
		t.Errorf("nameForHub second garbage = %q, want hive-2", got)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"prod": true, "a.b-c_9": true, "9lives": true,
		"": false, "has space": false, "a,b": false, "-lead": false, "a/b": false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home")
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if !strings.HasSuffix(dir, "/.config/hive") {
		t.Errorf("DefaultDir = %q", dir)
	}
}

func TestSaveAndProjectFailWhenDirIsAFile(t *testing.T) {
	parent := t.TempDir()
	blocked := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := sample()
	if err := Save(blocked, f); err == nil {
		t.Error("Save into a file path succeeded")
	}
	if err := Project(blocked, f); err == nil {
		t.Error("Project into a file path succeeded")
	}
}

func TestMigrateWithNoContributorIDs(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "HIVE_REGISTRATION_TOKEN=tok-1\nHIVE_HUB=wss://a.dev/contribute\n")
	f, err := Migrate(dir)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if f.Profiles[0].ContributorID != "" {
		t.Errorf("ContributorID = %q, want empty", f.Profiles[0].ContributorID)
	}
}

func TestParseEnvFileSkipsCommentsAndKeepsFirstDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "# comment\n\nnot-a-pair\nKEY=first\nKEY=second\n")
	ev, err := parseEnvFile(EnvPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if ev.values["KEY"] != "second" || len(ev.order) != 1 {
		t.Errorf("parseEnvFile = %+v", ev)
	}
	if _, err := parseEnvFile(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing file err = %v", err)
	}
}
