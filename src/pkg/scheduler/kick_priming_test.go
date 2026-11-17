package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// --- agentsRepoRoot ---

// A hive that configures no checkout keeps the previous behavior exactly: no
// root, so primeAgentsMd stays a no-op. This is the default and the common case.
func TestAgentsRepoRoot_UnconfiguredIsEmpty(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if got := s.agentsRepoRoot("hive"); got != "" {
		t.Errorf("agentsRepoRoot() = %q, want empty with no checkout configured", got)
	}
}

// checkouts_dir is the explicit source: each repo is at "<dir>/<name>", and an
// org-qualified slug resolves the same as the bare name.
func TestAgentsRepoRoot_FromCheckoutsDir(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.Project.CheckoutsDir = "/data/checkouts"
	s := New(cfg, slog.Default())

	want := filepath.Join("/data/checkouts", "hive")
	for _, repo := range []string{"hive", "hivecommons/hive"} {
		if got := s.agentsRepoRoot(repo); got != want {
			t.Errorf("agentsRepoRoot(%q) = %q, want %q", repo, got, want)
		}
	}
}

// Per-repo, not global: a multi-repo hive must get each repo's own root, never
// one repo's AGENTS.md injected into work on another.
func TestAgentsRepoRoot_IsPerRepo(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.CheckoutsDir = "/data/checkouts"
	s := New(cfg, slog.Default())

	a, b := s.agentsRepoRoot("alpha"), s.agentsRepoRoot("beta")
	if a == b {
		t.Fatalf("two repos resolved to the same root %q", a)
	}
	if got, want := a, filepath.Join("/data/checkouts", "alpha"); got != want {
		t.Errorf("agentsRepoRoot(alpha) = %q, want %q", got, want)
	}
}

// policies.local_dir is a real checkout root, but of policies.repo — so it is
// used only when that IS the repo being asked about.
func TestAgentsRepoRoot_FromPoliciesLocalDir(t *testing.T) {
	newCfg := func(policiesRepo string) *config.Config {
		cfg := &config.Config{}
		cfg.Project.Org = "hivecommons"
		cfg.Policies.Repo = policiesRepo
		cfg.Policies.LocalDir = "/data/policies"
		return cfg
	}

	t.Run("matching repo uses the checkout", func(t *testing.T) {
		for _, src := range []string{
			"https://github.com/hivecommons/hive",
			"https://github.com/hivecommons/hive.git",
			"hivecommons/hive",
			"hive",
		} {
			s := New(newCfg(src), slog.Default())
			if got := s.agentsRepoRoot("hive"); got != "/data/policies" {
				t.Errorf("policies.repo=%q: agentsRepoRoot = %q, want /data/policies", src, got)
			}
		}
	})

	t.Run("a different repo does not", func(t *testing.T) {
		s := New(newCfg("https://github.com/hivecommons/hive-config"), slog.Default())
		if got := s.agentsRepoRoot("hive"); got != "" {
			t.Errorf("agentsRepoRoot = %q, want empty — the policies checkout is a different repo", got)
		}
	})

	t.Run("a different org does not", func(t *testing.T) {
		s := New(newCfg("https://github.com/someone-else/hive"), slog.Default())
		if got := s.agentsRepoRoot("hive"); got != "" {
			t.Errorf("agentsRepoRoot = %q, want empty — same name, different owner", got)
		}
	})
}

// checkouts_dir wins when both are set: it is the explicit per-repo source,
// policies.local_dir is the incidental one.
func TestAgentsRepoRoot_CheckoutsDirWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.Project.CheckoutsDir = "/data/checkouts"
	cfg.Policies.Repo = "https://github.com/hivecommons/hive"
	cfg.Policies.LocalDir = "/data/policies"
	s := New(cfg, slog.Default())

	if got, want := s.agentsRepoRoot("hive"), filepath.Join("/data/checkouts", "hive"); got != want {
		t.Errorf("agentsRepoRoot = %q, want %q", got, want)
	}
}

// A repo name that would climb out of checkouts_dir resolves to no root rather
// than to a path outside it.
func TestAgentsRepoRoot_RejectsEscapingName(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.CheckoutsDir = "/data/checkouts"
	s := New(cfg, slog.Default())

	for _, repo := range []string{"..", ".", "a/../..", ""} {
		if got := s.agentsRepoRoot(repo); got != "" && !strings.HasPrefix(got, "/data/checkouts/") {
			t.Errorf("agentsRepoRoot(%q) = %q, escaped the checkouts dir", repo, got)
		}
	}
}

// The end-to-end claim of kubestellar/hive#5227: with a root threaded, a real
// AGENTS.md on disk reaches the kick prompt. Before this wiring the root was
// hardcoded empty and this was unreachable.
func TestAgentsRepoRoot_ThreadedRootReachesTheKick(t *testing.T) {
	checkouts := t.TempDir()
	root := filepath.Join(checkouts, "hive")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "Always run `make verify` before pushing."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.Project.PrimaryRepo = "hive"
	cfg.Project.CheckoutsDir = checkouts
	s := New(cfg, slog.Default())

	got := s.primeAgentsMd(cfg.Project.PrimaryRepo, s.agentsRepoRoot(cfg.Project.PrimaryRepo))
	if !strings.Contains(got, body) {
		t.Fatalf("AGENTS.md body did not reach the injection; got %q", got)
	}
}

// --- primeAgentsMd ---

func TestPrimeAgentsMd_EmptyRootIsNoOp(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if got := s.primeAgentsMd("", ""); got != "" {
		t.Errorf("primeAgentsMd(\"\", \"\") = %q, want empty", got)
	}
}

func TestPrimeAgentsMd_MissingFileYieldsEmpty(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	root := t.TempDir() // no AGENTS.md inside
	if got := s.primeAgentsMd("", root); got != "" {
		t.Errorf("primeAgentsMd(root without AGENTS.md) = %q, want empty", got)
	}
}

func TestPrimeAgentsMd_InjectsBody(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	root := t.TempDir()
	const body = "Always run `make verify` before pushing."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := s.primeAgentsMd("", root)
	if got == "" {
		t.Fatal("primeAgentsMd returned empty for a root with AGENTS.md")
	}
	if !strings.Contains(got, body) {
		t.Errorf("injection %q does not contain AGENTS.md body %q", got, body)
	}
	if !strings.Contains(got, "Repository Agent Instructions") {
		t.Errorf("injection %q missing the AGENTS.md injection header", got)
	}
}

func TestPrimeAgentsMd_EmptyAgentsMdYieldsEmpty(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("   \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.primeAgentsMd("", root); got != "" {
		t.Errorf("primeAgentsMd(blank AGENTS.md) = %q, want empty", got)
	}
}

// --- remoteAgentsRepoRoot (GitHub fallback, #17090) ---

type fakeAgentsMdFetcher struct {
	body  string
	err   error
	calls int
	last  [4]string
}

func (f *fakeAgentsMdFetcher) GetFileContentRef(_ context.Context, owner, repo, path, ref string) (string, error) {
	f.calls++
	f.last = [4]string{owner, repo, path, ref}
	return f.body, f.err
}

func useTempRemoteAgentsMdDir(t *testing.T) {
	t.Helper()
	old := remoteAgentsMdDir
	remoteAgentsMdDir = t.TempDir()
	t.Cleanup(func() { remoteAgentsMdDir = old })
}

// With no local root and no fetcher wired the behaviour is unchanged: nothing.
func TestAgentsRepoRoot_NoFetcherStaysEmpty(t *testing.T) {
	useTempRemoteAgentsMdDir(t)
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	s := New(cfg, slog.Default())
	if got := s.agentsRepoRoot("hive"); got != "" {
		t.Errorf("agentsRepoRoot() = %q, want empty with no fetcher", got)
	}
}

// A hosted spoke with no checkout gets the primary repo's AGENTS.md from the
// forge's default branch, materialised so the existing priming path injects it,
// and one fetch serves every kick inside the TTL.
func TestAgentsRepoRoot_FetchesFromGitHubWhenNoLocalRoot(t *testing.T) {
	useTempRemoteAgentsMdDir(t)
	const body = "Always add a changelog.d fragment."
	f := &fakeAgentsMdFetcher{body: body + "\n"}

	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.PrimaryRepo = "hive"
	s := New(cfg, slog.Default())
	s.SetAgentsMdFetcher(f)

	root := s.agentsRepoRoot("hive")
	if want := filepath.Join(remoteAgentsMdDir, "hivecommons", "hive"); root != want {
		t.Fatalf("agentsRepoRoot() = %q, want %q", root, want)
	}
	if f.last != [4]string{"hivecommons", "hive", "AGENTS.md", ""} {
		t.Errorf("fetched %v, want owner/repo AGENTS.md at the default branch", f.last)
	}
	if got := s.primeAgentsMd("", root); !strings.Contains(got, body) {
		t.Fatalf("fetched AGENTS.md did not reach the injection; got %q", got)
	}

	for i := 0; i < 3; i++ {
		if again := s.agentsRepoRoot("hivecommons/hive"); again != root {
			t.Fatalf("agentsRepoRoot() = %q on repeat, want cached %q", again, root)
		}
	}
	if f.calls != 1 {
		t.Errorf("fetch calls = %d, want 1 (served from cache inside the TTL)", f.calls)
	}
}

// A repo without AGENTS.md (404) yields no root and is not re-asked inside
// the TTL; a later failure keeps serving the last-known-good copy.
func TestAgentsRepoRoot_RemoteMissAndLastKnownGood(t *testing.T) {
	useTempRemoteAgentsMdDir(t)
	f := &fakeAgentsMdFetcher{err: errors.New("404 Not Found")}
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	s := New(cfg, slog.Default())
	s.SetAgentsMdFetcher(f)

	if got := s.agentsRepoRoot("hive"); got != "" {
		t.Fatalf("agentsRepoRoot() = %q, want empty on 404", got)
	}
	s.agentsRepoRoot("hive")
	if f.calls != 1 {
		t.Errorf("fetch calls = %d, want 1 (negative result cached)", f.calls)
	}

	// Expire the cache, succeed once, then fail: the good copy must survive.
	s.remoteAgents = nil
	f.err, f.body = nil, "rule\n"
	root := s.agentsRepoRoot("hive")
	if root == "" {
		t.Fatal("agentsRepoRoot() empty after a successful fetch")
	}
	s.remoteAgents["hivecommons/hive"] = remoteAgentsMd{root: root, fetched: time.Now().Add(-2 * remoteAgentsMdTTL)}
	f.err = errors.New("502 Bad Gateway")
	if got := s.agentsRepoRoot("hive"); got != root {
		t.Errorf("agentsRepoRoot() = %q after refresh failure, want last-known-good %q", got, root)
	}
}

// A configured local root still wins over the forge: no fetch happens.
func TestAgentsRepoRoot_LocalRootBeatsFetch(t *testing.T) {
	useTempRemoteAgentsMdDir(t)
	f := &fakeAgentsMdFetcher{body: "x"}
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.CheckoutsDir = "/data/checkouts"
	s := New(cfg, slog.Default())
	s.SetAgentsMdFetcher(f)
	if got, want := s.agentsRepoRoot("hive"), filepath.Join("/data/checkouts", "hive"); got != want {
		t.Errorf("agentsRepoRoot() = %q, want %q", got, want)
	}
	if f.calls != 0 {
		t.Errorf("fetch calls = %d, want 0 when a local root exists", f.calls)
	}
}
