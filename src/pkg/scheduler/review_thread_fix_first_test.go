package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const reviewThreadFixture = `{"generated_at":"2026-09-17T00:00:00Z","enabled":true,"total_threads":3,"prs":[
  {"repo":"test-org/console","number":501,"title":"fix: nil deref","head_ref":"hive/fix-501","agent":"scanner",
   "threads":[
     {"thread_id":"PRRT_a","path":"src/x.go","line":42,"author":"chatgpt-codex-connector[bot]","body":"x may be nil here\nconsider a guard","hive_replies":0},
     {"thread_id":"PRRT_b","path":"src/y.go","line":0,"author":"Copilot","body":"unused import","hive_replies":0}
   ]},
  {"repo":"test-org/console","number":502,"title":"quality PR","head_ref":"hive/q-502","agent":"quality",
   "threads":[{"thread_id":"PRRT_c","path":"a.go","line":1,"author":"Copilot","body":"nit","hive_replies":0}]},
  {"repo":"test-org/console","number":503,"title":"unattributed","head_ref":"hive/u-503",
   "threads":[{"thread_id":"PRRT_d","path":"b.go","line":2,"author":"Copilot","body":"nit","hive_replies":0}]},
  {"repo":"test-org/console","number":504,"title":"escalated","head_ref":"hive/e-504","agent":"scanner","escalated":true,
   "threads":[{"thread_id":"PRRT_e","path":"c.go","line":3,"author":"Copilot","body":"nit","hive_replies":0}]},
  {"repo":"test-org/console","number":505,"title":"all resolved","head_ref":"hive/r-505","agent":"scanner","threads":[]}
]}`

// The review-thread fix-before-new section routes each PR to the agent that
// opened it, with every thread's id, location, and finding excerpt, plus the
// in-thread reply and resolve instructions. Escalated PRs and PRs with no
// remaining threads never appear (hivecommons/hive#7360).
func TestFormatReviewThreadFixData_PerAgentRouting(t *testing.T) {
	scanner := formatReviewThreadFixData([]byte(reviewThreadFixture), "scanner", true)
	for _, want := range []string{
		"FIX-BEFORE-NEW",
		"unresolved review-bot threads (2 PRs, 3 threads)",
		"#501 test-org/console — fix: nil deref",
		"head_ref: hive/fix-501",
		"thread PRRT_a (src/x.go:42, by chatgpt-codex-connector[bot])",
		"x may be nil here",
		"thread PRRT_b (src/y.go, by Copilot)", // line 0 → no :line suffix
		"#503 test-org/console",                // unattributed defaults to scanner
		"--comment --thread <thread_id>",
		"--resolve-thread <thread_id>",
		"Never reply twice",
		"a human's thread is never yours to close",
	} {
		if !strings.Contains(scanner, want) {
			t.Errorf("scanner section missing %q:\n%s", want, scanner)
		}
	}
	for _, reject := range []string{"#502 ", "PRRT_c", "#504 ", "PRRT_e", "#505 "} {
		if strings.Contains(scanner, reject) {
			t.Errorf("scanner section must not contain %q (other agent / escalated / nothing left):\n%s", reject, scanner)
		}
	}

	quality := formatReviewThreadFixData([]byte(reviewThreadFixture), "quality", true)
	if !strings.Contains(quality, "#502 ") || strings.Contains(quality, "#501 ") {
		t.Errorf("quality section wrong:\n%s", quality)
	}

	if got := formatReviewThreadFixData([]byte(reviewThreadFixture), "outreach", true); got != "" {
		t.Errorf("agent with no bot threads must get an empty section, got:\n%s", got)
	}
	if got := formatReviewThreadFixData([]byte("not json"), "scanner", true); got != "" {
		t.Errorf("malformed file must yield empty section, got:\n%s", got)
	}
	disabled := strings.Replace(reviewThreadFixture, `"enabled":true`, `"enabled":false`, 1)
	if got := formatReviewThreadFixData([]byte(disabled), "scanner", true); got != "" {
		t.Errorf("a report from a hive with review_bots off must yield no section, got:\n%s", got)
	}
}

// resolve_after_fix: false swaps the resolve step for an explicit "do not
// resolve" instruction, so the agent never writes a resolve_thread request
// the watcher would have to deny.
func TestFormatReviewThreadFixData_ResolveAfterFixOff(t *testing.T) {
	out := formatReviewThreadFixData([]byte(reviewThreadFixture), "scanner", false)
	if strings.Contains(out, "--resolve-thread") {
		t.Errorf("resolve step must be absent when resolve_after_fix is false:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT resolve the thread") {
		t.Errorf("expected explicit do-not-resolve instruction:\n%s", out)
	}
}

// Long findings are truncated and a large PR backlog is capped with a
// summary line, using the same bounds as the red-CI block.
func TestFormatReviewThreadFixData_Bounds(t *testing.T) {
	long := strings.Repeat("y", 2*redPRFixExcerptRunes)
	var rows []string
	for i := 0; i < redPRFixMaxDetailed+2; i++ {
		rows = append(rows, `{"repo":"o/r","number":`+string(rune('1'+i))+`00,"head_ref":"h","agent":"scanner","threads":[{"thread_id":"PRRT_`+string(rune('a'+i))+`","path":"p.go","line":1,"author":"Copilot","body":"`+long+`"}]}`)
	}
	data := `{"enabled":true,"prs":[` + strings.Join(rows, ",") + `]}`
	out := formatReviewThreadFixData([]byte(data), "scanner", true)
	if !strings.Contains(out, "… and 2 more PRs") {
		t.Errorf("expected cap summary line, got:\n%s", out)
	}
	if strings.Contains(out, long) {
		t.Error("finding excerpt must be truncated")
	}
}

// addReviewThreadFixFirst injects the section right below the kick header
// for PR-capable agents, stays out of advisory kicks, and coexists with the
// red-CI block at the same seam.
func TestAddReviewThreadFixFirst_Injection(t *testing.T) {
	dir := t.TempDir()
	orig := reviewThreadsPath
	reviewThreadsPath = filepath.Join(dir, "review-threads.json")
	t.Cleanup(func() { reviewThreadsPath = orig })
	if err := os.WriteFile(reviewThreadsPath, []byte(reviewThreadFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	origCI := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = origCI })
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

	msg := s.addReviewThreadFixFirst("scanner", "[agent:scanner]\nWORK LIST\n")
	if !strings.HasPrefix(msg, "[agent:scanner]\n\n## 💬 FIX-BEFORE-NEW") {
		t.Errorf("section must land directly below the kick header:\n%s", msg[:min(len(msg), 120)])
	}
	if got := s.addReviewThreadFixFirst("advisor", "[agent:advisor]\nADVISORY\n"); strings.Contains(got, "review-bot threads") {
		t.Error("advisory agent must not receive the section")
	}
	if got := s.addReviewThreadFixFirst("outreach", "[agent:outreach]\nWORK\n"); strings.Contains(got, "review-bot threads") {
		t.Error("agent with no bot threads must not receive the section")
	}
	if got := s.addReviewThreadFixFirst("scanner", ""); got != "" {
		t.Error("empty (fail-closed) message must stay empty")
	}

	// Both stuck-PR blocks ride the same seam: red CI first, then threads.
	both := s.addReviewThreadFixFirst("scanner", s.addRedPRFixFirst("scanner", "[agent:scanner]\nWORK LIST\n"))
	red := strings.Index(both, "🔴 FIX-BEFORE-NEW")
	threads := strings.Index(both, "💬 FIX-BEFORE-NEW")
	work := strings.Index(both, "WORK LIST")
	if red < 0 || threads < 0 || !(threads < red && red < work) {
		t.Errorf("expected thread block, then red-CI block, then the work list (thread=%d red=%d work=%d):\n%s", threads, red, work, both)
	}
}
