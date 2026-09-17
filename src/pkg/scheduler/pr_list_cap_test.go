package scheduler

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The PR sections of a kick were the only unbounded term in the prompt: issues
// were capped but PRs were emitted in full, so a spoke with 302 open PRs
// produced a 69.5 KiB kick against a ~22 KiB documented worst case. These tests
// pin the cap at every site that renders a PR list, pin the "... N more" marker
// that keeps the agent aware the list is partial, and pin that both caps are
// operator-configurable. See hivecommons/hive#7368.

// newCapScheduler builds a scheduler with explicit kick-list caps. Zero means
// "unset", which must fall back to the documented default.
func newCapScheduler(issueCap, prCap int) *Scheduler {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:              "test-org",
			Repos:            []string{"test-org/console"},
			MaxIssuesPerKick: issueCap,
			MaxPRsPerKick:    prCap,
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func capTestPRs(n int) []github.PullRequest {
	prs := make([]github.PullRequest, 0, n)
	for i := 0; i < n; i++ {
		prs = append(prs, github.PullRequest{
			Repo:   "acme/widget",
			Number: i + 1,
			Title:  fmt.Sprintf("fix: thing number %d", i+1),
			Author: "someone",
		})
	}
	return prs
}

func capTestIssues(n int) []github.Issue {
	issues := make([]github.Issue, 0, n)
	for i := 0; i < n; i++ {
		issues = append(issues, github.Issue{
			Repo:   "acme/widget",
			Number: i + 1,
			Title:  fmt.Sprintf("bug number %d", i+1),
		})
	}
	return issues
}

// countCapTestRefLines counts rendered "acme/widget#N" entries, ignoring the
// "N more" marker line and any surrounding prose.
func countCapTestRefLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "acme/widget#") && !strings.Contains(line, "more") {
			n++
		}
	}
	return n
}

func capTestActionable(n int) *github.ActionableResult {
	return &github.ActionableResult{
		PRs: github.PRResult{Count: n, Items: capTestPRs(n)},
	}
}

// ── The cap itself ──────────────────────────────────────────────────────────

func TestFormatPRList_CapsAtDefault(t *testing.T) {
	s := newCapScheduler(0, 0)

	result := s.formatPRList(capTestActionable(302)) // the backlog measured on the spoke

	if got := countCapTestRefLines(result); got != config.DefaultMaxPRsPerKick {
		t.Errorf("rendered %d PR lines, want %d", got, config.DefaultMaxPRsPerKick)
	}
	if strings.Contains(result, "acme/widget#301") {
		t.Error("a PR beyond the cap was rendered")
	}
}

func TestFormatPRList_AnnouncesOmittedPRs(t *testing.T) {
	s := newCapScheduler(0, 0)

	result := s.formatPRList(capTestActionable(config.DefaultMaxPRsPerKick + 7))

	want := fmt.Sprintf("... 7 more open PRs not shown (list capped at %d)", config.DefaultMaxPRsPerKick)
	if !strings.Contains(result, want) {
		t.Errorf("missing omission marker %q in:\n%s", want, result)
	}
}

// A list that fits must not gain a marker — otherwise every small hive is told
// its complete list is partial.
func TestFormatPRList_NoMarkerWhenUnderCap(t *testing.T) {
	s := newCapScheduler(0, 0)

	result := s.formatPRList(capTestActionable(3))

	if strings.Contains(result, "more open PRs not shown") {
		t.Errorf("unexpected omission marker for a 3-PR list:\n%s", result)
	}
	if got := countCapTestRefLines(result); got != 3 {
		t.Errorf("rendered %d PR lines, want 3", got)
	}
}

// Exactly-at-cap is the off-by-one boundary: a list of exactly the cap is
// complete, not truncated.
func TestFormatPRList_ExactlyAtCapHasNoMarker(t *testing.T) {
	s := newCapScheduler(0, 0)

	result := s.formatPRList(capTestActionable(config.DefaultMaxPRsPerKick))

	if strings.Contains(result, "more open PRs not shown") {
		t.Errorf("unexpected omission marker at exactly the cap:\n%s", result)
	}
	if got := countCapTestRefLines(result); got != config.DefaultMaxPRsPerKick {
		t.Errorf("rendered %d PR lines, want %d", got, config.DefaultMaxPRsPerKick)
	}
}

// The legacy hardcoded scanner kick is still live for hives without a
// scanner.md template, so it needs the same bound.
func TestBuildScannerMessage_CapsPRList(t *testing.T) {
	s := newCapScheduler(0, 0)

	result := s.buildScannerMessage(nil, capTestActionable(302))

	if got := countCapTestRefLines(result); got != config.DefaultMaxPRsPerKick {
		t.Errorf("rendered %d PR lines, want %d", got, config.DefaultMaxPRsPerKick)
	}
	if !strings.Contains(result, "... 272 more open PRs not shown") {
		t.Errorf("missing omission marker in:\n%s", result)
	}
	// The true total must survive so the agent is not misled about backlog size.
	if !strings.Contains(result, "ACTIONABLE PRs (302):") {
		t.Error("header lost the true PR count")
	}
}

func TestBuildScannerMessage_CapsStaleDrafts(t *testing.T) {
	s := newCapScheduler(0, 0)
	actionable := &github.ActionableResult{
		PRs: github.PRResult{StaleDrafts: capTestPRs(config.DefaultMaxPRsPerKick + 5)},
	}

	result := s.buildScannerMessage(nil, actionable)

	if got := countCapTestRefLines(result); got != config.DefaultMaxPRsPerKick {
		t.Errorf("rendered %d stale draft lines, want %d", got, config.DefaultMaxPRsPerKick)
	}
	if !strings.Contains(result, "... 5 more stale draft PRs not shown") {
		t.Errorf("missing stale-draft omission marker in:\n%s", result)
	}
}

func TestBuildScannerMessage_CapsIssueList(t *testing.T) {
	s := newCapScheduler(5, 0)

	result := s.buildScannerMessage(capTestIssues(40), &github.ActionableResult{})

	if got := countCapTestRefLines(result); got != 5 {
		t.Errorf("rendered %d issue lines, want 5", got)
	}
}

// The regression stated in the terms it was reported in: the rendered PR
// section must stay inside the budget that pkg/dashboard/prompt_history.go
// sizes prompt retention against, instead of growing with the backlog.
func TestFormatPRList_StaysWithinDocumentedSizeBudget(t *testing.T) {
	s := newCapScheduler(0, 0)

	size := len(s.formatPRList(capTestActionable(302)))

	const budgetBytes = 3584 // 3.5 KiB, per prompt_history.go
	if size > budgetBytes {
		t.Errorf("PR section is %d B, over the documented %d B budget", size, budgetBytes)
	}
}

// ── Operator configurability ────────────────────────────────────────────────

func TestPRListCap_HonoursOperatorSetting(t *testing.T) {
	s := newCapScheduler(0, 5)

	result := s.formatPRList(capTestActionable(50))

	if got := countCapTestRefLines(result); got != 5 {
		t.Errorf("rendered %d PR lines, want the configured 5", got)
	}
	if !strings.Contains(result, "... 45 more open PRs not shown (list capped at 5)") {
		t.Errorf("marker did not report the configured cap:\n%s", result)
	}
}

func TestIssueListCap_HonoursOperatorSetting(t *testing.T) {
	s := newCapScheduler(7, 0)

	if got := s.issueListCap(); got != 7 {
		t.Errorf("issueListCap() = %d, want the configured 7", got)
	}
}

// Zero must mean "use the default", never "unlimited" — an uncapped list is
// the bug these settings exist to prevent.
func TestKickListCaps_ZeroMeansDefaultNotUnlimited(t *testing.T) {
	s := newCapScheduler(0, 0)

	if got := s.issueListCap(); got != config.DefaultMaxIssuesPerKick {
		t.Errorf("unset issue cap = %d, want default %d", got, config.DefaultMaxIssuesPerKick)
	}
	if got := s.prListCap(); got != config.DefaultMaxPRsPerKick {
		t.Errorf("unset PR cap = %d, want default %d", got, config.DefaultMaxPRsPerKick)
	}
}

func TestKickListCaps_ClampToCeiling(t *testing.T) {
	s := newCapScheduler(100000, 100000)

	if got := s.issueListCap(); got != config.MaxKickListCap {
		t.Errorf("issue cap = %d, want clamp to %d", got, config.MaxKickListCap)
	}
	if got := s.prListCap(); got != config.MaxKickListCap {
		t.Errorf("PR cap = %d, want clamp to %d", got, config.MaxKickListCap)
	}
}

// A negative value in a hand-edited hive.yaml must not slice to a panic or
// render an empty work list; it is treated as unset.
func TestKickListCaps_NegativeFallsBackToDefault(t *testing.T) {
	s := newCapScheduler(-1, -20)

	if got := s.issueListCap(); got != config.DefaultMaxIssuesPerKick {
		t.Errorf("negative issue cap = %d, want default %d", got, config.DefaultMaxIssuesPerKick)
	}
	if got := s.prListCap(); got != config.DefaultMaxPRsPerKick {
		t.Errorf("negative PR cap = %d, want default %d", got, config.DefaultMaxPRsPerKick)
	}
}

// A nil config must not panic — the accessors are called on every kick render.
func TestKickListCaps_NilConfigUsesDefaults(t *testing.T) {
	var s *Scheduler

	if got := s.issueListCap(); got != config.DefaultMaxIssuesPerKick {
		t.Errorf("nil scheduler issue cap = %d, want %d", got, config.DefaultMaxIssuesPerKick)
	}
	if got := s.prListCap(); got != config.DefaultMaxPRsPerKick {
		t.Errorf("nil scheduler PR cap = %d, want %d", got, config.DefaultMaxPRsPerKick)
	}
}
