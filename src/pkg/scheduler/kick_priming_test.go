package scheduler

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/knowledge"
)

// --- agentsRepoRoot ---

func TestAgentsRepoRoot_ReturnsEmpty(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if got := s.agentsRepoRoot(); got != "" {
		t.Errorf("agentsRepoRoot() = %q, want empty (no wired local checkout)", got)
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
