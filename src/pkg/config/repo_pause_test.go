package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPauseTestConfig builds a Config that satisfies validateSaveGuard (org +
// at least one agent) and persists only into dir.
func newPauseTestConfig(t *testing.T, repos ...string) *Config {
	t.Helper()
	dir := t.TempDir()
	hermeticPersistPaths(t, dir)
	return &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "acme", Repos: repos},
		GitHub:     GitHubConfig{AppID: 3568013},
		Agents:     map[string]AgentConfig{"scanner": {Backend: "claude"}},
		Data:       DataConfig{AgentsDir: t.TempDir()},
	}
}

// A pause must match the repo however it is spelled. project.repos accepts a
// bare name and an explicit cross-org reference, and GitHub repo names are
// case-insensitive — so an operator who pauses "Console" must not find agents
// still writing to "acme/console".
func TestIsRepoPaused_MatchesEverySpelling(t *testing.T) {
	cfg := newPauseTestConfig(t, "console", "laredo/cuga-agent")
	cfg.Project.PausedRepos = []RepoPause{{Repo: "Console"}, {Repo: "laredo/cuga-agent"}}

	for _, spelling := range []string{"console", "Console", "acme/console", "ACME/Console"} {
		if !cfg.IsRepoPaused(spelling) {
			t.Errorf("IsRepoPaused(%q) = false, want true", spelling)
		}
	}
	if !cfg.IsRepoPaused("laredo/cuga-agent") {
		t.Error("cross-org entry not matched by its own spelling")
	}
	// A same-named repo in a DIFFERENT org is a different repository.
	if cfg.IsRepoPaused("other/console") {
		t.Error("IsRepoPaused matched a same-named repo in another org")
	}
	if cfg.IsRepoPaused("dashboard") {
		t.Error("IsRepoPaused matched an unpaused repo")
	}
}

// ActiveRepos is what agent-facing surfaces enumerate; Project.Repos is what
// operator-facing ones do. Keeping both is the whole difference between pausing
// a repo and deleting it from config.
func TestActiveRepos_DropsPausedKeepsProjectRepos(t *testing.T) {
	cfg := newPauseTestConfig(t, "console", "dashboard", "docs")
	cfg.Project.PausedRepos = []RepoPause{{Repo: "dashboard"}}

	got := cfg.ActiveRepos()
	want := []string{"console", "docs"}
	if len(got) != len(want) {
		t.Fatalf("ActiveRepos() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ActiveRepos() = %v, want %v (order must be preserved)", got, want)
		}
	}
	if len(cfg.Project.Repos) != 3 {
		t.Errorf("Project.Repos was mutated to %v — a paused repo must stay watched", cfg.Project.Repos)
	}
}

// With nothing paused the hive must behave exactly as before, down to not
// re-allocating the repo list on every kick.
func TestActiveRepos_NothingPausedReturnsProjectRepos(t *testing.T) {
	cfg := newPauseTestConfig(t, "console", "dashboard")
	got := cfg.ActiveRepos()
	if len(got) != 2 || got[0] != "console" || got[1] != "dashboard" {
		t.Fatalf("ActiveRepos() = %v, want the unfiltered list", got)
	}
	if &got[0] != &cfg.Project.Repos[0] {
		t.Error("ActiveRepos allocated a copy with nothing paused; the no-pause path should return Project.Repos itself")
	}
}

// The pause must survive a restart — the guarantee AgentConfig.Paused had to
// earn the hard way — and must carry who/when/why with it.
func TestSetRepoPausedAndSave_PersistsWithProvenance(t *testing.T) {
	cfg := newPauseTestConfig(t, "console", "dashboard")

	changed, err := cfg.SetRepoPausedAndSave("dashboard", true, "bketelsen", "release freeze")
	if err != nil {
		t.Fatalf("SetRepoPausedAndSave: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a real pause")
	}

	raw, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatalf("reading persisted config: %v", err)
	}
	for _, want := range []string{"paused_repos", "dashboard", "bketelsen", "release freeze"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("persisted config missing %q:\n%s", want, raw)
		}
	}

	reloaded, err := Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rp, paused := reloaded.RepoPauseFor("dashboard")
	if !paused {
		t.Fatal("pause did not survive the reload — the restart-safety guarantee is the point of persisting it")
	}
	if rp.By != "bketelsen" || rp.Reason != "release freeze" {
		t.Errorf("provenance lost across restart: %+v", rp)
	}
	if rp.At == nil || rp.At.IsZero() {
		t.Error("PausedAt lost across restart")
	}
	if reloaded.IsRepoPaused("console") {
		t.Error("pausing one repo paused another")
	}
}

// Re-pausing an already-paused repo must not restamp who/when/why: a stale
// dashboard re-issuing a pause would otherwise quietly rewrite the record of
// why a repo has been quiet since Tuesday.
func TestSetRepoPausedAndSave_RepauseKeepsOriginalProvenance(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	if _, err := cfg.SetRepoPausedAndSave("console", true, "alice", "incident"); err != nil {
		t.Fatalf("first pause: %v", err)
	}
	first, _ := cfg.RepoPauseFor("console")

	changed, err := cfg.SetRepoPausedAndSave("console", true, "bob", "something else")
	if err != nil {
		t.Fatalf("second pause: %v", err)
	}
	if changed {
		t.Error("changed = true for a no-op re-pause")
	}
	second, _ := cfg.RepoPauseFor("console")
	if second.By != first.By || second.Reason != first.Reason || !second.At.Equal(*first.At) {
		t.Errorf("re-pause rewrote the provenance: %+v -> %+v", first, second)
	}
	if n := len(cfg.Project.PausedRepos); n != 1 {
		t.Errorf("re-pause added a duplicate entry: %d entries", n)
	}
}

func TestSetRepoPausedAndSave_ResumeClearsAndPersists(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	if _, err := cfg.SetRepoPausedAndSave("console", true, "alice", "freeze"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Resume by a DIFFERENT spelling than the pause used: an operator resuming
	// from the dashboard sends whatever the card shows.
	changed, err := cfg.SetRepoPausedAndSave("acme/console", false, "alice", "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a real resume")
	}
	if cfg.IsRepoPaused("console") {
		t.Error("repo still paused after resume")
	}

	reloaded, err := Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.IsRepoPaused("console") {
		t.Error("resume did not survive the reload")
	}

	// Resuming an already-running repo is a no-op, not an error.
	changed, err = cfg.SetRepoPausedAndSave("console", false, "alice", "")
	if err != nil || changed {
		t.Errorf("no-op resume: changed=%v err=%v, want false/nil", changed, err)
	}
}

// A hive that has never used the feature must load and save byte-identically
// to before: paused_repos is omitempty, so nothing appears in its config.
func TestPausedRepos_AbsentFromConfigWhenUnused(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "paused_repos") {
		t.Errorf("paused_repos written for a hive with no pauses:\n%s", raw)
	}
	if cfg.PausedRepoNames() != nil {
		t.Error("PausedRepoNames() should be nil when nothing is paused")
	}
	if cfg.PausedRepoSet() != nil {
		t.Error("PausedRepoSet() should be nil when nothing is paused")
	}
}

// A pause naming a repo the hive does not watch is inert. It must be reported,
// not silently obeyed and not fatal — refusing to boot over a stale entry would
// be a worse failure than an ignored one.
func TestPausedRepoWarnings(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	cfg.Project.PausedRepos = []RepoPause{
		{Repo: "console"},
		{Repo: "typo-repo"},
		{Repo: "   "},
	}
	warnings := PausedRepoWarnings(cfg)
	if len(warnings) != 2 {
		t.Fatalf("PausedRepoWarnings() = %v, want 2 (unwatched + empty)", warnings)
	}
	if !strings.Contains(warnings[0], "typo-repo") {
		t.Errorf("warning does not name the offending repo: %q", warnings[0])
	}
	if cfg.IsRepoPaused("console") == false {
		t.Error("a warning must not disable the pauses that ARE valid")
	}
}

func TestQualifyRepo(t *testing.T) {
	for _, tt := range []struct{ org, repo, want string }{
		{"acme", "console", "acme/console"},
		{"acme", "laredo/cuga-agent", "laredo/cuga-agent"}, // already cross-org
		{"", "console", "console"},
		{"acme", "  console  ", "acme/console"},
		{"acme", "", ""},
	} {
		if got := QualifyRepo(tt.org, tt.repo); got != tt.want {
			t.Errorf("QualifyRepo(%q, %q) = %q, want %q", tt.org, tt.repo, got, tt.want)
		}
	}
}

func TestSetRepoPausedAndSave_RejectsEmptyRepo(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	if _, err := cfg.SetRepoPausedAndSave("  ", true, "alice", ""); err == nil {
		t.Error("expected an error for an empty repo name")
	}
}

// A nil Config must answer "nothing is paused" rather than panicking: the
// predicate is handed to the proxy and the GitHub client, which run on every
// request and every sweep.
func TestRepoPauseNilConfigIsSafe(t *testing.T) {
	var cfg *Config
	if cfg.IsRepoPaused("console") {
		t.Error("nil config reported a pause")
	}
	if cfg.ActiveRepos() != nil || cfg.PausedRepoNames() != nil || cfg.PausedRepoSet() != nil {
		t.Error("nil config returned non-nil repo lists")
	}
	if _, err := cfg.SetRepoPausedAndSave("console", true, "", ""); err == nil {
		t.Error("expected an error pausing on a nil config")
	}
	if got := PausedRepoWarnings(nil); got != nil {
		t.Errorf("PausedRepoWarnings(nil) = %v, want nil", got)
	}
}

// A hand-written pause with no timestamp is a real state — "unknown", not the
// zero time — and must round-trip as such.
func TestRepoPause_HandWrittenEntryHasNoTimestamp(t *testing.T) {
	cfg := newPauseTestConfig(t, "console")
	cfg.Project.PausedRepos = []RepoPause{{Repo: "console", Reason: "not onboarded yet"}}
	rp, paused := cfg.RepoPauseFor("console")
	if !paused {
		t.Fatal("hand-written pause not honoured")
	}
	if rp.At != nil {
		t.Errorf("At = %v, want nil for an entry with no timestamp", rp.At)
	}
	if rp.By != "" {
		t.Errorf("By = %q, want empty for an unattributed entry", rp.By)
	}
}
