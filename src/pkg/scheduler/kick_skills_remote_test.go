package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// pathFetcher answers GetFileContentRef per repo path, so a test can serve
// AGENTS.md and individual skills/ files differently. A path with no entry is
// a typed GitHub 404, which is what the contents API returns.
type pathFetcher struct {
	files map[string]string
	errs  map[string]error
	calls map[string]int
}

func newPathFetcher() *pathFetcher {
	return &pathFetcher{files: map[string]string{}, errs: map[string]error{}, calls: map[string]int{}}
}

func gitHub404() error {
	return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
}

func (f *pathFetcher) GetFileContentRef(_ context.Context, _, _, path, _ string) (string, error) {
	f.calls[path]++
	if err, ok := f.errs[path]; ok {
		return "", err
	}
	if body, ok := f.files[path]; ok {
		return body, nil
	}
	return "", gitHub404()
}

// remoteSkillsScheduler is a hosted-style spoke: no checkout, a fetcher wired,
// and agent "scanner" declaring the given skills.
func remoteSkillsScheduler(t *testing.T, f AgentsMdFetcher, skills ...string) *Scheduler {
	t.Helper()
	useTempRemoteAgentsMdDir(t)
	useSkillsDir(t)
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"scanner": {Skills: skills}},
		},
	}
	cfg.Project.Org = "hivecommons"
	s := New(cfg, slog.Default())
	s.SetAgentsMdFetcher(f)
	return s
}

func primeRemote(s *Scheduler) string {
	return s.primeSkillsForRepo("scanner", "hive", s.agentsRepoRoot("hive"))
}

// The reported case: a spoke with no checkout and a repo with skills/ but no
// AGENTS.md. The declared skill is fetched through the API and injected, and
// one fetch serves every kick inside the TTL.
func TestPrimeSkillsForRepo_FetchesDeclaredRepoSkillWithoutCheckout(t *testing.T) {
	f := newPathFetcher()
	f.files["skills/deploy.md"] = "---\nname: deploy\n---\nRun the canary first.\n"
	s := remoteSkillsScheduler(t, f, "deploy")

	got := primeRemote(s)
	if !strings.Contains(got, "Run the canary first.") {
		t.Fatalf("repo skill not injected without a checkout; got %q", got)
	}
	if strings.Contains(got, "name: deploy") {
		t.Errorf("front matter of a fetched repo skill reached the prompt: %q", got)
	}
	for i := 0; i < 3; i++ {
		primeRemote(s)
	}
	if n := f.calls["skills/deploy.md"]; n != 1 {
		t.Errorf("skills/deploy.md fetched %d times, want 1 inside the TTL", n)
	}
}

// Only declared names are fetched, and a registry skill wins, so it is never
// fetched from the repo at all.
func TestPrimeSkillsForRepo_RegistrySkillIsNotFetched(t *testing.T) {
	f := newPathFetcher()
	f.files["skills/go-testing.md"] = "REPO VERSION"
	s := remoteSkillsScheduler(t, f, "go-testing")
	writeSkill(t, skillsRegistryDir, "go-testing.md", "REGISTRY VERSION")

	got := primeRemote(s)
	if !strings.Contains(got, "REGISTRY VERSION") || strings.Contains(got, "REPO VERSION") {
		t.Errorf("registry must win over the repo; got %q", got)
	}
	if n := f.calls["skills/go-testing.md"]; n != 0 {
		t.Errorf("registry skill fetched from the repo %d times, want 0", n)
	}
	for path := range f.calls {
		if strings.HasPrefix(path, "skills/") && path != "skills/go-testing.md" {
			t.Errorf("undeclared skill path fetched: %s", path)
		}
	}
}

// A skill deleted upstream must leave the prompt: the refresh 404 removes the
// cached copy. A transient failure keeps the last-known-good copy instead.
func TestPrimeSkillsForRepo_RefreshFailureKeepsCopyAnd404RemovesIt(t *testing.T) {
	f := newPathFetcher()
	f.files["skills/deploy.md"] = "Run the canary first."
	s := remoteSkillsScheduler(t, f, "deploy")
	if got := primeRemote(s); !strings.Contains(got, "Run the canary first.") {
		t.Fatalf("first kick did not inject the skill; got %q", got)
	}
	expire := func() {
		s.remoteAgentsMu.Lock()
		for k := range s.remoteSkills {
			s.remoteSkills[k] = time.Now().Add(-2 * remoteAgentsMdTTL)
		}
		s.remoteAgentsMu.Unlock()
	}

	expire()
	f.errs["skills/deploy.md"] = errors.New("502 Bad Gateway")
	if got := primeRemote(s); !strings.Contains(got, "Run the canary first.") {
		t.Errorf("transient refresh failure dropped the cached skill; got %q", got)
	}

	expire()
	delete(f.errs, "skills/deploy.md")
	delete(f.files, "skills/deploy.md")
	if got := primeRemote(s); got != "" {
		t.Errorf("skill removed upstream is still injected: %q", got)
	}
	if _, err := os.Stat(filepath.Join(remoteAgentsMdDir, "hivecommons", "hive", "skills", "deploy.md")); !os.IsNotExist(err) {
		t.Errorf("cached skill file not deleted after a 404, stat err = %v", err)
	}
}

// A file that could never fit the injection cap is not cached or injected.
func TestPrimeSkillsForRepo_OversizedRepoSkillIsNotCached(t *testing.T) {
	f := newPathFetcher()
	f.files["skills/huge.md"] = strings.Repeat("x", maxSkillsInjectionBytes+1)
	s := remoteSkillsScheduler(t, f, "huge")
	if got := primeRemote(s); got != "" {
		t.Errorf("oversized repo skill injected (%d chars)", len(got))
	}
	if _, err := os.Stat(filepath.Join(remoteAgentsMdDir, "hivecommons", "hive", "skills", "huge.md")); !os.IsNotExist(err) {
		t.Errorf("oversized repo skill was written to the cache, stat err = %v", err)
	}
}

// A declared name that is not a plain file name is never sent to the API and
// never joined into a cache path.
func TestPrimeSkillsForRepo_UnsafeSkillNameIsNotFetched(t *testing.T) {
	f := newPathFetcher()
	names := []string{"../escape", "a/b", `a\b`, "..", ".hidden"}
	s := remoteSkillsScheduler(t, f, names...)
	primeRemote(s)
	for path := range f.calls {
		if strings.HasPrefix(path, "skills/") {
			t.Errorf("unsafe skill name reached the API as %q", path)
		}
	}
}

// A checkout that has content stays authoritative: no skill is fetched.
func TestPrimeSkillsForRepo_LocalCheckoutIsNotOverriddenByFetch(t *testing.T) {
	f := newPathFetcher()
	f.files["skills/deploy.md"] = "REMOTE"
	s := remoteSkillsScheduler(t, f, "deploy")
	checkouts := t.TempDir()
	writeFileForTest(t, filepath.Join(checkouts, "hive", "skills", "deploy.md"), "LOCAL")
	s.cfg.Project.CheckoutsDir = checkouts

	got := primeRemote(s)
	if !strings.Contains(got, "LOCAL") || strings.Contains(got, "REMOTE") {
		t.Errorf("local checkout skill not used; got %q", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("fetcher called %v with a populated checkout", f.calls)
	}
}

// A configured checkout root that is missing or empty no longer turns
// injection off: with a fetcher wired, Hive falls back to the forge.
func TestAgentsRepoRoot_EmptyCheckoutFallsBackToGitHub(t *testing.T) {
	f := newPathFetcher()
	f.files["AGENTS.md"] = "remote rule\n"
	f.files["skills/deploy.md"] = "Run the canary first."
	s := remoteSkillsScheduler(t, f, "deploy")
	s.cfg.Project.CheckoutsDir = t.TempDir() // no "hive" inside

	root := s.agentsRepoRoot("hive")
	if want := filepath.Join(remoteAgentsMdDir, "hivecommons", "hive"); root != want {
		t.Fatalf("agentsRepoRoot() = %q, want the GitHub cache root %q", root, want)
	}
	if got := s.primeSkillsForRepo("scanner", "hive", root); !strings.Contains(got, "Run the canary first.") {
		t.Errorf("repo skill not injected after the checkout fallback; got %q", got)
	}
}

// Without a fetcher there is nothing to fall back to, so a configured root is
// returned as before even when it is empty.
func TestAgentsRepoRoot_EmptyCheckoutWithoutFetcherKeepsRoot(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.CheckoutsDir = t.TempDir()
	s := New(cfg, slog.Default())
	if got, want := s.agentsRepoRoot("hive"), filepath.Join(cfg.Project.CheckoutsDir, "hive"); got != want {
		t.Errorf("agentsRepoRoot() = %q, want %q", got, want)
	}
}

// AGENTS.md deleted upstream must stop reaching kicks: a refresh 404 removes
// the cached copy rather than serving it as last-known-good.
func TestAgentsRepoRoot_RemoteAgentsMd404RemovesCachedCopy(t *testing.T) {
	f := newPathFetcher()
	f.files["AGENTS.md"] = "old rule\n"
	s := remoteSkillsScheduler(t, f)
	root := s.agentsRepoRoot("hive")
	if root == "" {
		t.Fatal("agentsRepoRoot() empty after a successful fetch")
	}
	s.remoteAgentsMu.Lock()
	s.remoteAgents["hivecommons/hive"] = remoteAgentsMd{root: root, fetched: time.Now().Add(-2 * remoteAgentsMdTTL)}
	s.remoteAgentsMu.Unlock()
	delete(f.files, "AGENTS.md")

	if got := s.agentsRepoRoot("hive"); got != "" {
		t.Errorf("agentsRepoRoot() = %q after AGENTS.md was deleted upstream, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("cached AGENTS.md not removed after a 404, stat err = %v", err)
	}
}

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
