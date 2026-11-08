package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/timeline"
)

// residentOnlyKickText is text that belongs to a resident agent's kick and
// must never reach a contributor's role guidance: the work-list headings
// (kickmessage.go, lane_messages.go and the shipped templates), the merge and
// dispatch instructions, the App-token contract, the red-main notice, the
// closing instruction, and the golden queue's own items.
var residentOnlyKickText = []string{
	"MERGE-ELIGIBLE",
	"CI-FAILING",
	"ACTIONABLE ISSUES",
	"ACTIONABLE PRs",
	"hive-merge",
	"Begin now",
	"RED MAIN",
	"## Merge Sweep",
	"## Dispatching Fixes",
	"## Priority Order",
	"run_in_background",
	"## Project Authentication",
	"AUTHORIZED REPOS",
	"Workflow files are",
	"golden-org/console#11",
	"#41 golden-org/console",
	"#51 golden-org/console",
}

func guidanceScheduler(t *testing.T, scannerTemplate string) *Scheduler {
	t.Helper()
	s := kickGoldenScheduler(t)
	checkouts := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkouts, "console"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkouts, "console", "AGENTS.md"),
		[]byte("# Console agent notes\n\nREPO-OWN-INSTRUCTION: run make lint before pushing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Project.CheckoutsDir = checkouts
	// The guidance is checked against the SHIPPED templates, unlike the
	// resident goldens' stand-in.
	if scannerTemplate == "" {
		scannerTemplate = "scanner-automerge.md"
	}
	for agent, template := range map[string]string{"scanner": scannerTemplate, "quality": "quality-full.md"} {
		a := s.cfg.Agents[agent]
		a.KickTemplate = template
		s.cfg.Agents[agent] = a
	}
	s.SetLastActionable(kickGoldenActionable())
	return s
}

// A contributor taking on a role gets the role's guidance — policy text, house
// style, lane rules and the repository's own AGENTS.md — and none of the
// resident agent's work, merge or dispatch instructions.
func TestBuildAgentGuidanceKeepsPolicyDropsResidentWork(t *testing.T) {
	cases := []struct {
		agent, template string
		keep            []string
	}{
		{agent: "scanner", template: "scanner-automerge.md", keep: []string{
			"## Rules", "## Shared CI Baseline Triage (MANDATORY)",
			"## After You Push: Do Not Wait for CI (MANDATORY)", "## Publishable Content Boundary",
		}},
		{agent: "scanner", template: "scanner-holdgated.md", keep: []string{
			"## Rules", "## Opening Hold-Gated PRs", "## After You Push: Do Not Wait for CI (MANDATORY)",
		}},
		{agent: "quality", keep: []string{"## Rules", "## After You Push: Do Not Wait for CI (MANDATORY)"}},
	}
	for _, tc := range cases {
		t.Run(tc.agent+"/"+tc.template, func(t *testing.T) {
			s := guidanceScheduler(t, tc.template)
			g := s.BuildAgentGuidance(tc.agent)
			keep := append([]string{
				"[agent:" + tc.agent + "]",
				"HOUSE STYLE (source: built-in)",
				governorWorkSourceRule,
				"REPO-OWN-INSTRUCTION: run make lint before pushing.",
			}, tc.keep...)
			for _, want := range keep {
				if !strings.Contains(g, want) {
					t.Errorf("guidance is missing %q", want)
				}
			}
			for _, banned := range residentOnlyKickText {
				if strings.Contains(g, banned) {
					t.Errorf("guidance carries resident-only text %q", banned)
				}
			}
			if t.Failed() {
				t.Logf("guidance:\n%s", g)
			}
		})
	}
}

// Building the guidance reads no queue: no classification sweep (whose
// recordClassified writes KindClassified journeys right after ClassifyAll),
// no triage, and no kick summary recorded for the resident agent. The manual
// kick on the same scheduler does all of that, which proves the probe works.
func TestBuildAgentGuidanceMakesNoClassificationSweep(t *testing.T) {
	s := guidanceScheduler(t, "")
	store := timeline.NewStore()
	s.SetLifecycleRecorder(store)

	if g := s.BuildAgentGuidance("scanner"); g == "" {
		t.Fatal("guidance is empty")
	}
	if n := len(store.Journeys(0)); n != 0 {
		t.Fatalf("guidance recorded %d classified journeys; it must not classify the queue", n)
	}
	if _, ok := s.kickSummaries.Load("scanner"); ok {
		t.Fatal("guidance recorded a kick summary for the resident scanner")
	}

	s.BuildAgentMessageFromLastActionable("scanner")
	if n := len(store.Journeys(0)); n == 0 {
		t.Fatal("control: the manual kick should classify the cached queue")
	}
}

func TestGuidanceTemplateDropsSessionSectionsAndLists(t *testing.T) {
	in := strings.Join([]string{
		"# Role",
		"intro",
		"## Rules",
		"- rule one",
		"## Merge Sweep",
		"sweep text",
		"${MERGE_ELIGIBLE}",
		"### Merge Process",
		"merge step",
		"## Gate",
		"```bash",
		"## Workflow inside a fence is not a section",
		"```",
		"gate text",
		"## Workflow",
		"1. read the list",
		"${KNOWLEDGE}",
		"## Work Lists",
		"ACTIONABLE ISSUES:",
		"${ISSUE_LIST}",
		"",
		"⛔ never discover work",
		"## CI Repair — Fix Failing PRs",
		"repair text",
		"## Workflow files in reach",
		"kept: not the Workflow section",
	}, "\n")
	got := guidanceTemplate(in)
	for _, want := range []string{"# Role", "intro", "- rule one", "## Gate", "## Workflow inside a fence is not a section", "gate text", "${KNOWLEDGE}", "## Work Lists", "⛔ never discover work", "## Workflow files in reach", "kept: not the Workflow section"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidanceTemplate dropped %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"## Merge Sweep", "sweep text", "${MERGE_ELIGIBLE}", "merge step", "1. read the list", "ACTIONABLE ISSUES:", "${ISSUE_LIST}", "repair text", "## CI Repair"} {
		if strings.Contains(got, banned) {
			t.Errorf("guidanceTemplate kept %q:\n%s", banned, got)
		}
	}
}
