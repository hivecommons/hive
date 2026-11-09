package scheduler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
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

// withTaskRepoAndKnowledge gives a guidance scheduler a second checkout
// (docs, not the primary console) with its own AGENTS.md, a knowledge vault
// holding one fact about the assigned task, and a writing guide.
func withTaskRepoAndKnowledge(t *testing.T, s *Scheduler) {
	t.Helper()
	docs := filepath.Join(s.cfg.Project.CheckoutsDir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "AGENTS.md"),
		[]byte("# Docs agent notes\n\nTASK-REPO-INSTRUCTION: run make docs-check before pushing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "sidebar.md"), []byte("---\ntitle: Sidebar anchor links drift\ntype: gotcha\nlayer: project\n---\n"+
		"TASK-KNOWLEDGE: sidebar anchors are generated from headings; rename both together.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := knowledge.NewFileStore(vault, "vault", logger)
	if err != nil {
		t.Fatal(err)
	}
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	primer.AddFileStore("vault", store, knowledge.LayerProject)
	s.SetPrimer(primer)
	s.cfg.Project.WritingGuide = "WRITING-GUIDE-MARKER: lead with the user-visible change."
}

var guidanceTask = github.Issue{Repo: "golden-org/docs", Number: 1614, Title: "Sidebar anchor links broken", Labels: []string{"sidebar"}}

// A contributor's guidance for a task in docs carries docs' AGENTS.md (not the
// primary console's), the "# Relevant Knowledge" primed from that one issue,
// and the writing guide — and still none of the resident agent's work.
// Priming it records nothing for the resident agent: no kick summary, no
// primed slugs, no classified journeys.
func TestBuildAgentGuidanceForTaskCarriesTaskRepoAndIssueKnowledge(t *testing.T) {
	for _, agent := range []string{"scanner", "quality"} {
		t.Run(agent, func(t *testing.T) {
			s := guidanceScheduler(t, "")
			withTaskRepoAndKnowledge(t, s)
			store := timeline.NewStore()
			s.SetLifecycleRecorder(store)

			g := s.BuildAgentGuidanceForTask(agent, guidanceTask)
			for _, want := range []string{
				"[agent:" + agent + "]",
				"HOUSE STYLE (source: built-in)",
				"# Relevant Knowledge",
				"TASK-KNOWLEDGE: sidebar anchors",
				"TASK-REPO-INSTRUCTION: run make docs-check before pushing.",
				"WRITING GUIDE (set by this hive's owner",
				"WRITING-GUIDE-MARKER",
			} {
				if !strings.Contains(g, want) {
					t.Errorf("guidance is missing %q", want)
				}
			}
			if strings.Contains(g, "REPO-OWN-INSTRUCTION") {
				t.Errorf("guidance carries the primary repo's AGENTS.md, not the task repo's")
			}
			for _, banned := range residentOnlyKickText {
				if strings.Contains(g, banned) {
					t.Errorf("guidance carries resident-only text %q", banned)
				}
			}
			if _, ok := s.KickSummary(agent); ok {
				t.Errorf("guidance recorded a kick summary for the resident %s", agent)
			}
			if slugs := s.KickPrimedSlugs(agent); slugs != nil {
				t.Errorf("guidance recorded primed slugs %v for the resident %s", slugs, agent)
			}
			if n := len(store.Journeys(0)); n != 0 {
				t.Errorf("guidance recorded %d classified journeys", n)
			}
			if t.Failed() {
				t.Logf("guidance:\n%s", g)
			}
		})
	}
}

// A task that names no repository falls back to the primary repo's AGENTS.md,
// as before; one with no title or labels primes no knowledge.
func TestBuildAgentGuidanceForTaskWithoutRepoFallsBackToPrimary(t *testing.T) {
	s := guidanceScheduler(t, "")
	withTaskRepoAndKnowledge(t, s)
	g := s.BuildAgentGuidanceForTask("scanner", github.Issue{})
	if !strings.Contains(g, "REPO-OWN-INSTRUCTION: run make lint before pushing.") {
		t.Errorf("guidance without a task repo lost the primary repo's AGENTS.md")
	}
	if strings.Contains(g, "TASK-REPO-INSTRUCTION") || strings.Contains(g, "# Relevant Knowledge") {
		t.Errorf("guidance without a task carries task-repo instructions or knowledge:\n%s", g)
	}
	if !strings.Contains(g, "WRITING-GUIDE-MARKER") {
		t.Errorf("guidance lost the writing guide")
	}
}

// taskKnowledgeSourceLines returns the decoded "role guidance knowledge: …"
// records a scheduler wrote to buf.
func taskKnowledgeSourceLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "role guidance knowledge: ") {
			out = append(out, rec)
		}
	}
	return out
}

// A role guidance render says where its "# Relevant Knowledge" came from: the
// assigned issue (with the query it built and how many entries matched), or
// nothing, and why. The shared "knowledge: injected" line names only the role,
// so without this an operator cannot tell a task-primed section from a
// resident kick's, or see the query that picked off-topic entries.
func TestBuildAgentGuidanceForTaskLogsKnowledgeSource(t *testing.T) {
	tests := []struct {
		name       string
		task       github.Issue
		wantMsg    string
		wantSource string
		wantHits   float64
	}{
		{
			name:       "primed from the assigned issue",
			task:       guidanceTask,
			wantMsg:    `role guidance knowledge: primed from issue golden-org/docs#1614 (query="sidebar anchor links broken", hits=1)`,
			wantSource: "issue",
			wantHits:   1,
		},
		{
			name:       "an issue whose search matches nothing primes nothing, with no fallback",
			task:       github.Issue{Repo: "golden-org/docs", Number: 1615, Title: "Kernel panic resuming laptop"},
			wantMsg:    `role guidance knowledge: nothing primed, no entry matched issue golden-org/docs#1615 (query="kernel panic resuming laptop"); there is no fallback to general entries`,
			wantSource: "none",
		},
		{
			name:       "no assigned issue",
			task:       github.Issue{},
			wantMsg:    "role guidance knowledge: nothing primed, (no assigned issue) has no title or labels to search on",
			wantSource: "none",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := guidanceScheduler(t, "")
			withTaskRepoAndKnowledge(t, s)
			var buf bytes.Buffer
			s.logger = slog.New(slog.NewJSONHandler(&buf, nil))

			g := s.BuildAgentGuidanceForTask("scanner", tc.task)
			lines := taskKnowledgeSourceLines(t, &buf)
			if len(lines) != 1 {
				t.Fatalf("got %d role guidance knowledge lines, want 1:\n%s", len(lines), buf.String())
			}
			rec := lines[0]
			if rec["msg"] != tc.wantMsg {
				t.Errorf("msg = %q\nwant  %q", rec["msg"], tc.wantMsg)
			}
			if rec["source"] != tc.wantSource || rec["hits"] != tc.wantHits || rec["agent"] != "scanner" {
				t.Errorf("source=%v hits=%v agent=%v, want source=%s hits=%v agent=scanner", rec["source"], rec["hits"], rec["agent"], tc.wantSource, tc.wantHits)
			}
			if primed := strings.Contains(g, "# Relevant Knowledge"); primed != (tc.wantSource == "issue") {
				t.Errorf("guidance has Relevant Knowledge = %v, but the log says source=%s", primed, tc.wantSource)
			}
		})
	}
}

// A resident kick never writes the role guidance line.
func TestResidentKickWritesNoTaskKnowledgeSourceLine(t *testing.T) {
	s := guidanceScheduler(t, "")
	withTaskRepoAndKnowledge(t, s)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	_ = s.primeKnowledge("scanner", []github.Issue{guidanceTask})
	if lines := taskKnowledgeSourceLines(t, &buf); len(lines) != 0 {
		t.Errorf("resident priming wrote %d role guidance knowledge lines:\n%s", len(lines), buf.String())
	}
}
