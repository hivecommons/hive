package scheduler

import (
	"fmt"
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

func TestFormatRedPRFixData_ConflictRowsPromptRebaseAndReroute(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":9258,"repo":"hivecommons/hive","title":"v6 readiness","agent":"scanner",
	   "mergeable_state":"dirty","conflict":true,"rerouted_from":"strategist"},
	  {"number":9259,"repo":"hivecommons/hive","title":"quality conflict","agent":"quality",
	   "mergeable_state":"dirty","conflict":true}
	]}`
	out := formatRedPRFixData([]byte(data), "scanner")
	for _, want := range []string{
		"failing CI or merge conflicts (1)",
		"#9258 hivecommons/hive",
		"conflict: mergeable_state=dirty",
		"merge the base branch or rebase",
		"rerouted from paused/unavailable lane: strategist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("conflict fix block missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#9259 ") {
		t.Errorf("quality conflict must not appear in scanner block unless rerouted:\n%s", out)
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
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner":  {Mode: "ISSUES_AND_PRS"},
				"advisor":  {Mode: "ADVISORY"},
				"outreach": {Mode: "ISSUES_AND_PRS"},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := New(cfg, logger)

	msg := s.addRedPRFixFirst("scanner", "[agent:scanner]\nWORK LIST\n")
	if !strings.Contains(msg, "FIX-BEFORE-NEW") {
		t.Fatalf("scanner kick missing fix-before-new section:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "[agent:scanner]\nWORK LIST\n\n## 🔴 FIX-BEFORE-NEW") {
		t.Errorf("section must land in the dynamic tail after the body (#12125):\n%s", msg[:min(len(msg), 120)])
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

// A routed fixer rebase is only handed over by a kick that renders it:
// KickCarriesRebaseAttempt reads the kick text the fix-before-new builder
// wrote, so cmd/hive can start the attempt's deadline at that kick's
// delivery. It is true only for a detailed conflict row with the attempt the
// ledger is on — not for another attempt, another PR or repo whose number
// shares a prefix, a red row with no rebase line, a row past the detailed
// cap, a busy-branch row, or a kick with no fix-before-new section.
func TestKickCarriesRebaseAttempt(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":1382,"repo":"projectbluefin/utah","title":"adopted fork conflict","agent":"scanner","from_fork":true,"adopted":true,
	   "reachable_action":"push","head_repo":"mendezr/utah","conflict":true,"mergeable_state":"dirty","rebase_attempt":1,"rebase_attempts_max":2,"base_ref":"main"},
	  {"number":972,"repo":"projectbluefin/utah-packages","title":"second conflict","agent":"scanner","conflict":true,
	   "mergeable_state":"dirty","rebase_attempt":2,"rebase_attempts_max":2,"base_ref":"main"},
	  {"number":138,"repo":"projectbluefin/utah","title":"red, not conflicting","agent":"scanner","failing_checks":["build"]}
	]}`
	section := formatRedPRFixData([]byte(data), "scanner")
	kick := "[agent:scanner]\nWORK LIST\n  #1382 projectbluefin/utah — listed in the work list too\n" + "\n" + section +
		"\n## 💬 FIX-BEFORE-NEW — review threads\n\n  #138 projectbluefin/utah — t\n    rebase-resolve (attempt 1/2): not the red section\n"

	for _, tc := range []struct {
		name    string
		repo    string
		number  int
		attempt int
		want    bool
	}{
		{"adopted fork row, current attempt", "projectbluefin/utah", 1382, 1, true},
		{"repo spelled in another case", "ProjectBluefin/Utah", 1382, 1, true},
		{"second row, its own attempt", "projectbluefin/utah-packages", 972, 2, true},
		{"attempt the kick did not carry", "projectbluefin/utah", 1382, 2, false},
		{"same number, other repo", "projectbluefin/utah-packages", 1382, 1, false},
		{"repo that is a prefix of the listed one", "projectbluefin/utah", 972, 2, false},
		{"number that is a prefix of a listed one", "projectbluefin/utah", 138, 1, false},
		{"not listed at all", "projectbluefin/utah", 9999, 1, false},
		{"zero attempt", "projectbluefin/utah", 1382, 0, false},
	} {
		if got := KickCarriesRebaseAttempt(kick, tc.repo, tc.number, tc.attempt); got != tc.want {
			t.Errorf("%s: KickCarriesRebaseAttempt(%s#%d, attempt %d) = %v, want %v\n%s", tc.name, tc.repo, tc.number, tc.attempt, got, tc.want, kick)
		}
	}
	if KickCarriesRebaseAttempt("[agent:scanner]\n  #1382 projectbluefin/utah — t\n    rebase-resolve (attempt 1/2): x\n", "projectbluefin/utah", 1382, 1) {
		t.Error("a kick with no fix-before-new section must not count as carrying the rebase")
	}
	// An earlier header (an issue body in the work list quoting an old
	// kick) must not hide the real section, and a quoted one that is not at
	// the start of a line opens nothing.
	quoted := "[agent:scanner]\nWORK LIST\n" + redPRFixHeader + " — quoted (1)\n\n  #7 o/other — t\n    rebase-resolve (attempt 1/2): old\n\n" + section
	if !KickCarriesRebaseAttempt(quoted, "projectbluefin/utah", 1382, 1) {
		t.Errorf("an earlier quoted header hid the real section:\n%s", quoted)
	}
	if KickCarriesRebaseAttempt("> "+redPRFixHeader+" — quoted (1)\n\n  #1382 projectbluefin/utah — t\n    rebase-resolve (attempt 1/2): x\n", "projectbluefin/utah", 1382, 1) {
		t.Error("a header that does not start its line must not open the section")
	}

	// Past the detailed cap a row is only counted, never handed over.
	var rows []string
	for i := 0; i < redPRFixMaxDetailed+1; i++ {
		rows = append(rows, fmt.Sprintf(`{"number":%d,"repo":"o/r","title":"t","agent":"scanner","conflict":true,"mergeable_state":"dirty","rebase_attempt":1,"rebase_attempts_max":2}`, 100+i))
	}
	capped := formatRedPRFixData([]byte(`{"ci_failing":[`+strings.Join(rows, ",")+`]}`), "scanner")
	if !KickCarriesRebaseAttempt(capped, "o/r", 100, 1) {
		t.Errorf("first detailed row must count:\n%s", capped)
	}
	if last := 100 + redPRFixMaxDetailed; KickCarriesRebaseAttempt(capped, "o/r", last, 1) {
		t.Errorf("row #%d past the detailed cap must not count:\n%s", last, capped)
	}

	// A busy-branch row is listed "suggest, do not push": not handed over.
	busy := formatRedPRFixDataGuarded([]byte(data), "scanner", nil, func(row busyBranchRow) string {
		if row.Number == 1382 {
			return "another worker holds it"
		}
		return ""
	})
	if !strings.Contains(busy, "#1382 projectbluefin/utah") || KickCarriesRebaseAttempt(busy, "projectbluefin/utah", 1382, 1) {
		t.Errorf("busy row must be listed but not count as delivered:\n%s", busy)
	}
	if !KickCarriesRebaseAttempt(busy, "projectbluefin/utah-packages", 972, 2) {
		t.Errorf("the other row is still handed over:\n%s", busy)
	}
}
