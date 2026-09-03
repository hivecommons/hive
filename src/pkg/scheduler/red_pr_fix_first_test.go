package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const redPRFixture = `{"generated_at":"2026-08-26T00:00:00Z","ci_failing":[
  {"number":22815,"repo":"test-org/console","title":"[Refactor] split tests","agent":"scanner",
   "failing_checks":["build-gate","Test (chromium, shard 1)"],
   "excerpt":"src/hooks/__tests__/a.test.tsx:1:8 — 'React' is defined but never used."},
  {"number":7,"repo":"test-org/console","title":"quality PR","agent":"quality","failing_checks":["go test"]},
  {"number":8,"repo":"test-org/console","title":"unattributed PR","failing_checks":["build-gate"]},
  {"number":9,"repo":"test-org/console","title":"escalated PR","agent":"scanner","escalated":true}
]}`

// The fix-before-new section routes each red PR to its authoring agent, with
// the failing checks and CI evidence inline, and the push-to-same-branch
// instruction. Escalated (needs-human) PRs never appear.
func TestFormatRedPRFixData_PerAgentRouting(t *testing.T) {
	scanner := formatRedPRFixData([]byte(redPRFixture), "scanner")
	for _, want := range []string{
		"FIX-BEFORE-NEW",
		"#22815 test-org/console",
		"build-gate, Test (chromium, shard 1)",
		"'React' is defined but never used",
		"#8 test-org/console", // unattributed defaults to scanner
		"gh pr checkout",
		"Do NOT open a replacement PR",
	} {
		if !strings.Contains(scanner, want) {
			t.Errorf("scanner section missing %q:\n%s", want, scanner)
		}
	}
	if strings.Contains(scanner, "#7 ") {
		t.Error("quality's PR must not appear in scanner's section")
	}
	if strings.Contains(scanner, "#9 ") {
		t.Error("escalated PR must never appear in an agent's fix list")
	}

	quality := formatRedPRFixData([]byte(redPRFixture), "quality")
	if !strings.Contains(quality, "#7 ") || strings.Contains(quality, "#22815") {
		t.Errorf("quality section wrong:\n%s", quality)
	}

	if got := formatRedPRFixData([]byte(redPRFixture), "outreach"); got != "" {
		t.Errorf("agent with no red PRs must get an empty section, got:\n%s", got)
	}
	if got := formatRedPRFixData([]byte("not json"), "scanner"); got != "" {
		t.Errorf("malformed file must yield empty section, got:\n%s", got)
	}
}

// Long evidence excerpts are truncated and a large backlog is capped with a
// summary line instead of flooding the kick.
func TestFormatRedPRFixData_Bounds(t *testing.T) {
	long := strings.Repeat("x", 2*redPRFixExcerptRunes)
	var rows []string
	for i := 0; i < redPRFixMaxDetailed+3; i++ {
		rows = append(rows, `{"number":`+string(rune('1'+i))+`00,"repo":"o/r","title":"t","agent":"scanner","excerpt":"`+long+`"}`)
	}
	data := `{"ci_failing":[` + strings.Join(rows, ",") + `]}`
	out := formatRedPRFixData([]byte(data), "scanner")
	if !strings.Contains(out, "… and 3 more") {
		t.Errorf("expected cap summary line, got:\n%s", out)
	}
	if strings.Contains(out, long) {
		t.Error("excerpt must be truncated")
	}
}

// addRedPRFixFirst injects the section right below the kick header for
// PR-capable agents on every resolution path, and stays out of advisory
// agents' kicks entirely.
func TestAddRedPRFixFirst_Injection(t *testing.T) {
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if err := os.WriteFile(ciFailingPath, []byte(redPRFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Agents: map[string]config.AgentConfig{
			"scanner":  {Mode: "ISSUES_AND_PRS"},
			"advisor":  {Mode: "ADVISORY"},
			"outreach": {Mode: "ISSUES_AND_PRS"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := New(cfg, logger)

	msg := s.addRedPRFixFirst("scanner", "[agent:scanner]\nWORK LIST\n")
	if !strings.Contains(msg, "FIX-BEFORE-NEW") {
		t.Fatalf("scanner kick missing fix-before-new section:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "[agent:scanner]\n\n## 🔴 FIX-BEFORE-NEW") {
		t.Errorf("section must land directly below the kick header:\n%s", msg[:min(len(msg), 120)])
	}

	if got := s.addRedPRFixFirst("advisor", "[agent:advisor]\nADVISORY\n"); strings.Contains(got, "FIX-BEFORE-NEW") {
		t.Error("advisory agent must not receive the section")
	}
	if got := s.addRedPRFixFirst("outreach", "[agent:outreach]\nWORK\n"); strings.Contains(got, "FIX-BEFORE-NEW") {
		t.Error("agent with no red PRs must not receive the section")
	}
	if got := s.addRedPRFixFirst("scanner", ""); got != "" {
		t.Error("empty (fail-closed) message must stay empty")
	}
}
