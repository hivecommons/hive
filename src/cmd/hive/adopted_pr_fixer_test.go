package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Once the hive adopts a contributor PR in place (stage 074) the PR is the
// hive's to finish: a merge conflict or a red check on it is a fixer kick,
// with the same routing as a hive-authored PR, pushing to the PR's own
// branch. These tests pin that an adopted PR reaches ci-failing.json for
// both kinds of work — including past the sentinel alert, which is a merge
// checkpoint for a maintainer, not a repair checkpoint — while a contributor
// PR the hive did NOT adopt never becomes a push instruction.

// adoptInPlace records a live in-place adoption for repo#number in the
// shared stage store, which writeMergeEligible consults, and forgets it
// again when the test ends.
func adoptInPlace(t *testing.T, repo string, number int, head string) {
	t.Helper()
	key := stagelabels.Key(repo, number)
	store := stagelabels.Shared()
	store.Observe(key, stagelabels.AdoptedByHive, "check-failing:test")
	store.MarkAdoptedInPlace(key, head, "contributor-offline", time.Now())
	t.Cleanup(func() { store.Forget(key) })
	if rec, ok := store.Get(key); !ok || !rec.AdoptedInPlace() {
		t.Fatalf("adoption not recorded for %s", key)
	}
}

func TestClassifyMergeEligibility_AdoptedPRReachesFixerPastSentinel(t *testing.T) {
	red := github.PullRequest{Number: 13252, Author: "mendezr", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "failure",
		FailingChecks: []string{"test (rest 5/5)", "coverage"},
		Labels:        []string{"sentinel-alert", "stage/074-adopted-by-hive", "author/clanker"}}
	conflicted := github.PullRequest{Number: 13251, Author: "arteeh", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "pending",
		Labels: []string{"sentinel-alert", "needs-rebase", "stage/074-adopted-by-hive"}}
	green := github.PullRequest{Number: 13253, Author: "mendezr", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success",
		Labels: []string{"sentinel-alert", "stage/074-adopted-by-hive"}}

	cases := []struct {
		name    string
		pr      github.PullRequest
		adopted bool
		want    mergeBucket
		reason  string
	}{
		{"adopted red PR with sentinel-alert is fix work", red, true, mergeBucketFailing, "CI failing"},
		{"adopted conflicted PR with sentinel-alert is rebase work", conflicted, true, mergeBucketConflict, "rebase"},
		{"adopted green PR with sentinel-alert still does not merge", green, true, mergeBucketSkip, blockLabelSentinel},
		{"non-adopted red PR with sentinel-alert is nobody's fix work", red, false, mergeBucketSkip, blockLabelSentinel},
		{"non-adopted conflicted PR with sentinel-alert is skipped", conflicted, false, mergeBucketSkip, blockLabelSentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict, _ := classifyMergeEligibility(tc.pr, false, "hivecommons/hive", mergeGates{adopted: tc.adopted})
			if bucket != tc.want {
				t.Fatalf("bucket = %v, want %v (verdict %+v)", bucket, tc.want, verdict)
			}
			if !strings.Contains(verdict.Reason, tc.reason) {
				t.Errorf("verdict reason %q does not mention %q", verdict.Reason, tc.reason)
			}
		})
	}
}

func TestWriteMergeEligible_AdoptedPRConflictSchedulesRebaseKick(t *testing.T) {
	const repo = "hivecommons/hive"
	adoptInPlace(t, repo, 13251, "baa0f5b")
	pr := github.PullRequest{Number: 13251, Repo: "hive", Author: "arteeh", HeadSHA: "baa0f5b",
		HeadRef: "feat/reviewer-confidence", HeadRepo: "arteeh/hive", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "pending",
		Labels: []string{"needs-rebase", "stage/074-adopted-by-hive", "author/clanker"}}

	_, failing := runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons"})
	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want the adopted conflicted PR listed once", failing)
	}
	row := failing[0]
	if !row.Conflict || row.MergeableState != "dirty" {
		t.Errorf("row = %+v, want a conflict row carrying mergeable_state=dirty", row)
	}
	if row.ReachableAction != github.ReachableActionPush {
		t.Errorf("reachable_action = %q, want push: the hive owns an adopted PR's branch", row.ReachableAction)
	}
	if row.Agent != "scanner" {
		t.Errorf("agent = %q, want the fallback fixer (scanner) for an adopted PR", row.Agent)
	}
}

func TestWriteMergeEligible_AdoptedRedPRSchedulesFixKickPastSentinel(t *testing.T) {
	const repo = "hivecommons/hive"
	adoptInPlace(t, repo, 13252, "2f270d7")
	pr := github.PullRequest{Number: 13252, Repo: "hive", Author: "mendezr", HeadSHA: "2f270d7",
		HeadRef: "feat/contribute-watch", HeadRepo: "mendezr/hive", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "failure",
		FailingChecks: []string{"test (rest 5/5)", "coverage"},
		Labels:        []string{"sentinel-alert", "stage/074-adopted-by-hive", "author/clanker"}}

	_, failing := runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons"})
	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want the adopted red PR listed once", failing)
	}
	row := failing[0]
	if row.Conflict || len(row.FailingChecks) != 2 {
		t.Errorf("row = %+v, want a red-CI row with both failing checks", row)
	}
	if row.ReachableAction != github.ReachableActionPush || row.Agent != "scanner" {
		t.Errorf("row = %+v, want a push row owned by the fallback fixer", row)
	}
	if !row.Adopted || !row.SentinelAlert {
		t.Errorf("row = %+v, want adopted=true sentinel_alert=true so the kick says fix CI but leave the label", row)
	}
}

func TestWriteMergeEligible_NonAdoptedContributorPRGetsNoHivePush(t *testing.T) {
	conflicted := github.PullRequest{Number: 13212, Repo: "hive", Author: "hanthor", HeadSHA: "3eb1016",
		HeadRef: "feat/x", HeadRepo: "hanthor/hive", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "pending",
		Labels: []string{"needs-rebase", "stage/072-returned-to-contributor"}}
	red := github.PullRequest{Number: 13213, Repo: "hive", Author: "hanthor", HeadSHA: "4eb1016",
		HeadRef: "feat/y", HeadRepo: "hanthor/hive", FromFork: true,
		Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "failure", FailingChecks: []string{"test"},
		Labels: []string{"stage/072-returned-to-contributor"}}
	sentinel := github.PullRequest{Number: 13214, Repo: "hive", Author: "hanthor", HeadSHA: "5eb1016",
		HeadRef: "feat/z", HeadRepo: "hanthor/hive", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "failure", FailingChecks: []string{"test"},
		Labels: []string{"sentinel-alert", "stage/072-returned-to-contributor"}}

	_, failing := runWriteMergeEligible(t, []github.PullRequest{conflicted, red, sentinel}, mergeEligibleInputs{org: "hivecommons"})
	for _, row := range failing {
		switch row.Number {
		case 13212:
			t.Errorf("conflicted contributor PR %d was filed for a rebase: %+v", row.Number, row)
		case 13214:
			t.Errorf("sentinel-alerted contributor PR %d was filed for repair: %+v", row.Number, row)
		case 13213:
			if row.ReachableAction != github.ReachableActionCommentOnly || row.Adopted {
				t.Errorf("red contributor fork PR row = %+v, want comment-only and not adopted", row)
			}
		}
	}
}

func useMemoryRebaseAttempts(t *testing.T) *rebasemerge.Attempts {
	t.Helper()
	store := rebasemerge.NewAttempts("")
	prev := rebaseAttempts
	rebaseAttempts = func() *rebasemerge.Attempts { return store }
	t.Cleanup(func() { rebaseAttempts = prev })
	return store
}

func trustedAuthorRebaseConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}
	cfg.AutoMerge.TrustedAuthors.Enabled = true
	return cfg
}

// hivecommons/hive#15103: a trusted author's PR (the operator's own
// identity, opened through a session agent) that goes dirty is the hive's
// rebase work — the trusted-author lane never rebases, so the fixer router
// must list it. Attempts are capped: the third dirty head stops the kicks.
func TestWriteMergeEligible_TrustedAuthorConflictGetsRebaseKickWithLoopGuard(t *testing.T) {
	store := useMemoryRebaseAttempts(t)
	cfg := trustedAuthorRebaseConfig()
	mk := func(head string) github.PullRequest {
		return github.PullRequest{Number: 15087, Repo: "hive", Author: "clubanderson", HeadSHA: head,
			HeadRef: "feat/decision-actions", HeadRepo: "hivecommons/hive", BaseRef: "v5",
			Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "pending",
			Labels: []string{"needs-rebase"}}
	}
	_, failing := runWriteMergeEligible(t, []github.PullRequest{mk("aaa1111")}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want the trusted author's dirty PR listed once", failing)
	}
	row := failing[0]
	if !row.Conflict || !row.TrustedAuthor || row.ReachableAction != github.ReachableActionPush || row.Agent != "scanner" {
		t.Errorf("row = %+v, want a trusted-author push conflict row owned by the fallback fixer", row)
	}
	if row.RebaseAttempt != 1 || row.RebaseAttemptsMax != rebasemerge.MaxRebaseAttempts || row.BaseRef != "v5" {
		t.Errorf("row = %+v, want attempt 1/%d onto v5", row, rebasemerge.MaxRebaseAttempts)
	}
	// The fixer pushed and the head is still dirty: attempt 2 of 2.
	_, failing = runWriteMergeEligible(t, []github.PullRequest{mk("bbb2222")}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 1 || failing[0].RebaseAttempt != 2 {
		t.Fatalf("second dirty head: ci_failing = %+v, want attempt 2", failing)
	}
	// A third dirty head is past the cap: no kick, a person's turn.
	_, failing = runWriteMergeEligible(t, []github.PullRequest{mk("ccc3333")}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 0 {
		t.Fatalf("third dirty head: ci_failing = %+v, want nothing (attempts exhausted)", failing)
	}
	if a, ok := store.Get("hivecommons/hive", 15087); !ok || !a.Exhausted() {
		t.Fatalf("attempts = %+v ok=%v, want an exhausted episode", a, ok)
	}
	// Once the PR is clean again the episode ends.
	clean := mk("ddd4444")
	clean.Mergeable, clean.MergeableState, clean.Labels = github.MergeableYes, "clean", nil
	runWriteMergeEligible(t, []github.PullRequest{clean}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if _, ok := store.Get("hivecommons/hive", 15087); ok {
		t.Fatal("episode should be cleared once the PR is no longer conflicting")
	}
}

func TestWriteMergeEligible_UntrustedConflictIsNotHiveRebaseWork(t *testing.T) {
	useMemoryRebaseAttempts(t)
	cfg := trustedAuthorRebaseConfig()
	pr := github.PullRequest{Number: 15090, Repo: "hive", Author: "stranger", HeadSHA: "eee5555",
		HeadRef: "feat/x", HeadRepo: "hivecommons/hive", BaseRef: "v5",
		Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "pending", Labels: []string{"needs-rebase"}}
	_, failing := runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 0 {
		t.Fatalf("ci_failing = %+v, want nothing for an untrusted author's conflict", failing)
	}
	cfg.AutoMerge.TrustedAuthors.Enabled = false
	pr.Author = "clubanderson"
	_, failing = runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 0 {
		t.Fatalf("ci_failing = %+v, want nothing when the trusted-author tier is off", failing)
	}
}
