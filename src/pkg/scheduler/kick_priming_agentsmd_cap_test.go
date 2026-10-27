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

// TestPrimeAgentsMd_CapIsVisibleInKickAndLog proves the #12129 contract end to
// end: with project.agents_md_max_bytes below the file's size, the kick text
// is bounded, whole sections are dropped at heading boundaries, the warning
// names them where the agent reads it, and the scheduler logs the drop.
func TestPrimeAgentsMd_CapIsVisibleInKickAndLog(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const capBytes = 2048
	s := New(&config.Config{Project: config.ProjectConfig{AgentsMdMaxBytes: capBytes}}, logger)

	root := t.TempDir()
	file := "## Branching\n\nAll PRs target `testing`.\n\n" +
		"## Giant style guide\n\n" + strings.Repeat("Indent with tabs. ", 300) + "\n\n" +
		"## Tests\n\nRun `make verify` before pushing.\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	got := s.primeAgentsMd(root)
	if got == "" {
		t.Fatal("expected an injection")
	}
	if len(got) > capBytes {
		t.Errorf("injection %d bytes exceeds cap %d", len(got), capBytes)
	}
	for _, want := range []string{"## Branching", "All PRs target `testing`.", "## Tests", "Run `make verify`"} {
		if !strings.Contains(got, want) {
			t.Errorf("surviving section text %q missing:\n%s", want, got)
		}
	}
	if strings.Contains(got, "## Giant style guide") || strings.Contains(got, "Indent with tabs.") {
		t.Errorf("oversized section must be dropped whole, not clipped:\n%s", got)
	}
	if !strings.Contains(got, agentsmd.InjectionTruncatedMarker+" 2048 bytes; 1 section(s) omitted") ||
		!strings.Contains(got, "\"Giant style guide\"") ||
		!strings.Contains(got, "read AGENTS.md in the repository root") {
		t.Errorf("warning line should name the cap, the section and the remedy:\n%s", got)
	}
	if strings.Index(got, agentsmd.InjectionTruncatedMarker) > strings.Index(got, "## Branching") {
		t.Errorf("warning must come before the surviving rules:\n%s", got)
	}
	log := logBuf.String()
	if !strings.Contains(log, "AGENTS.md exceeded the injection cap") || !strings.Contains(log, "cap_bytes=2048") ||
		!strings.Contains(log, "Giant style guide") {
		t.Errorf("scheduler should log the drop with cap and section names, got:\n%s", log)
	}
}

// TestPrimeAgentsMd_UnderCapHasNoWarning is the common case: a normal-sized
// file injects exactly as before, with no marker and no warning log.
func TestPrimeAgentsMd_UnderCapHasNoWarning(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(&config.Config{}, logger)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## Tests\n\nRun `make verify`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := s.primeAgentsMd(root)
	if !strings.Contains(got, "Run `make verify`.") {
		t.Fatalf("body missing:\n%s", got)
	}
	if strings.Contains(got, agentsmd.InjectionTruncatedMarker) {
		t.Errorf("no warning expected under the cap:\n%s", got)
	}
	if strings.Contains(logBuf.String(), "exceeded the injection cap") {
		t.Errorf("no warning log expected under the cap:\n%s", logBuf.String())
	}
}
