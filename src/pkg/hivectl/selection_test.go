package hivectl

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNormalizeHiveID(t *testing.T) {
	for _, tc := range []struct{ hub, want string }{
		{"wss://Hive.Example:443/contribute", "https://hive.example"},
		{"https://HIVE.example/api/contribute?x=1#fragment", "https://hive.example"},
		{"ws://Hive.Example:80/contribute", "http://hive.example"},
		{"http://hive.example:8080/contribute", "http://hive.example:8080"},
		{"wss://[2001:db8::1]:443/contribute", "https://[2001:db8::1]"},
	} {
		got, err := NormalizeHiveID(tc.hub)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeHiveID(%q) = %q, %v; want %q", tc.hub, got, err, tc.want)
		}
	}
	for _, hub := range []string{"not-a-url", "ftp://hive.example", "https://user:password@hive.example", "https://hive.example:99999", "https://hive.example:"} {
		if _, err := NormalizeHiveID(hub); err == nil {
			t.Errorf("accepted invalid hive %q", hub)
		}
	}
}

func TestNormalizeRepositoryID(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"GitHub/GitHub.COM/Owner/Repo", "github/github.com/owner/repo"},
		{"gitlab/gitlab.example/Group/SubGroup/Repo", "gitlab/gitlab.example/group/subgroup/repo"},
		{"gitea/forge.example:8443/Owner/Repo", "gitea/forge.example:8443/owner/repo"},
	} {
		got, err := NormalizeRepositoryID(tc.id)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeRepositoryID(%q) = %q, %v", tc.id, got, err)
		}
	}
	for _, id := range []string{"owner/repo", "github.com/owner/repo", "github/github.com/owner", "unknown/github.com/owner/repo", "github/github.com//repo", "github/github.com/../repo", "github/github.com/owner/repo;other", "github/github.com/owner/repo,other", "github/github.com/owner/repo\n", "github/user@host/owner/repo", "github/host?query/owner/repo", "github/host/owner/repo$VAR", "github/host/owner/repo'"} {
		if _, err := NormalizeRepositoryID(id); err == nil {
			t.Errorf("accepted invalid repo %q", id)
		}
	}
}

func TestEffectiveRepositorySelection(t *testing.T) {
	const a = "github/github.com/org/a"
	const b = "github/github.com/org/b"
	const c = "gitlab/gitlab.com/org/c"
	selected := &RepositorySelection{Mode: SelectionSelected, Repositories: []string{strings.ToUpper(a), a, b}, Include: []string{b, c}, Exclude: []string{strings.ToUpper(b)}}
	got, err := selected.EffectiveRepositories()
	if err != nil || !reflect.DeepEqual(got, []string{a, c}) {
		t.Fatalf("effective = %v, %v", got, err)
	}
	selected.Mode = SelectionAllCurrent
	selected.MembershipSnapshot = []string{b}
	selected.FollowNewRepositories = true
	got, err = selected.EffectiveRepositories()
	if err != nil || !reflect.DeepEqual(got, []string{c}) {
		t.Fatalf("all-current must use snapshot, not recommendations or a wildcard: %v, %v", got, err)
	}
	selected.Include = nil
	got, err = selected.EffectiveRepositories()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty selection = %v, %v", got, err)
	}
	selected.Mode = "everything"
	if _, err := selected.EffectiveRepositories(); err == nil {
		t.Fatal("accepted invalid mode")
	}
}

func TestEnsureHivePreservesCredentialAndConsent(t *testing.T) {
	selection := &RepositorySelection{Mode: SelectionSelected, Sources: []string{"first"}}
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{{Name: "saved", Hub: "wss://Hive.Example:443/contribute", ContributorID: "original", RegistrationToken: "secret", Selection: selection}}}
	target, added, err := set.EnsureHive(Profile{Name: "publisher", Hub: "https://hive.example/other", RegistrationToken: "replacement"}, true)
	if err != nil || added || len(set.Profiles) != 1 {
		t.Fatalf("overlap = %v, %v", added, err)
	}
	if target.Hub != "wss://Hive.Example:443/contribute" || target.RegistrationToken != "secret" || target.ContributorID != "original" || target.Selection != selection || set.Active != "saved" {
		t.Fatalf("overlap changed saved profile: %+v", target.Redacted())
	}
	// No token is inherited when the endpoint's origin changes.
	other, added, err := set.EnsureHive(Profile{Name: "other", Hub: "wss://other.example/contribute"}, false)
	if err != nil || !added || other.RegistrationToken != "" {
		t.Fatalf("changed origin reused credential: added=%v err=%v", added, err)
	}
	// Existing same-URL/different-ID subscriptions are never collapsed by lookup.
	if err := set.Add(Profile{Name: "second-identity", Hub: "https://hive.example", ContributorID: "second"}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.EnsureHive(Profile{Name: "third", Hub: "wss://hive.example/contribute"}, false); err == nil {
		t.Fatal("ambiguous identity was silently chosen")
	}
	if len(set.Profiles) != 3 {
		t.Fatal("legacy identities were collapsed")
	}
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryScopeProjectionIsAlignedAndShellSafe(t *testing.T) {
	const a = "github/github.com/org/a"
	const b = "github/github.com/org/b"
	set := &ProfileSet{Version: ProfilesVersion, Active: "empty", Profiles: []Profile{
		{Name: "legacy", Hub: "wss://same.example/contribute", ContributorID: "one", RegistrationToken: "token1"},
		{Name: "scoped", Hub: "wss://same.example/contribute", ContributorID: "two", RegistrationToken: "token2", Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{b, a}}},
		{Name: "empty", Hub: "wss://empty.example/contribute", RegistrationToken: "token3", Selection: &RepositorySelection{Mode: SelectionSelected}},
	}}
	store := NewProfileStore(t.TempDir())
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	env, err := readEnvFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := env.value("HIVE_HUB"); got != "wss://empty.example/contribute,wss://same.example/contribute,wss://same.example/contribute" {
		t.Fatalf("hubs = %q", got)
	}
	if got := env.value("HIVE_REGISTRATION_TOKEN"); got != "token3,token1,token2" {
		t.Fatalf("tokens misaligned: %q", got)
	}
	want := ",*," + a + ";" + b
	if got := env.value("HIVE_REPO_SCOPE"); got != "'"+want+"'" {
		t.Fatalf("scope = %q", got)
	}
	// A semicolon must survive actual shell sourcing, not execute a command.
	cmd := exec.Command("sh", "-c", ". \"$1\"; printf '%s' \"$HIVE_REPO_SCOPE\"", "scope-test", store.EnvPath())
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != want {
		t.Fatalf("shell projection = %q, %v", output, err)
	}
	set.Profiles[2].Selection.Exclude = []string{strings.ToUpper(a)}
	scopes, err := set.RepositoryScopes()
	if err != nil || !reflect.DeepEqual(scopes, []string{"", "*", b}) {
		t.Fatalf("global exclusion = %v, %v", scopes, err)
	}
}

func TestSelectionProvenanceRoundTripAndNoExport(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := &ProfileSet{Version: ProfilesVersion, Active: "a", Profiles: []Profile{{Name: "a", Hub: "wss://a.example/contribute", RegistrationToken: "secret", Selection: &RepositorySelection{Mode: SelectionAllCurrent, Include: []string{"github/github.com/org/personal"}, Exclude: []string{"github/github.com/org/excluded"}, MembershipSnapshot: []string{"github/github.com/org/accepted"}, Sources: []string{"bluefin"}}}}, Presets: []PresetProvenance{{ID: "bluefin", Source: "https://bluefin.example/preset.yml", ReviewedRevision: "2026-10-17", AcceptedAt: time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)}}}
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !reflect.DeepEqual(loaded, set) {
		t.Fatalf("round trip lost consent: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("store mode = %o", info.Mode().Perm())
	}
	profile := set.Profiles[0]
	// The existing explicit bundle allowlist must never acquire local selection.
	plain, err := json.Marshal(plainFromProfile(profile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "selection") || strings.Contains(string(plain), "bluefin") {
		t.Fatal("bundle contains local consent/provenance")
	}
	bundle, err := EncryptProfileBundle(profile, []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := DecryptProfileBundle(bundle, []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Selection != nil || imported.RegistrationToken != profile.RegistrationToken {
		t.Fatal("bundle must retain the existing encrypted credential but not local consent")
	}
	redacted, err := yaml.Marshal(profile.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "selection:") || strings.Contains(string(redacted), "secret") {
		t.Fatal("printable profile leaked local state or credentials")
	}
	if profile.Selection.FollowNewRepositories {
		t.Fatal("follow must default off")
	}
}

func TestLegacySelectionRoundTripAndProjectionKeys(t *testing.T) {
	legacy := "version: 1\nactive: a\nprofiles:\n  - name: a\n    hub: wss://a.example/contribute\n    registration_token: secret\n    contributor_id: contributor\n    session: review\n"
	store := NewProfileStore(t.TempDir())
	if err := os.WriteFile(store.Path(), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	set, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if set.Profiles[0].Selection != nil || set.Presets != nil {
		t.Fatal("invented selection/provenance")
	}
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"selection:", "presets:", "repo_scope_version:"} {
		if strings.Contains(string(data), field) {
			t.Errorf("legacy gained %s", field)
		}
	}
	existing := "# custom header\nHIVE_HUB=wss://a.example/contribute\nHIVE_REGISTRATION_TOKEN=secret\nCONTRIBUTOR_ID=contributor\nHIVE_HUB_DISABLED=false\nHIVE_SESSION=review\nHIVE_COMMONS_STRATEGY=ranked\nCUSTOM=keep\n"
	if err := os.WriteFile(store.EnvPath(), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEnvProjection(set); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing+"HIVE_REPO_SCOPE='*'\nHIVE_REPO_SCOPE_VERSION=0\n" {
		t.Fatalf("legacy keys changed: %s", data)
	}
}

func TestCommittedScopeVersionMonotonic(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{{Name: "a", Hub: "https://a.example"}}}
	for want := uint64(1); want <= 3; want++ {
		// Even a stale caller cannot roll the persisted generation backwards.
		set.RepoScopeVersion = 0
		if err := store.Commit(set); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		env, err := readEnvFile(store.EnvPath())
		if err != nil {
			t.Fatal(err)
		}
		if loaded.RepoScopeVersion != want || env.value("HIVE_REPO_SCOPE_VERSION") != strconv.FormatUint(want, 10) {
			t.Fatalf("version = %d; want %d", loaded.RepoScopeVersion, want)
		}
	}
	set.Profiles[0].Selection = &RepositorySelection{Mode: "invalid"}
	if err := store.Commit(set); err == nil {
		t.Fatal("committed invalid consent")
	}
	if set.RepoScopeVersion != 3 {
		t.Fatal("invalid commit advanced scope")
	}
}

func TestSelectionAndProvenanceValidation(t *testing.T) {
	for _, selection := range []*RepositorySelection{{}, {Mode: "all"}, {Mode: SelectionSelected, Include: []string{"owner/repo"}}, {Mode: SelectionSelected, MembershipSnapshot: []string{"*"}}, {Mode: SelectionSelected, Sources: []string{"bad source"}}} {
		set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{{Name: "a", Hub: "https://a.example", Selection: selection}}}
		if err := set.Validate(); err == nil {
			t.Errorf("accepted invalid selection %+v", selection)
		}
	}
	for _, presets := range [][]PresetProvenance{{{ID: "a"}}, {{ID: "a", Source: "file.yml", ReviewedRevision: "1"}, {ID: "A", Source: "file.yml", ReviewedRevision: "2"}}} {
		set := &ProfileSet{Version: ProfilesVersion, Presets: presets}
		if err := set.Validate(); err == nil {
			t.Fatal("accepted invalid provenance")
		}
	}
}

func TestFindHiveSkipsLegacyHostsThatCannotNormalize(t *testing.T) {
	legacy := Profile{Name: "compose", Hub: "ws://hive_hub:8080/contribute", RegistrationToken: "legacy-token"}
	if err := ValidateHubURL(legacy.Hub); err != nil {
		t.Fatalf("fixture must be a hub ValidateHubURL admits: %v", err)
	}
	if _, err := NormalizeHiveID(legacy.Hub); err == nil {
		t.Fatal("fixture must be a hub the canonical form rejects")
	}
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{
		legacy,
		{Name: "saved", Hub: "wss://hive.example/contribute", RegistrationToken: "secret"},
	}}
	found, err := set.FindHive("https://hive.example/other")
	if err != nil || found == nil || found.Name != "saved" {
		t.Fatalf("legacy host broke lookup for unrelated origin: found=%v err=%v", found, err)
	}
	target, added, err := set.EnsureHive(Profile{Name: "new", Hub: "wss://other.example/contribute"}, false)
	if err != nil || !added || target.RegistrationToken != "" || len(set.Profiles) != 3 {
		t.Fatalf("onboarding alongside legacy host: added=%v err=%v", added, err)
	}
	if set.Profiles[0].Hub != legacy.Hub || set.Profiles[0].RegistrationToken != legacy.RegistrationToken {
		t.Fatal("legacy profile was altered")
	}
}
