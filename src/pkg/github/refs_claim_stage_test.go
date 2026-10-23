package github

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A Refs-only PR can leave the issue in the enumeration's actionable set,
// but dispatch withholds it. Both stage derivation paths must report coverage.
func TestRefsClaimStagesAsCovered(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	ledger.Reconcile([]IssueClaim{{Repo: "acme/widgets", Issue: 17, PRRepo: "acme/widgets", PRNumber: 46, Reference: true, ObservedAt: time.Now()}}, true)
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 17}})
	FilterClaimedIssues(result, ledger, nil, testLogger())
	inputs := IssueStageInputs(result)
	if len(inputs) != 1 {
		t.Fatalf("inputs = %+v", inputs)
	}
	prStage := func(repo string, number int) stagelabels.Stage {
		if repo != "acme/widgets" || number != 46 {
			t.Fatalf("covering PR lookup = %s#%d", repo, number)
		}
		return stagelabels.Fixing
	}
	in := inputs[0]
	stage, covered, _ := IssueStageFor(in, prStage)
	if stage != stagelabels.CoveredByPR || !covered {
		t.Fatalf("stage = %s, covered = %v", stage, covered)
	}
	decision := IssueDeriver().Derive(issueFactsFrom(in, prStage))
	if decision.Stage != stage {
		t.Fatalf("engine = %s, legacy = %s", decision.Stage, stage)
	}
	released := &ActionableResult{}
	released.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 17}})
	FilterClaimedIssues(released, ledger, func(string, int) bool { return true }, testLogger())
	if stage, _, _ := IssueStageFor(IssueStageInputs(released)[0], prStage); stage == stagelabels.CoveredByPR {
		t.Fatal("red+stale reference must release coverage")
	}
	in.Labels = []string{"hold"}
	if stage, _, _ := IssueStageFor(in, prStage); stage != stagelabels.None {
		t.Fatalf("hold must outrank coverage: %s", stage)
	}
	in.Labels = nil
	in.Claim.MergedPR = true
	if stage, _, _ := IssueStageFor(in, prStage); stage == stagelabels.CoveredByPR {
		t.Fatal("merged reference must release coverage")
	}
}

func TestRefsCoverageDoesNotUpgradeClosingScope(t *testing.T) {
	fake := newIssueStageServer(t)
	fake.prBodies[42] = "Refs #4"
	fake.prBases[42] = "v5"
	fake.prAuthors[42] = "hive[bot]"
	client := issueStageTestClient(t, fake.srv, true)
	client.SetAppBotLogin("hive[bot]")
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	issue := Issue{Repo: "acme/widgets", Number: 4}
	annotateIssueWithClaim(&issue, IssueClaim{PRNumber: 42, PRRepo: issue.Repo, PRURL: "https://x/pr/42", Reference: true})
	result.Issues = IssueResultFromItems([]Issue{issue})
	res := syncSeeded(t, fake, client, result, store, stageNow())
	if res.PRBodiesEdited != 0 {
		t.Fatalf("coverage upgraded a non-closing claim: %+v", res)
	}
	rec, ok := store.Get(stagelabels.Key(issue.Repo, issue.Number))
	if !ok || rec.Stage != stagelabels.CoveredByPR {
		t.Fatalf("refs issue record = %+v, present = %v", rec, ok)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prPatches[42] != "" {
		t.Fatalf("PR body was edited: %q", fake.prPatches[42])
	}
}

func TestTrackerClassificationConversionThreshold(t *testing.T) {
	for _, count := range []int{1, 2, 3} {
		for _, marker := range []string{"none", "title", "label"} {
			t.Run(fmt.Sprintf("%d/%s", count, marker), func(t *testing.T) {
				body := ""
				for i := 1; i <= count; i++ {
					body += fmt.Sprintf("- [ ] #%d\n", i)
				}
				title := "Ordinary issue"
				var labels []string
				if marker == "title" {
					title = "[epic] Coordination"
				}
				if marker == "label" {
					labels = []string{"tracker"}
				}
				want := count >= trackerTaskListMin || marker != "none"
				issue := Issue{Title: title, Labels: labels, Body: body, IsTracker: isTracker(title, labels, body)}
				apiIssue := &gh.Issue{Title: &title, Body: &body}
				for _, row := range []RepoWorkIssue{
					repoWorkIssueFromIssue(issue, "claimed", "live claim"),
					repoWorkIssue("acme/widgets", apiIssue, labels, "claimed", "live claim"),
				} {
					if row.IsTracker != want {
						t.Fatalf("IsTracker = %v, want %v: %+v", row.IsTracker, want, row)
					}
					if (row.TrackerProgress != nil) != (count >= trackerTaskListMin) {
						t.Fatalf("unexpected progress: %+v", row.TrackerProgress)
					}
					if row.Children != nil {
						t.Fatal("display-only progress must not populate child ledger")
					}
				}
			})
		}
	}
}

func TestTrackerTaskProgress(t *testing.T) {
	body := "- [x] #1 done\n- [ ] acme/widgets#2 remaining\n- [X] #3 done\n- [ ] ordinary task\n"
	got := TrackerTaskProgress(body)
	if got == nil || got.Total != 3 || got.Completed != 2 {
		t.Fatalf("progress = %+v", got)
	}
	row := repoWorkIssueFromIssue(Issue{Body: body}, "claimed", "live claim")
	if !row.IsTracker || row.TrackerProgress == nil || row.TrackerProgress.Completed != 2 || row.Children != nil {
		t.Fatalf("tracker metadata lost in work details: %+v", row)
	}
	if TrackerTaskProgress("- [ ] ordinary task") != nil {
		t.Fatal("ordinary checklist is not sub-issue progress")
	}
}
