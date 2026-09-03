package scheduler

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
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
	for _, repo := range []string{"hive", "kubestellar/hive"} {
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

	got := s.primeAgentsMd(s.agentsRepoRoot(cfg.Project.PrimaryRepo))
	if !strings.Contains(got, body) {
		t.Fatalf("AGENTS.md body did not reach the injection; got %q", got)
	}
}

// --- primeAgentsMd ---

func TestPrimeAgentsMd_EmptyRootIsNoOp(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if got := s.primeAgentsMd(""); got != "" {
		t.Errorf("primeAgentsMd(\"\") = %q, want empty", got)
	}
}

func TestPrimeAgentsMd_MissingFileYieldsEmpty(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	root := t.TempDir() // no AGENTS.md inside
	if got := s.primeAgentsMd(root); got != "" {
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

	got := s.primeAgentsMd(root)
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
	if got := s.primeAgentsMd(root); got != "" {
		t.Errorf("primeAgentsMd(blank AGENTS.md) = %q, want empty", got)
	}
}

// --- inceptionVars ---

// writeInceptionState persists a crafted state file where NewInceptionEngine
// loads it from, so tests can exercise states not reachable via Start alone.
func writeInceptionState(t *testing.T, dataDir string, state knowledge.InceptionState) {
	t.Helper()
	dir := filepath.Join(dataDir, "inception")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInceptionVars_NoEngine(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	idea, phase, mode, answers, slug, repoURL := s.inceptionVars()
	if idea != "" || phase != "" || mode != "" || answers != "" || slug != "" || repoURL != "" {
		t.Errorf("inceptionVars() with no engine = (%q,%q,%q,%q,%q,%q), want all empty",
			idea, phase, mode, answers, slug, repoURL)
	}
}

func TestInceptionVars_EngineWithoutState(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	s.SetInception(knowledge.NewInceptionEngine(t.TempDir(), nil, nil))

	idea, phase, mode, answers, slug, repoURL := s.inceptionVars()
	if idea != "" || phase != "" || mode != "" || answers != "" || slug != "" || repoURL != "" {
		t.Errorf("inceptionVars() with stateless engine = (%q,%q,%q,%q,%q,%q), want all empty",
			idea, phase, mode, answers, slug, repoURL)
	}
}

func TestInceptionVars_GreenfieldState(t *testing.T) {
	dataDir := t.TempDir()
	writeInceptionState(t, dataDir, knowledge.InceptionState{
		Phase:    knowledge.PhaseCapture,
		Mode:     knowledge.InceptionGreenfield,
		IdeaText: "a CLI that summarizes build logs",
		IdeaSlug: "idea-cli-build-logs",
		Questions: []knowledge.Question{
			{ID: "q1", Text: "Which language?", Category: "language"},
		},
		Answers:   map[string]string{"q1": "Go"},
		FactSlugs: []string{"idea-cli-build-logs"},
		StartedAt: time.Now(),
	})

	s := New(&config.Config{}, slog.Default())
	s.SetInception(knowledge.NewInceptionEngine(dataDir, nil, nil))

	idea, phase, mode, answers, slug, repoURL := s.inceptionVars()
	if idea != "a CLI that summarizes build logs" {
		t.Errorf("idea = %q, want the greenfield idea text", idea)
	}
	if phase != string(knowledge.PhaseCapture) {
		t.Errorf("phase = %q, want %q", phase, knowledge.PhaseCapture)
	}
	if mode != string(knowledge.InceptionGreenfield) {
		t.Errorf("mode = %q, want %q", mode, knowledge.InceptionGreenfield)
	}
	if slug != "idea-cli-build-logs" {
		t.Errorf("slug = %q, want idea-cli-build-logs", slug)
	}
	if repoURL != "" {
		t.Errorf("repoURL = %q, want empty for greenfield", repoURL)
	}
	if !strings.Contains(answers, "Which language?") || !strings.Contains(answers, "Go") {
		t.Errorf("answers = %q, want rendered question and answer", answers)
	}
}

func TestInceptionVars_BrownfieldIdeaFallsBackToRepoURL(t *testing.T) {
	dataDir := t.TempDir()
	writeInceptionState(t, dataDir, knowledge.InceptionState{
		Phase:     knowledge.PhaseCapture,
		Mode:      knowledge.InceptionBrownfield,
		IdeaText:  "", // crafted: force the RepoURL fallback branch
		IdeaSlug:  "idea-brownfield-repo",
		RepoURL:   "https://github.com/example/legacy",
		Answers:   map[string]string{},
		FactSlugs: []string{},
		StartedAt: time.Now(),
	})

	s := New(&config.Config{}, slog.Default())
	s.SetInception(knowledge.NewInceptionEngine(dataDir, nil, nil))

	idea, _, mode, answers, _, repoURL := s.inceptionVars()
	if mode != string(knowledge.InceptionBrownfield) {
		t.Errorf("mode = %q, want %q", mode, knowledge.InceptionBrownfield)
	}
	if repoURL != "https://github.com/example/legacy" {
		t.Errorf("repoURL = %q, want the brownfield repo URL", repoURL)
	}
	if idea != repoURL {
		t.Errorf("idea = %q, want fallback to repoURL %q when IdeaText is empty", idea, repoURL)
	}
	if answers != "" {
		t.Errorf("answers = %q, want empty with no recorded answers", answers)
	}
}

func TestInceptionVars_GreenfieldEmptyIdeaDoesNotFallBack(t *testing.T) {
	dataDir := t.TempDir()
	writeInceptionState(t, dataDir, knowledge.InceptionState{
		Phase:     knowledge.PhaseCapture,
		Mode:      knowledge.InceptionGreenfield,
		IdeaText:  "",
		IdeaSlug:  "idea-empty",
		RepoURL:   "https://github.com/example/should-not-leak",
		Answers:   map[string]string{},
		FactSlugs: []string{},
		StartedAt: time.Now(),
	})

	s := New(&config.Config{}, slog.Default())
	s.SetInception(knowledge.NewInceptionEngine(dataDir, nil, nil))

	idea, _, _, _, _, _ := s.inceptionVars()
	if idea != "" {
		t.Errorf("idea = %q, want empty: RepoURL fallback is brownfield-only", idea)
	}
}
