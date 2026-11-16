package scheduler

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentsmd"
	"github.com/hivecommons/hive/pkg/config"
)

const agentsMdCapWarning = "AGENTS.md exceeded the injection cap"

func writeAgentsMd(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, agentsmd.AgentsFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func thirteenKBAgentsMd() string {
	var b strings.Builder
	for i := 0; b.Len() < 13*1000; i++ {
		b.WriteString("## Rule " + string(rune('A'+i)) + "\n\n" + strings.Repeat("Follow the rule. ", 55) + "\n\n")
	}
	return b.String()
}

// #18495: with nothing configured, a 13 KB AGENTS.md reaches the kick whole.
func TestPrimeAgentsMd_ThirteenKBIncludedInFullByDefault(t *testing.T) {
	t.Setenv(agentsmd.MaxBytesEnv, "")
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(&config.Config{Project: config.ProjectConfig{Org: "acme"}}, logger)
	root := t.TempDir()
	file := thirteenKBAgentsMd()
	writeAgentsMd(t, root, file)

	got := s.primeAgentsMd("widgets13", root)
	if strings.Contains(got, agentsmd.InjectionTruncatedMarker) || strings.Contains(logBuf.String(), agentsMdCapWarning) {
		t.Fatalf("13 KB AGENTS.md was truncated by default:\n%s", logBuf.String())
	}
	if !strings.Contains(got, strings.TrimSpace(file[strings.LastIndex(file, "## Rule"):])) {
		t.Error("last section of the 13 KB file missing from the kick")
	}
	st, ok := agentsmd.StatusFor("acme/widgets13")
	if !ok {
		t.Fatal("injection status not recorded")
	}
	if st.Truncated || st.SourceBytes != len(file) || st.CapBytes != agentsmd.DefaultMaxInjectionBytes || st.RenderedBytes != len(got) {
		t.Errorf("status wrong: %+v (file %d bytes)", st, len(file))
	}
}

// A per-repo cap overrides the project cap for that repo only.
func TestPrimeAgentsMd_PerRepoOverride(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme"},
		Repos:   map[string]config.RepoSettings{"tight": {AgentsMdMaxBytes: 4096}},
	}
	s := New(cfg, logger)
	root := t.TempDir()
	writeAgentsMd(t, root, thirteenKBAgentsMd())

	got := s.primeAgentsMd("tight", root)
	if len(got) > 4096 || !strings.Contains(got, agentsmd.InjectionTruncatedMarker+" 4096 bytes") {
		t.Fatalf("per-repo cap not applied (%d bytes):\n%s", len(got), got)
	}
	st, ok := agentsmd.StatusFor("acme/tight")
	if !ok || !st.Truncated || st.CapBytes != 4096 || len(st.OmittedSections) == 0 {
		t.Errorf("truncation not reported in status: %+v", st)
	}
	// Another repo with the same file inherits the (default) project cap.
	if other := s.primeAgentsMd("loose", root); strings.Contains(other, agentsmd.InjectionTruncatedMarker) {
		t.Error("per-repo cap leaked into another repo")
	}
}

// The truncation warning is logged once per content change, not every kick.
func TestPrimeAgentsMd_WarnsOncePerContentChange(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(&config.Config{Project: config.ProjectConfig{AgentsMdMaxBytes: 2048}}, logger)
	root := t.TempDir()
	big := "## Keep\n\nshort\n\n## Huge\n\n" + strings.Repeat("x", 3000) + "\n"
	writeAgentsMd(t, root, big)

	for i := 0; i < 3; i++ {
		if got := s.primeAgentsMd("", root); !strings.Contains(got, agentsmd.InjectionTruncatedMarker) {
			t.Fatalf("kick %d lost its in-prompt warning", i)
		}
	}
	if n := strings.Count(logBuf.String(), agentsMdCapWarning); n != 1 {
		t.Fatalf("warning logged %d times for unchanged content, want 1", n)
	}
	writeAgentsMd(t, root, big+"\n## Huge too\n\n"+strings.Repeat("y", 3000)+"\n")
	s.primeAgentsMd("", root)
	s.primeAgentsMd("", root)
	if n := strings.Count(logBuf.String(), agentsMdCapWarning); n != 2 {
		t.Fatalf("warning logged %d times after one content change, want 2", n)
	}
}

func TestAgentsMdStatusRepoAndMaxBytesWithoutConfig(t *testing.T) {
	s := &Scheduler{}
	if got := s.agentsMdStatusRepo("  "); got != "" {
		t.Errorf("blank repo -> %q", got)
	}
	if got := s.agentsMdStatusRepo("acme/x"); got != "acme/x" {
		t.Errorf("qualified repo -> %q", got)
	}
	if got := s.agentsMdMaxBytes("x"); got != 0 {
		t.Errorf("nil config cap = %d, want 0", got)
	}
}
