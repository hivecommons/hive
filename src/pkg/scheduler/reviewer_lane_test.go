package scheduler

// Tests for the reviewer lane (#5480): the escalated-PR adjudication kick.
//
// The lane's safety properties are all here: escalated-only rows, the
// one-pass-ever exclusion (reviewer-passed), the per-kick cap with oldest
// first ordering, the hard ACMM gate, and the adjudication contract itself
// (REPAIR / DE-ESCALATE / RECOMMEND-CLOSE with the close authority split at
// L6). Each is a one-line edit away from an agent that closes human queues,
// so each is pinned.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const reviewerFixture = `{"generated_at":"2026-09-01T00:00:00Z","ci_failing":[
  {"number":22815,"repo":"test-org/console","title":"red but NOT escalated","agent":"scanner",
   "failing_checks":["build-gate"],"excerpt":"still in the automated lane"},
  {"number":9,"repo":"test-org/console","title":"escalated split PR","agent":"scanner","escalated":true,
   "failing_checks":["build-gate","Test (chromium, shard 1)"],
   "excerpt":"ReferenceError: seedMission is not defined"},
  {"number":41,"repo":"test-org/console","title":"escalated, already adjudicated","agent":"quality","escalated":true,
   "labels":["needs-human","reviewer-passed"],"failing_checks":["go test"]},
  {"number":44,"repo":"test-org/console","title":"escalated, close already recommended","agent":"quality","escalated":true,
   "labels":["needs-human","reviewer-recommend-close"],"failing_checks":["go test"]},
  {"number":3,"repo":"test-org/console","title":"oldest escalated","escalated":true,
   "labels":["needs-human"],"failing_checks":["lint"]}
]}`

// Only rows with escalated=true appear; red-but-still-automated PRs stay in
// the fix lane, and rows carrying reviewer-passed are excluded forever.
func TestFormatReviewerWorkList_EscalatedOnlyAndReviewerPassedExcluded(t *testing.T) {
	out := formatReviewerWorkList([]byte(reviewerFixture))
	for _, want := range []string{
		"test-org/console#9",
		"test-org/console#3",
		"gh pr checkout 9 --repo test-org/console",
		"build-gate, Test (chromium, shard 1)",
		"ReferenceError: seedMission is not defined",
		"original author agent: scanner",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("work list missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#22815") {
		t.Error("non-escalated red PR must stay in the automated fix lane, not the reviewer's list")
	}
	if strings.Contains(out, "#41") {
		t.Error("reviewer-passed PR must be excluded: one reviewer pass per PR, ever")
	}
	// #5511 gap G4: below the close-authority level a RECOMMEND-CLOSE verdict
	// leaves the PR open, so without this exclusion the reviewer would
	// re-adjudicate (re-comment) it on every kick until an operator acts.
	if strings.Contains(out, "#44") {
		t.Error("reviewer-recommend-close PR must be excluded: the verdict was already delivered")
	}
	// Unattributed rows surface as scanner, the fleet's primary PR creator.
	if !strings.Contains(out, "original author agent: scanner") {
		t.Errorf("unattributed escalated row must default its author agent to scanner:\n%s", out)
	}
	// Oldest first: #3 precedes #9.
	if strings.Index(out, "#3 ") > strings.Index(out, "#9 ") {
		t.Errorf("work list must be oldest first (ascending PR number):\n%s", out)
	}

	if got := formatReviewerWorkList([]byte(`{"ci_failing":[{"number":1,"repo":"o/r","title":"red"}]}`)); got != "" {
		t.Errorf("no escalated rows must yield an empty list, got:\n%s", got)
	}
	if got := formatReviewerWorkList([]byte("not json")); got != "" {
		t.Errorf("malformed file must yield an empty list, got:\n%s", got)
	}
}

// The work list is structurally capped at reviewerMaxPRsPerKick rows, oldest
// first, with the remainder summarized rather than listed.
func TestFormatReviewerWorkList_CapOldestFirst(t *testing.T) {
	var rows []string
	for _, n := range []int{50, 10, 40, 20, 30} { // deliberately out of order
		rows = append(rows, fmt.Sprintf(`{"number":%d,"repo":"o/r","title":"t%d","agent":"scanner","escalated":true}`, n, n))
	}
	data := `{"ci_failing":[` + strings.Join(rows, ",") + `]}`
	out := formatReviewerWorkList([]byte(data))

	for _, want := range []string{"o/r#10", "o/r#20", "o/r#30"} {
		if !strings.Contains(out, want) {
			t.Errorf("capped list missing oldest row %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"o/r#40", "o/r#50"} {
		if strings.Contains(out, banned) {
			t.Errorf("row %q exceeds the per-kick cap of %d:\n%s", banned, reviewerMaxPRsPerKick, out)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("… %d more escalated PRs", 2)) {
		t.Errorf("expected cap summary line for the 2 held-back rows:\n%s", out)
	}
	if strings.Index(out, "#10") > strings.Index(out, "#20") || strings.Index(out, "#20") > strings.Index(out, "#30") {
		t.Errorf("rows must render oldest first:\n%s", out)
	}
}

func reviewerTestScheduler(t *testing.T, acmmLevel int, fixture string) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if fixture != "" {
		if err := os.WriteFile(ciFailingPath, []byte(fixture), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Agents: map[string]config.AgentConfig{
			// An operator-added adjudicator: name is NOT "reviewer"; the ROLE
			// is what routes it into the lane.
			"adjudicator": {Role: "reviewer", Mode: "ISSUES_AND_PRS"},
			"scanner":     {Mode: "ISSUES_AND_PRS"},
		},
	}
	if acmmLevel > 0 {
		cfg.ACMMLevel = &acmmLevel
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// Below ACMM L5 the kick renders only the dormant notice — no work list, no
// contract — regardless of how deep the escalated queue is. This is the hard
// gate: an operator adding a reviewer-role agent to a low-trust hive must not
// acquire an agent that un-escalates human queues.
func TestBuildReviewerMessage_ACMMGateDormantBelowL5(t *testing.T) {
	for _, level := range []int{0, 1, 2, 3, 4} {
		s := reviewerTestScheduler(t, level, reviewerFixture)
		msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
		if !strings.Contains(msg, "REVIEWER LANE DORMANT") {
			t.Errorf("L%d: kick must say the lane is dormant:\n%s", level, msg)
		}
		if !strings.Contains(msg, "Stand down") {
			t.Errorf("L%d: dormant kick must order a stand-down:\n%s", level, msg)
		}
		for _, banned := range []string{"#9", "#3", "ADJUDICATION CONTRACT", "REPAIR", "gh pr checkout"} {
			if strings.Contains(msg, banned) {
				t.Errorf("L%d: dormant kick must not render %q:\n%s", level, banned, msg)
			}
		}
	}
}

// At L5+ the kick carries the work list and the full adjudication contract:
// the three exclusive verdicts, the same-branch repair rule, label mechanics,
// the cap, mandatory attribution/advisory audit, and the human-authored-PR and
// reviewer-passed invariants. Below L6 closing is forbidden.
func TestBuildReviewerMessage_ContractAtL5(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	for _, want := range []string{
		"[agent:adjudicator]",
		"test-org/console#9",
		"REPAIR",
		"DE-ESCALATE",
		"RECOMMEND-CLOSE",
		"EXACTLY ONE verdict",
		"diff/test-count parity",
		"SAME branch",
		"Do NOT open a replacement PR",
		"--remove-label needs-human",
		"--add-label " + ReviewerPassedLabel,
		"[reviewer] recommend close:",
		"hive-review <number> --repo <owner/repo> --comment",
		"agent_pr_reviewed",
		"poll the `.result.json` path",
		"`\"ok\": true`",
		"A queued request is not yet",
		"bd create --title \"Reviewer adjudication:",
		"--type advisory --priority 2 --actor adjudicator",
		"--external-ref \"gh-<owner/repo>#<number>\"",
		"advisory digest",
		"If either record fails, leave `needs-human` in place",
		"--add-label " + ReviewerRecommendCloseLabel,
		"NEVER close the PR yourself",
		fmt.Sprintf("AT MOST %d PRs this kick", reviewerMaxPRsPerKick),
		"NEVER touch a human-authored PR",
		"prior reviewer pass",
		"NEVER run gh pr list",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("L5 reviewer kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "MAY then close") {
		t.Error("closing must not be offered below L6")
	}
}

// At L6, and only at L6+, the kick grants close authority after the
// recommend-close comment.
func TestBuildReviewerMessage_CloseAllowedAtL6(t *testing.T) {
	s := reviewerTestScheduler(t, 6, reviewerFixture)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msg, "MAY then close the PR yourself") {
		t.Errorf("L6 kick must grant close authority:\n%s", msg)
	}
	if strings.Contains(msg, "NEVER close the PR yourself") {
		t.Errorf("L6 kick must not simultaneously forbid closing:\n%s", msg)
	}
}

// An empty escalated queue produces a stand-down kick, not a hunt.
func TestBuildReviewerMessage_EmptyQueueStandsDown(t *testing.T) {
	s := reviewerTestScheduler(t, 5, `{"ci_failing":[{"number":1,"repo":"o/r","title":"red, not escalated"}]}`)
	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msg, "(none)") || !strings.Contains(msg, "stand down") {
		t.Errorf("empty queue must render (none) + stand down:\n%s", msg)
	}
	if strings.Contains(msg, "ADJUDICATION CONTRACT") {
		t.Errorf("no contract without work:\n%s", msg)
	}
}

// BuildAgentMessage routes by ROLE, not name: an operator-added agent with
// role: reviewer receives the adjudication kick through the normal resolution
// chain's hardcoded fallback, so it participates in ordinary cadence kicks
// like every other role.
func TestBuildAgentMessage_RoutesReviewerRole(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	msg := s.BuildAgentMessage("adjudicator", nil, &github.ActionableResult{})
	if !strings.Contains(msg, "[agent:adjudicator]") || !strings.Contains(msg, "ADJUDICATION CONTRACT") {
		t.Errorf("role: reviewer agent must receive the adjudication kick:\n%s", msg)
	}
	// A role-less agent must not be captured by the reviewer routing.
	scanner := s.BuildAgentMessage("scanner", nil, &github.ActionableResult{})
	if strings.Contains(scanner, "ADJUDICATION CONTRACT") {
		t.Errorf("scanner must not receive the reviewer kick:\n%s", scanner)
	}
}

// #5617 item 4: "oldest first" now means the PR's real creation time. The old
// (repo, number) proxy sorted by repo NAME first, so against the per-kick cap
// an old escalated PR in a late-alphabet repo was starved behind newer ones in
// an early-alphabet repo on every kick, forever.
func TestFormatReviewerWorkList_TrueCreationTimeOrdering(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":9001,"repo":"alpha/console","title":"newest","escalated":true,"created_at":"2026-09-01T00:00:00Z"},
	  {"number":12,"repo":"zeta/service","title":"oldest","escalated":true,"created_at":"2026-06-01T00:00:00Z"},
	  {"number":9000,"repo":"alpha/console","title":"middle","escalated":true,"created_at":"2026-07-01T00:00:00Z"}
	]}`
	out := formatReviewerWorkList([]byte(data))
	oldest := strings.Index(out, "zeta/service#12")
	middle := strings.Index(out, "alpha/console#9000")
	newest := strings.Index(out, "alpha/console#9001")
	if oldest < 0 || middle < 0 || newest < 0 {
		t.Fatalf("every escalated row must be listed:\n%s", out)
	}
	// Under the old key the two alpha/console rows came first purely because
	// "alpha" < "zeta", and the genuinely oldest PR came last.
	if oldest > middle || middle > newest {
		t.Errorf("rows must order by creation time, not (repo, number):\n%s", out)
	}
	if !strings.Contains(out, "opened: 2026-06-01T00:00:00Z") {
		t.Errorf("the ordering key must be visible in the row so the reviewer can check it:\n%s", out)
	}
}

// A ci-failing.json from a hub that recorded no creation times still works:
// those rows keep the old (repo, number) proxy among themselves and sort AFTER
// every row whose age is actually known — an unproven age must not jump ahead
// of a measured one.
func TestFormatReviewerWorkList_UnknownCreationTimeSortsLast(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":1,"repo":"aaa/repo","title":"no timestamp","escalated":true},
	  {"number":2,"repo":"aaa/repo","title":"also none","escalated":true},
	  {"number":900,"repo":"zzz/repo","title":"known age","escalated":true,"created_at":"2026-08-01T00:00:00Z"}
	]}`
	out := formatReviewerWorkList([]byte(data))
	known := strings.Index(out, "zzz/repo#900")
	if known < 0 || known > strings.Index(out, "aaa/repo#1") {
		t.Errorf("a row with a known creation time must precede every row without one:\n%s", out)
	}
	if strings.Index(out, "aaa/repo#1") > strings.Index(out, "aaa/repo#2") {
		t.Errorf("timestamp-less rows must keep the (repo, number) proxy among themselves:\n%s", out)
	}
	if n := strings.Count(out, "opened:"); n != 1 {
		t.Errorf("only the row with a known creation time may render an opened line (got %d):\n%s", n, out)
	}
}

// hivecommons/hive#9477: an escalated PR that is conflicted has no failing
// check (CI does not run on it), so it is absent from ci_failing. The hub lists
// it under "escalated" instead, and the lane must adjudicate it from there,
// with GitHub's mergeability shown as the evidence. A row present in both lists
// renders once.
func TestFormatReviewerWorkList_IncludesEscalatedNonFailingPRs(t *testing.T) {
	fixture := `{"ci_failing":[
  {"number":9,"repo":"o/r","title":"red escalated","escalated":true,"failing_checks":["build"]}
],"escalated":[
  {"number":9258,"repo":"o/r","title":"conflicted escalated","escalated":true,
   "labels":["needs-human"],"mergeable":"no","ci_status":"success"},
  {"number":9,"repo":"o/r","title":"red escalated","escalated":true},
  {"number":77,"repo":"o/r","title":"already adjudicated","escalated":true,
   "labels":["reviewer-passed"],"mergeable":"no"}
]}`
	out := formatReviewerWorkList([]byte(fixture))
	for _, want := range []string{"o/r#9258", "state: mergeable=no, ci=success", "gh pr checkout 9258 --repo o/r", "o/r#9 "} {
		if !strings.Contains(out, want) {
			t.Errorf("work list missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "o/r#9 "); n != 1 {
		t.Errorf("row in both lists rendered %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "#77") {
		t.Errorf("reviewer-passed row from the escalated list must be excluded:\n%s", out)
	}
}

// hivecommons/hive#9477: with the L6 pack roster, the template-less
// adjudicator receives the lane contract and the escalated PR, while the queue
// reviewer keeps its kick_template (reviewer-queue.md). Before the fix the
// only role-reviewer agent in the pack was the templated one, so the lane was
// unreachable.
func TestBuildAgentMessage_PackL6AdjudicatorGetsLaneReviewerKeepsQueue(t *testing.T) {
	s := reviewerTestScheduler(t, 6, `{"ci_failing":[],"escalated":[
  {"number":9258,"repo":"test-org/console","title":"conflicted escalated","escalated":true,
   "labels":["needs-human"],"mergeable":"no","ci_status":"success"}]}`)
	pack, err := config.ACMMPackByLevel(6)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Agents = map[string]config.AgentConfig{}
	for _, pa := range pack.Agents {
		if pa.Name == "reviewer" || pa.Name == "adjudicator" {
			s.cfg.Agents[pa.Name] = config.AgentConfig{Role: pa.Role, Mode: pa.Mode, KickTemplate: pa.KickTemplate}
		}
	}
	if len(s.cfg.Agents) != 2 {
		t.Fatalf("L6 pack must define both reviewer and adjudicator, got %v", s.cfg.Agents)
	}

	lane := s.BuildAgentMessage("adjudicator", nil, &github.ActionableResult{})
	for _, want := range []string{"[agent:adjudicator]", "ADJUDICATION CONTRACT", "test-org/console#9258", "MAY then close the PR yourself"} {
		if !strings.Contains(lane, want) {
			t.Errorf("adjudicator kick missing %q:\n%s", want, lane)
		}
	}

	queue := s.BuildAgentMessage("reviewer", nil, &github.ActionableResult{})
	if strings.Contains(queue, "ADJUDICATION CONTRACT") {
		t.Errorf("the queue reviewer must keep reviewer-queue.md, not the lane contract:\n%s", queue)
	}
	if queue == "" || !strings.Contains(queue, "[agent:reviewer]") {
		t.Errorf("the queue reviewer must still receive its templated kick, got:\n%s", queue)
	}
}

const reviewerOneEscalatedFixture = `{"ci_failing":[
  {"number":9,"repo":"test-org/console","title":"escalated split PR","agent":"scanner","escalated":true,
   "failing_checks":["build-gate"]}
]}`

const reviewerNoneEscalatedFixture = `{"ci_failing":[{"number":1,"repo":"test-org/console","title":"red, not escalated"}]}`

// reviewerMultiRepoScheduler is reviewerTestScheduler on a multi-repo project,
// the shape where the repos section carries the MULTI-REPO COVERAGE rotation.
func reviewerMultiRepoScheduler(t *testing.T, acmmLevel int, fixture string) *Scheduler {
	t.Helper()
	s := reviewerTestScheduler(t, acmmLevel, fixture)
	s.cfg.Project.Repos = []string{"test-org/console", "test-org/docs", "test-org/infra"}
	return s
}

// reviewerLaneGate is the one place that decides whether the lane is awake and
// has work; both the kick text and the scheduled-kick skip read it (#11046).
func TestReviewerLaneGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		level        int
		fixture      string
		wantReason   string
		wantWorkList bool
	}{
		{name: "below ACMM gate", level: reviewerLaneMinACMMLevel - 1, fixture: reviewerFixture, wantReason: "dormant below ACMM 5"},
		{name: "no ACMM level", level: 0, fixture: reviewerFixture, wantReason: "dormant below ACMM 5"},
		{name: "empty escalated set", level: 5, fixture: reviewerNoneEscalatedFixture, wantReason: reviewerLaneIdleNothingToAdjudicate},
		{name: "missing ci-failing.json", level: 6, fixture: "", wantReason: reviewerLaneIdleNothingToAdjudicate},
		{name: "one escalated PR", level: 5, fixture: reviewerOneEscalatedFixture, wantWorkList: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewerTestScheduler(t, tc.level, tc.fixture)
			level, workList, reason := s.reviewerLaneGate()
			if level != tc.level {
				t.Errorf("level = %d, want %d", level, tc.level)
			}
			if reason != tc.wantReason {
				t.Errorf("idle reason = %q, want %q", reason, tc.wantReason)
			}
			if (workList != "") != tc.wantWorkList {
				t.Errorf("work list present = %v, want %v:\n%s", workList != "", tc.wantWorkList, workList)
			}
		})
	}
}

// #11046: a scheduled reviewer-lane kick with nothing to adjudicate is not
// sent. No KickMessage means nothing is dispatched, so no LastKick is stamped
// and no stand-down outcome can mark the lane BLOCKED. Other due agents in the
// same pass are unaffected, and one escalated PR still produces the contract.
func TestBuildKickMessages_ReviewerLaneSkipsIdleKick(t *testing.T) {
	for _, tc := range []struct {
		name     string
		level    int
		fixture  string
		wantKick bool
	}{
		{name: "empty escalated set", level: 5, fixture: reviewerNoneEscalatedFixture},
		{name: "missing ci-failing.json", level: 6, fixture: ""},
		{name: "ACMM below the gate", level: reviewerLaneMinACMMLevel - 1, fixture: reviewerFixture},
		{name: "one escalated PR", level: 5, fixture: reviewerOneEscalatedFixture, wantKick: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewerMultiRepoScheduler(t, tc.level, tc.fixture)
			msgs := s.BuildKickMessages(&github.ActionableResult{}, []string{"adjudicator", "scanner"})
			var lane *KickMessage
			sawScanner := false
			for i := range msgs {
				switch msgs[i].Agent {
				case "adjudicator":
					lane = &msgs[i]
				case "scanner":
					sawScanner = true
				}
			}
			if !sawScanner {
				t.Error("skipping the reviewer lane must not drop other due agents' kicks")
			}
			if !tc.wantKick {
				if lane != nil {
					t.Fatalf("idle reviewer lane must not be kicked, got:\n%s", lane.Message)
				}
				return
			}
			if lane == nil {
				t.Fatal("reviewer lane with an escalated PR must be kicked")
			}
			for _, want := range []string{"[agent:adjudicator]", "ADJUDICATION CONTRACT", "test-org/console#9"} {
				if !strings.Contains(lane.Message, want) {
					t.Errorf("working reviewer kick missing %q:\n%s", want, lane.Message)
				}
			}
			if strings.Contains(strings.ToLower(lane.Message), "stand down this kick") {
				t.Errorf("working reviewer kick must not stand down:\n%s", lane.Message)
			}
		})
	}
}

// #11045: a reviewer stand-down that IS rendered (a forced or manual kick)
// tells the agent to stand down and nothing else — no repo rotation and no
// "pick a repo and work it" closing line — on a multi-repo project.
func TestBuildAgentMessage_ReviewerStandDownHasNoRepoWorkInstruction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		level   int
		fixture string
	}{
		{name: "empty escalated set", level: 5, fixture: reviewerNoneEscalatedFixture},
		{name: "dormant below ACMM gate", level: reviewerLaneMinACMMLevel - 1, fixture: reviewerFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewerMultiRepoScheduler(t, tc.level, tc.fixture)
			msg, reason := s.buildAgentMessage("adjudicator", nil, &github.ActionableResult{})
			if reason == "" {
				t.Error("stand-down kick must carry an idle reason")
			}
			if public := s.BuildAgentMessage("adjudicator", nil, &github.ActionableResult{}); public != msg {
				t.Errorf("BuildAgentMessage must render the same forced kick:\n%s\n---\n%s", public, msg)
			}
			if !strings.Contains(strings.ToLower(msg), "stand down") {
				t.Errorf("stand-down kick must say stand down:\n%s", msg)
			}
			for _, banned := range []string{"MULTI-REPO COVERAGE", "Begin now: pick the authorized repo", concreteKickClosingInstruction} {
				if strings.Contains(msg, banned) {
					t.Errorf("stand-down kick must not contain %q:\n%s", banned, msg)
				}
			}
		})
	}
}

// A working reviewer kick and every other agent keep the concrete closing
// instruction: only the stand-down drops it.
func TestBuildAgentMessage_ClosingInstructionKeptWhenNotStandingDown(t *testing.T) {
	s := reviewerMultiRepoScheduler(t, 5, reviewerOneEscalatedFixture)
	for _, agent := range []string{"adjudicator", "scanner"} {
		msg, reason := s.buildAgentMessage(agent, nil, &github.ActionableResult{})
		if reason != "" {
			t.Errorf("%s: unexpected idle reason %q", agent, reason)
		}
		if !strings.Contains(msg, concreteKickClosingInstruction) {
			t.Errorf("%s: kick must carry the closing instruction:\n%s", agent, msg)
		}
	}
}
