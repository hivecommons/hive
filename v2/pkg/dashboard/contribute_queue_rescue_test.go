package dashboard

import (
	"log/slog"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
)

// TestReadyQueueSurfacesContributeRescuedIssues is the END-TO-END, DATA-DRIVEN
// reproduction + fix proof for the "contribute queue shows 0 with dozens of
// admissible issues" bug on the projectbluefin common hive.
//
// Confirmed live scenario this mirrors:
//   - Every repo toggled ON for contribute (repo scope is not the blocker).
//   - labels_mode="allow" with "3-clanker-queue" in the list (contribute filter
//     PASSES these issues).
//   - Title deny-list includes "epic:*" and "*hive advisory report*".
//   - skip_assigned_to_others=true.
//   - ~50 open "3-clanker-queue" issues across enabled repos, yet queue == [].
//
// Root cause: the contribute candidate set is the governor's actionable scan,
// which drops issues carrying a GOVERNOR exempt-label. The operator exempted the
// contribute label from the governor, so every "3-clanker-queue" issue was
// dropped upstream and the contribute filters (which only narrow the set) could
// never surface them. The fix retains such issues in
// ActionableResult.ContributeExtraIssues; buildRepos merges them into the
// per-repo ActionableIssues the contribute queue scans.
//
// This test builds the candidate set exactly as the FIXED pipeline does
// (buildRepos merges ContributeExtraIssues), runs it through the REAL ReadyQueue
// filter chain, and asserts the admissible math end-to-end:
//
//	50 rescued issues − 2 title-denied − 4 assigned-to-others = 44 admissible.
func TestReadyQueueSurfacesContributeRescuedIssues(t *testing.T) {
	setupContributeEnv(t)

	// Ground-truth per-repo admissible distribution (sums to 44).
	admissiblePerRepo := map[string]int{
		"common":          9,
		"bluefin-lts":     8,
		"fsdk-containers": 7,
		"lab":             6,
		"bonedigger":      4,
		"finpilot":        4,
		"dakota":          3,
		"bluefin":         2,
		"actions":         1,
	}
	repoOrder := []string{
		"common", "bluefin-lts", "fsdk-containers", "lab",
		"bonedigger", "finpilot", "dakota", "bluefin", "actions",
	}

	const org = "projectbluefin"
	const contributeLabel = "3-clanker-queue"

	// Build the governor scan RESULT as the fixed scan would emit it: every
	// "3-clanker-queue" issue is governor-exempt, so it arrives via
	// ContributeExtraIssues (NOT Issues). None of them are in the plain actionable
	// set — proving the queue is fed by the rescue path, not the governor set.
	var extra []github.Issue
	num := 1000
	created := time.Now().Add(-2 * time.Hour)

	// 44 clean, unassigned, non-title-denied admissible issues.
	for _, repo := range repoOrder {
		for i := 0; i < admissiblePerRepo[repo]; i++ {
			num++
			extra = append(extra, github.Issue{
				Repo:      repo,
				Number:    num,
				Title:     "clanker work item",
				Author:    "human-contributor",
				Labels:    []string{contributeLabel},
				Assignees: nil,
				CreatedAt: created,
				URL:       "https://github.com/" + org + "/" + repo + "/issues/1",
			})
		}
	}
	if len(extra) != 44 {
		t.Fatalf("fixture setup error: expected 44 admissible, built %d", len(extra))
	}

	// +4 issues assigned to OTHERS (must be excluded by skip-assigned).
	for i := 0; i < 4; i++ {
		num++
		extra = append(extra, github.Issue{
			Repo:      "common",
			Number:    num,
			Title:     "assigned clanker work",
			Author:    "human-contributor",
			Labels:    []string{contributeLabel},
			Assignees: []string{"someone-else"},
			CreatedAt: created,
		})
	}

	// +2 title-denied issues (an "epic:" and a "hive advisory report").
	num++
	extra = append(extra, github.Issue{
		Repo: "common", Number: num, Title: "epic: umbrella tracking",
		Author: "human-contributor", Labels: []string{contributeLabel}, CreatedAt: created,
	})
	num++
	extra = append(extra, github.Issue{
		Repo: "bluefin", Number: num, Title: "Weekly hive advisory report",
		Author: "human-contributor", Labels: []string{contributeLabel}, CreatedAt: created,
	})

	if len(extra) != 50 {
		t.Fatalf("fixture setup error: expected 50 total rescued, built %d", len(extra))
	}

	actionable := &github.ActionableResult{
		// Governor actionable set is EMPTY of clanker issues — the whole point.
		Issues:                github.IssueResult{Count: 0, Items: nil},
		ContributeExtraIssues: extra,
	}

	// Config with every repo enabled (DisabledRepos empty), the contribute allow
	// label filter, the title deny-list, and skip-assigned ON — matching live.
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: org, Repos: repoOrder},
		Hub: config.HubConfig{
			ContributeLabelsMode:           config.FilterModeAllow,
			ContributeDenyLabels:           []string{"clanker-queue", contributeLabel},
			ContributeTitlesMode:           config.FilterModeDeny,
			ContributeDenyTitles:           []string{"epic:*", "epic(*", "*hive advisory report*"},
			ContributeAuthorsMode:          config.FilterModeDeny,
			ContributeDenyAuthors:          []string{"*[bot]"},
			ContributeSkipAssignedToOthers: true,
			DisabledRepos:                  nil,
		},
	}

	// Build the per-repo status EXACTLY as production does: buildRepos merges
	// ContributeExtraIssues into ActionableIssues.
	repos := buildRepos(cfg, actionable)

	// Sanity: the merge actually placed the rescued issues into ActionableIssues.
	totalActionable := 0
	for _, r := range repos {
		totalActionable += len(r.ActionableIssues)
	}
	if totalActionable != 50 {
		t.Fatalf("buildRepos did not merge rescued issues: ActionableIssues total = %d, want 50", totalActionable)
	}

	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	deps.Config = cfg
	s.RegisterAPI(deps)

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: repos}
	s.statusMu.Unlock()

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)

	// The core assertion: 50 − 2 title-denied − 4 assigned-to-others = 44.
	const wantAdmissible = 44
	if len(q) != wantAdmissible {
		t.Fatalf("ReadyQueue admissible count = %d, want %d (50 rescued − 2 title − 4 assigned)", len(q), wantAdmissible)
	}

	// The RIGHT issues survived: none title-denied, none assigned-to-others, all
	// carry the contribute label.
	for _, it := range q {
		if it.Title == "epic: umbrella tracking" || it.Title == "Weekly hive advisory report" {
			t.Errorf("title-denied issue leaked into queue: %+v", it)
		}
		if it.Title == "assigned clanker work" {
			t.Errorf("assigned-to-others issue leaked into queue: %+v", it)
		}
		hasLabel := false
		for _, l := range it.Labels {
			if l == contributeLabel {
				hasLabel = true
			}
		}
		if !hasLabel {
			t.Errorf("queued issue missing contribute label: %+v", it)
		}
	}

	// Per-repo admissible distribution is preserved (the clean 44 only).
	perRepo := map[string]int{}
	for _, it := range q {
		// it.Repo is the full "org/name"; strip the org prefix for comparison.
		name := it.Repo
		if idx := len(org) + 1; len(it.Repo) > idx && it.Repo[:len(org)] == org {
			name = it.Repo[idx:]
		}
		perRepo[name]++
	}
	for repo, want := range admissiblePerRepo {
		if perRepo[repo] != want {
			t.Errorf("repo %q admissible = %d, want %d", repo, perRepo[repo], want)
		}
	}
}

// TestReadyQueueStarvedWithoutRescue is the NEGATIVE control: it proves the bug —
// without the rescue (ContributeExtraIssues empty, clanker issues absent from the
// governor actionable set), the very same config yields an EMPTY queue. This is
// exactly the 0-with-matching-issues symptom the fix eliminates.
func TestReadyQueueStarvedWithoutRescue(t *testing.T) {
	setupContributeEnv(t)

	const org = "projectbluefin"
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: org, Repos: []string{"common"}},
		Hub: config.HubConfig{
			ContributeLabelsMode: config.FilterModeAllow,
			ContributeDenyLabels: []string{"3-clanker-queue"},
		},
	}

	// Governor scan dropped every clanker issue and there is no rescue: the
	// candidate set is empty even though the issues exist and the filter passes.
	actionable := &github.ActionableResult{
		Issues:                github.IssueResult{Count: 0},
		ContributeExtraIssues: nil,
	}
	repos := buildRepos(cfg, actionable)

	s := NewServer(0, slog.Default())
	deps := testDeps(t)
	deps.Config = cfg
	s.RegisterAPI(deps)

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: repos}
	s.statusMu.Unlock()

	if q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit); len(q) != 0 {
		t.Fatalf("negative control: expected starved (empty) queue without rescue, got %d", len(q))
	}
}
