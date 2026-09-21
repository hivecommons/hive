package hivectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeEnv(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "contributor.env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write contributor.env: %v", err)
	}
	return path
}

func TestProfileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)

	set := &ProfileSet{
		Active: "acme",
		Profiles: []Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "c1", RegistrationToken: "tok1", AddedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)},
			{Name: "local", Hub: "ws://localhost:3001/contribute", ContributorID: "c2", RegistrationToken: "tok2", Session: "review", Backend: "claude"},
		},
	}
	if err := store.Save(set); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if set.Version != ProfilesVersion {
		t.Fatalf("Save did not stamp the schema version, got %d", set.Version)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil || len(loaded.Profiles) != 2 {
		t.Fatalf("Load returned %+v, want 2 profiles", loaded)
	}
	if loaded.Active != "acme" {
		t.Errorf("active = %q, want acme", loaded.Active)
	}
	if got := loaded.Profiles[1]; got.Session != "review" || got.Backend != "claude" || got.RegistrationToken != "tok2" {
		t.Errorf("second profile round-tripped as %+v", got)
	}
	if !loaded.Profiles[0].AddedAt.Equal(set.Profiles[0].AddedAt) {
		t.Errorf("added_at = %v, want %v", loaded.Profiles[0].AddedAt, set.Profiles[0].AddedAt)
	}
}

// The file holds every registration token the contributor owns. A world- or
// group-readable mode hands them to any other local account, so the mode is
// asserted rather than assumed.
func TestProfileStoreSaveIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	if err := store.Save(&ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "t"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("profiles.yml mode = %v, want 0600", got)
	}
}

func TestDefaultProfileStoreUsesHomeConfigHive(t *testing.T) {
	store, err := DefaultProfileStore()
	if err != nil {
		t.Fatalf("DefaultProfileStore: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if got, want := store.Path(), filepath.Join(home, ".config", "hive", "profiles.yml"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestLoadHubsSeen(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	if got, err := store.LoadHubsSeen(); err != nil || len(got) != 0 {
		t.Fatalf("missing hubs-seen = %v, %v; want empty map and nil error", got, err)
	}
	if got, want := store.HubsSeenPath(), filepath.Join(dir, "hubs-seen.json"); got != want {
		t.Fatalf("HubsSeenPath() = %q, want %q", got, want)
	}
	if err := os.WriteFile(store.HubsSeenPath(), []byte(`{"wss://a.example/contribute":"2026-09-21T15:04:05Z"}`), 0o600); err != nil {
		t.Fatalf("write hubs-seen: %v", err)
	}
	seen, err := store.LoadHubsSeen()
	if err != nil {
		t.Fatalf("LoadHubsSeen: %v", err)
	}
	if got := seen["wss://a.example/contribute"].UTC().Format(time.RFC3339); got != "2026-09-21T15:04:05Z" {
		t.Fatalf("last seen = %q", got)
	}
}

func TestLoadHubsSeenErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "bad json", body: "{", want: "parse hive last-seen file"},
		{name: "bad timestamp", body: `{"wss://a.example/contribute":"not-a-time"}`, want: "parse last-seen timestamp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewProfileStore(t.TempDir())
			if err := os.WriteFile(store.HubsSeenPath(), []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write hubs-seen: %v", err)
			}
			if _, err := store.LoadHubsSeen(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadHubsSeen error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateHubURLErrors(t *testing.T) {
	tests := []string{
		"",
		" wss://a.example/contribute",
		"ftp://a.example/contribute",
		"wss:///contribute",
		"wss://a.example/contribute,wss://b.example/contribute",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if err := ValidateHubURL(input); err == nil {
				t.Fatalf("ValidateHubURL(%q) succeeded, want error", input)
			}
		})
	}
}

func TestEnvFileUnsetAndRender(t *testing.T) {
	env := &envFile{lines: []string{"A=1", "HIVE_SESSION=review", "B=2", "HIVE_SESSION=stale"}}
	env.unset("HIVE_SESSION")
	if got := string(env.render()); got != "A=1\nB=2\n" {
		t.Fatalf("render after unset = %q", got)
	}
	if got := string((&envFile{}).render()); got != "" {
		t.Fatalf("empty render = %q, want empty string", got)
	}
}

func TestProfileStoreWriteFileReportsCreateDirError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	store := NewProfileStore(filepath.Join(blocker, "child"))
	if err := store.Save(&ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "t"}}}); err == nil ||
		!strings.Contains(err.Error(), "create hive config dir") {
		t.Fatalf("Save error = %v, want create dir error", err)
	}
}

func TestProfileStoreLoadMissingFileIsNotAnError(t *testing.T) {
	set, err := NewProfileStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load on a missing file: %v", err)
	}
	if set != nil {
		t.Errorf("Load on a missing file returned %+v, want nil", set)
	}
}

// A corrupt file must not read as "no hives": the next Save would then
// overwrite live credentials with an empty set.
func TestProfileStoreLoadRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	if err := os.WriteFile(store.Path(), []byte("profiles: [oops\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted invalid YAML")
	}
}

func TestProfileStoreLoadRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	if err := os.WriteFile(store.Path(), []byte("version: 99\nprofiles: []\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := store.Load()
	if err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("Load error = %v, want a refusal naming version 99", err)
	}
}

func TestMigrateFromPositionalEnv(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, strings.Join([]string{
		"HIVE_REGISTRATION_TOKEN=tok1,tok2",
		"HIVE_HUB=wss://acme.hive.example/contribute,wss://other.hive.example/contribute",
		"CONTRIBUTOR_ID=c1,c2",
		"CONTRIBUTOR_USERNAME=someone",
		"AGENT_BACKEND=claude",
		"",
	}, "\n"))

	set, migrated, err := store.LoadOrMigrate()
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	if !migrated {
		t.Fatal("LoadOrMigrate did not report a migration")
	}
	if len(set.Profiles) != 2 {
		t.Fatalf("migrated %d profiles, want 2", len(set.Profiles))
	}
	if set.Profiles[0].Name != "acme" || set.Profiles[1].Name != "other" {
		t.Errorf("names = %q/%q, want acme/other (derived from the hub host)", set.Profiles[0].Name, set.Profiles[1].Name)
	}
	if set.Profiles[0].RegistrationToken != "tok1" || set.Profiles[1].RegistrationToken != "tok2" {
		t.Errorf("tokens paired wrongly: %+v", set.Profiles)
	}
	if set.Profiles[0].ContributorID != "c1" || set.Profiles[1].ContributorID != "c2" {
		t.Errorf("ids paired wrongly: %+v", set.Profiles)
	}
	// The relay starts at activeHubIndex 0; migration must not change which
	// hub that is.
	if set.Active != "acme" {
		t.Errorf("active = %q, want the first hub in the legacy list", set.Active)
	}

	// Migration alone leaves the relay's configuration byte-identical.
	after, err := os.ReadFile(store.EnvPath())
	if err != nil {
		t.Fatalf("read contributor.env: %v", err)
	}
	if !strings.Contains(string(after), "HIVE_HUB=wss://acme.hive.example/contribute,wss://other.hive.example/contribute") {
		t.Errorf("migration rewrote contributor.env:\n%s", after)
	}

	// Second call reads the saved file rather than migrating again.
	_, migratedAgain, err := store.LoadOrMigrate()
	if err != nil {
		t.Fatalf("second LoadOrMigrate: %v", err)
	}
	if migratedAgain {
		t.Error("LoadOrMigrate migrated twice")
	}
}

// Two hives whose hosts share a first label must not both be named for it —
// duplicate names would make `hives use` ambiguous.
func TestMigrateDeduplicatesDerivedNames(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, "HIVE_HUB=wss://hive.a.example/contribute,wss://hive.b.example/contribute\nHIVE_REGISTRATION_TOKEN=t1,t2\n")

	set, _, err := store.LoadOrMigrate()
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	if set.Profiles[0].Name != "hive" || set.Profiles[1].Name != "hive-2" {
		t.Errorf("names = %q/%q, want hive/hive-2", set.Profiles[0].Name, set.Profiles[1].Name)
	}
}

// The misalignment the relay dies on ("FATAL: HIVE_HUB lists N hub(s) but ...")
// cannot be migrated without guessing which token belongs to which hub, and a
// wrong guess points a live credential at the wrong hive.
func TestMigrateRefusesMisalignedLists(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  string
		want string
	}{
		{
			name: "fewer tokens than hubs",
			env:  "HIVE_HUB=wss://a.example/contribute,wss://b.example/contribute\nHIVE_REGISTRATION_TOKEN=t1\n",
			want: "2 hub(s) but 1 registration token(s)",
		},
		{
			name: "fewer ids than hubs",
			env:  "HIVE_HUB=wss://a.example/contribute,wss://b.example/contribute\nHIVE_REGISTRATION_TOKEN=t1,t2\nCONTRIBUTOR_ID=c1\n",
			want: "2 hub(s) but 1 contributor id(s)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewProfileStore(dir)
			writeEnv(t, dir, tt.env)
			_, _, err := store.LoadOrMigrate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one naming %q", err, tt.want)
			}
			if _, statErr := os.Stat(store.Path()); !os.IsNotExist(statErr) {
				t.Error("a refused migration still wrote profiles.yml")
			}
		})
	}
}

func TestLoadOrMigrateWithNothingOnDisk(t *testing.T) {
	_, _, err := NewProfileStore(t.TempDir()).LoadOrMigrate()
	if err == nil || !strings.Contains(err.Error(), ErrNoProfiles.Error()) {
		t.Fatalf("error = %v, want ErrNoProfiles", err)
	}
}

// The projection is the whole point of the file: the relay keeps reading the
// same three variables, the active hive is first (activeHubIndex 0), and every
// other key in the file survives.
func TestWriteEnvProjection(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, strings.Join([]string{
		"HIVE_REGISTRATION_TOKEN=old",
		"HIVE_HUB=wss://acme.example/contribute",
		"CONTRIBUTOR_ID=c1",
		"CONTRIBUTOR_USERNAME=someone",
		"AGENT_BACKEND=claude",
		"HIVE_LITELLM_ENDPOINT=https://llm.example",
		"",
	}, "\n"))

	set := &ProfileSet{
		Active: "other",
		Profiles: []Profile{
			{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "c1", RegistrationToken: "tok1"},
			{Name: "other", Hub: "wss://other.example/contribute", ContributorID: "c2", RegistrationToken: "tok2"},
		},
	}
	if err := store.WriteEnvProjection(set); err != nil {
		t.Fatalf("WriteEnvProjection: %v", err)
	}
	got := readEnvMap(t, store.EnvPath())

	if got["HIVE_HUB"] != "wss://other.example/contribute,wss://acme.example/contribute" {
		t.Errorf("HIVE_HUB = %q; active hive must be projected first", got["HIVE_HUB"])
	}
	if got["HIVE_REGISTRATION_TOKEN"] != "tok2,tok1" {
		t.Errorf("HIVE_REGISTRATION_TOKEN = %q, want tok2,tok1", got["HIVE_REGISTRATION_TOKEN"])
	}
	if got["CONTRIBUTOR_ID"] != "c2,c1" {
		t.Errorf("CONTRIBUTOR_ID = %q, want c2,c1", got["CONTRIBUTOR_ID"])
	}
	// Keys the projection does not own are carried across, not dropped.
	if got["HIVE_LITELLM_ENDPOINT"] != "https://llm.example" || got["CONTRIBUTOR_USERNAME"] != "someone" || got["AGENT_BACKEND"] != "claude" {
		t.Errorf("projection dropped unmanaged keys: %+v", got)
	}

	// The three lists a relay pairs by index must always be the same length —
	// the invariant that makes the relay's FATAL path unreachable from a file
	// this code wrote.
	assertAligned(t, got)

	// The previous credentials survive, because the hub cannot reprint them.
	bak, err := os.ReadFile(store.EnvPath() + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(bak), "HIVE_REGISTRATION_TOKEN=old") {
		t.Errorf("backup did not keep the previous file:\n%s", bak)
	}

	info, err := os.Stat(store.EnvPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("contributor.env mode = %v, want 0600", perm)
	}
}

func TestWriteEnvProjectionCreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", ContributorID: "c", RegistrationToken: "t", Session: "review"}}}
	if err := store.WriteEnvProjection(set); err != nil {
		t.Fatalf("WriteEnvProjection: %v", err)
	}
	got := readEnvMap(t, store.EnvPath())
	if got["HIVE_HUB"] != "wss://a.example/contribute" || got["HIVE_REGISTRATION_TOKEN"] != "t" {
		t.Errorf("projection = %+v", got)
	}
	if got["HIVE_SESSION"] != "review" {
		t.Errorf("HIVE_SESSION = %q, want the active profile's label", got["HIVE_SESSION"])
	}
	if _, err := os.Stat(store.EnvPath() + ".bak"); !os.IsNotExist(err) {
		t.Error("a first projection wrote a backup of a file that did not exist")
	}
}

// An empty set truncates the lists rather than removing the file: a relay
// started against an empty HIVE_HUB fails loudly, where a missing file would
// silently fall back to the public hub default.
func TestWriteEnvProjectionWithNoProfilesTruncates(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, "HIVE_HUB=wss://a.example/contribute\nHIVE_REGISTRATION_TOKEN=t\nCONTRIBUTOR_ID=c\n")
	if err := store.WriteEnvProjection(&ProfileSet{}); err != nil {
		t.Fatalf("WriteEnvProjection: %v", err)
	}
	got := readEnvMap(t, store.EnvPath())
	for _, key := range []string{"HIVE_HUB", "HIVE_REGISTRATION_TOKEN", "CONTRIBUTOR_ID"} {
		if got[key] != "" {
			t.Errorf("%s = %q, want empty", key, got[key])
		}
	}
}

// A shell sourcing contributor.env lets the LAST assignment win, so a stale
// duplicate left behind would make the projection a no-op.
func TestWriteEnvProjectionCollapsesDuplicateAssignments(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, "HIVE_HUB=wss://stale.example/contribute\nAGENT_BACKEND=claude\nHIVE_HUB=wss://staler.example/contribute\n")
	set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "t"}}}
	if err := store.WriteEnvProjection(set); err != nil {
		t.Fatalf("WriteEnvProjection: %v", err)
	}
	data, err := os.ReadFile(store.EnvPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(data), "HIVE_HUB="); n != 1 {
		t.Fatalf("contributor.env has %d HIVE_HUB assignments, want 1:\n%s", n, data)
	}
	if !strings.Contains(string(data), "HIVE_HUB=wss://a.example/contribute") {
		t.Errorf("projection did not win:\n%s", data)
	}
}

// Round trip: a legacy file migrated and then projected back must describe the
// same hubs, tokens and ids, still aligned.
func TestMigrateThenProjectPreservesPairs(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	writeEnv(t, dir, "HIVE_HUB=wss://a.example/contribute,wss://b.example/contribute,wss://c.example/contribute\nHIVE_REGISTRATION_TOKEN=t1,t2,t3\nCONTRIBUTOR_ID=c1,c2,c3\n")

	set, _, err := store.LoadOrMigrate()
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	if err := store.WriteEnvProjection(set); err != nil {
		t.Fatalf("WriteEnvProjection: %v", err)
	}
	got := readEnvMap(t, store.EnvPath())
	assertAligned(t, got)
	if got["HIVE_HUB"] != "wss://a.example/contribute,wss://b.example/contribute,wss://c.example/contribute" {
		t.Errorf("HIVE_HUB = %q", got["HIVE_HUB"])
	}
	if got["HIVE_REGISTRATION_TOKEN"] != "t1,t2,t3" {
		t.Errorf("HIVE_REGISTRATION_TOKEN = %q", got["HIVE_REGISTRATION_TOKEN"])
	}
}

func TestValidateRejectsUnprojectableValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  ProfileSet
		want string
	}{
		{
			name: "comma in a token would split into two hubs' worth of values",
			set:  ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "to,k"}}},
			want: "comma",
		},
		{
			name: "comma in a hub URL",
			set:  ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute,wss://b.example/contribute", RegistrationToken: "t"}}},
			want: "comma",
		},
		{
			name: "shell metacharacter in a token",
			set:  ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "$(id)"}}},
			want: "sourced by a shell",
		},
		{
			name: "duplicate names differing only in case",
			set:  ProfileSet{Profiles: []Profile{{Name: "acme", Hub: "wss://a.example/contribute", RegistrationToken: "t"}, {Name: "ACME", Hub: "wss://b.example/contribute", RegistrationToken: "t2"}}},
			want: "duplicate hive profile name",
		},
		{
			name: "active names nothing",
			set:  ProfileSet{Active: "ghost", Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "t"}}},
			want: "not one of the configured profiles",
		},
		{
			name: "hub with an unusable scheme",
			set:  ProfileSet{Profiles: []Profile{{Name: "a", Hub: "ftp://a.example/contribute", RegistrationToken: "t"}}},
			want: "must use ws://",
		},
		{
			name: "empty name",
			set:  ProfileSet{Profiles: []Profile{{Name: "", Hub: "wss://a.example/contribute", RegistrationToken: "t"}}},
			want: "must be non-empty",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.set.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want one containing %q", err, tt.want)
			}
			// Save must refuse the same thing, so an invalid set can never
			// reach disk and be projected from there.
			if err := NewProfileStore(t.TempDir()).Save(&tt.set); err == nil {
				t.Error("Save accepted a set Validate rejects")
			}
		})
	}
}

func TestValidateProfileName(t *testing.T) {
	for _, ok := range []string{"a", "acme", "acme-prod", "acme_2", "hive.example", "A1"} {
		if err := ValidateProfileName(ok); err != nil {
			t.Errorf("ValidateProfileName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", " acme", "acme ", "acme/prod", "acme prod", "acme,prod", strings.Repeat("a", 65)} {
		if err := ValidateProfileName(bad); err == nil {
			t.Errorf("ValidateProfileName(%q) accepted an unusable name", bad)
		}
	}
}

func TestRedactedHidesTheToken(t *testing.T) {
	p := Profile{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "super-secret"}
	if got := p.Redacted(); strings.Contains(got.RegistrationToken, "super-secret") {
		t.Errorf("Redacted kept the token: %q", got.RegistrationToken)
	}
	if p.RegistrationToken != "super-secret" {
		t.Error("Redacted mutated the receiver")
	}
	if empty := (Profile{Name: "a"}).Redacted(); empty.RegistrationToken != "" {
		t.Errorf("Redacted invented a token placeholder for a profile without one: %q", empty.RegistrationToken)
	}
}

func TestProfileNameFromHub(t *testing.T) {
	for _, tt := range []struct{ hub, want string }{
		{"wss://acme.hive.hivecommons.dev/contribute", "acme"},
		{"wss://hive.hivecommons.dev/contribute", "hive"},
		{"ws://localhost:3001/contribute", "localhost"},
		{"wss://EXAMPLE.test/contribute", "example"},
		{"not a url", "not-a-url"},
	} {
		if got := ProfileNameFromHub(tt.hub); got != tt.want {
			t.Errorf("ProfileNameFromHub(%q) = %q, want %q", tt.hub, got, tt.want)
		}
	}
	if got := ProfileNameFromHub(""); got != "hive" {
		t.Errorf("ProfileNameFromHub(\"\") = %q, want the hive fallback", got)
	}
}

func TestHubHTTPBase(t *testing.T) {
	for _, tt := range []struct{ hub, want string }{
		{"wss://acme.example/contribute", "https://acme.example"},
		{"ws://localhost:3001/contribute", "http://localhost:3001"},
		{"https://acme.example/contribute", "https://acme.example"},
		{"wss://acme.example/base/contribute", "https://acme.example/base"},
		{"wss://acme.example", "https://acme.example"},
	} {
		got, err := HubHTTPBase(tt.hub)
		if err != nil {
			t.Fatalf("HubHTTPBase(%q): %v", tt.hub, err)
		}
		if got != tt.want {
			t.Errorf("HubHTTPBase(%q) = %q, want %q", tt.hub, got, tt.want)
		}
	}
	if _, err := HubHTTPBase("ftp://a.example"); err == nil {
		t.Error("HubHTTPBase accepted an unusable scheme")
	}
}

func TestActiveProfileFallsBackToTheFirstHub(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute"}, {Name: "b", Hub: "wss://b.example/contribute"}}}
	// No Active recorded: the relay starts at index 0, so "active" must mean
	// the first profile rather than nothing.
	if got := set.ActiveProfile(); got == nil || got.Name != "a" {
		t.Fatalf("ActiveProfile() = %+v, want the first profile", got)
	}
	if got := (&ProfileSet{}).ActiveProfile(); got != nil {
		t.Errorf("ActiveProfile() on an empty set = %+v, want nil", got)
	}
}

func readEnvMap(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		out[key] = value
	}
	return out
}

func assertAligned(t *testing.T, env map[string]string) {
	t.Helper()
	hubs := splitList(env["HIVE_HUB"])
	tokens := splitList(env["HIVE_REGISTRATION_TOKEN"])
	ids := splitList(env["CONTRIBUTOR_ID"])
	if len(hubs) != len(tokens) || len(hubs) != len(ids) {
		t.Fatalf("positional lists misaligned: %d hub(s), %d token(s), %d id(s)", len(hubs), len(tokens), len(ids))
	}
}
