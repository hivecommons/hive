package automerge

// Held / never-started required checks on trusted-lane PRs (#11552, #11553,
// #11877). These pin that the self-authored sweep surfaces workflow runs
// GitHub holds for approval on a same-repo head, never acts on a fork head,
// and names a head whose runs were never created instead of polling it forever.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/oktotest"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func heldRun(id int, headRepo string) map[string]any {
	return map[string]any{
		"id": id, "name": "v2 CI", "status": "completed", "conclusion": "action_required",
		"head_repository": map[string]any{"full_name": headRepo},
	}
}

// missingRequiredPR is PR 23 from TestTrySweepSelfAuthoredPRRequiredChecksDecision:
// the `ci/build` required status is absent, the check-run is green.
func missingRequiredPR(mutate func(*selfAuthoredPR)) selfAuthoredPR {
	pr := selfAuthoredPR{
		number: 23, author: testHiveAppBotLogin, mergeableState: "blocked",
		omitStatus: true, statusState: "success", checkStatus: "completed", checkConclusion: "success",
	}
	if mutate != nil {
		mutate(&pr)
	}
	return pr
}

func TestTrySweepSelfAuthoredPRSurfacesHeldWorkflowRunsWithoutApproving(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget"), heldRun(2, "acme/widget")}
		p.commentBodies = &comments
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	want := workflowApprovalRequiredPrefix + "ci/build"
	for tick := 1; tick <= 2; tick++ {
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
		if err != nil {
			t.Fatalf("tick %d returned error: %v", tick, err)
		}
		if reason != want {
			t.Fatalf("tick %d reason = %q, want %q", tick, reason, want)
		}
	}
	if len(comments) != 1 || comments[0] != workflowApprovalCommandBody("sha23") {
		t.Fatalf("comments = %q, want one %q command with the head marker", comments, workflowApprovalCommand)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merge while workflow approval is required", merged)
	}
}

func TestTrySweepSelfAuthoredPRNeverApprovesForkHeadRuns(t *testing.T) {
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "mallory/widget"), {
			"id": 2, "name": "v2 CI", "status": "completed", "conclusion": "action_required",
		}}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != "required-check-missing:ci/build" {
		t.Fatalf("reason = %q, want the original required-check-missing reason", reason)
	}
}

func TestTrySweepSelfAuthoredPRNamesNeverStartedRequiredCheck(t *testing.T) {
	now := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		updatedAt time.Time
		runs      []map[string]any
		want      string
	}{
		{name: "old head with no runs", updatedAt: now.Add(-2 * heldRunNeverStartedGrace), want: requiredCheckNeverStartedPrefix + "ci/build"},
		{name: "fresh head with no runs", updatedAt: now.Add(-time.Minute), want: "required-check-missing:ci/build"},
		{name: "unknown head age", want: "required-check-missing:ci/build"},
		{
			name: "old head with an in-progress run", updatedAt: now.Add(-2 * heldRunNeverStartedGrace),
			runs: []map[string]any{{"id": 1, "name": "v2 CI", "status": "in_progress", "head_repository": map[string]any{"full_name": "acme/widget"}}},
			want: "required-check-missing:ci/build",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := missingRequiredPR(func(p *selfAuthoredPR) {
				p.workflowRuns = tc.runs
				if !tc.updatedAt.IsZero() {
					p.updatedAt = tc.updatedAt.Format(time.RFC3339)
				}
			})
			var merged []int
			api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.now = func() time.Time { return now }

			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
			if err != nil {
				t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
			}
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestTerminalRequiredCheckHoldLoggedOncePerHeadAndReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{"held", workflowApprovalRequiredPrefix + "ci/build", true},
		{"never started", "required-check-never-started:ci/build", true},
		{"missing", "required-check-missing:ci/build", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminalRequiredCheckHold(tt.reason); got != tt.want {
				t.Fatalf("isTerminalRequiredCheckHold(%q) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}

	c := &Engine{}
	steps := []struct {
		name    string
		number  int
		sha     string
		reason  string
		wantNew bool
	}{
		{"first", 7, "abc", workflowApprovalRequiredPrefix + "ci/build", true},
		{"repeat tick", 7, "abc", workflowApprovalRequiredPrefix + "ci/build", false},
		{"sha case", 7, "ABC", workflowApprovalRequiredPrefix + "ci/build", false},
		{"new head", 7, "def", workflowApprovalRequiredPrefix + "ci/build", true},
		{"other pr", 8, "abc", workflowApprovalRequiredPrefix + "ci/build", true},
		{"reason changed", 7, "def", "required-check-never-started:ci/build", true},
	}
	for _, st := range steps {
		if got := c.markTerminalHoldLogged("acme", "widget", st.number, st.sha, st.reason); got != st.wantNew {
			t.Fatalf("%s: markTerminalHoldLogged = %v, want %v", st.name, got, st.wantNew)
		}
	}
}

// #11923: a Dependabot head autofix-pushed by github-actions[bot] can never
// produce a pull_request_target-only required context. The sweep names it
// required-check-never-ran and asks Dependabot to rebase exactly once per head.
func TestTrySweepTrustedBotNudgesDependabotRebaseOncePerHead(t *testing.T) {
	now := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = "dependabot[bot]"
		p.updatedAt = now.Add(-2 * heldRunNeverStartedGrace).Format(time.RFC3339)
		p.workflowRuns = []map[string]any{{"id": 1, "name": "v2 CI", "status": "completed", "conclusion": "success", "head_repository": map[string]any{"full_name": "acme/widget"}}}
		p.headCommitAuthor = githubActionsBotLogin
		p.commentBodies = &comments
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	c.now = func() time.Time { return now }
	rec := &recordingBoundary{}
	c.mutation = rec

	want := requiredCheckNeverRanPrefix + "ci/build"
	for tick := 1; tick <= 3; tick++ {
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
		if err != nil {
			t.Fatalf("tick %d returned error: %v", tick, err)
		}
		if reason != want {
			t.Fatalf("tick %d reason = %q, want %q", tick, reason, want)
		}
	}
	if len(comments) != 1 || comments[0] != dependabotRebaseComment {
		t.Fatalf("comments = %q, want exactly one %q", comments, dependabotRebaseComment)
	}
	if len(rec.claims) != 1 || rec.claims[0].Kind != effects.KindIssueComment || rec.claims[0].Target != "23" || rec.claims[0].Inputs["head_sha"] != "sha23" {
		t.Fatalf("mutation claims = %#v, want one issue_comment claim for PR 23 head sha23", rec.claims)
	}
	if !isTerminalRequiredCheckHold(want) {
		t.Fatalf("isTerminalRequiredCheckHold(%q) = false, want true so the hold logs once", want)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merge", merged)
	}
}

func TestTrySweepTrustedBotDoesNotNudgeOutsideAutofixShape(t *testing.T) {
	now := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	old := now.Add(-2 * heldRunNeverStartedGrace).Format(time.RFC3339)
	for _, tc := range []struct {
		name         string
		author       string
		commitAuthor string
		updatedAt    string
		want         string
	}{
		{name: "dependabot-pushed head", author: "dependabot[bot]", commitAuthor: "dependabot[bot]", updatedAt: old, want: requiredCheckNeverStartedPrefix + "ci/build"},
		{name: "fresh autofix head", author: "dependabot[bot]", commitAuthor: githubActionsBotLogin, updatedAt: now.Add(-time.Minute).Format(time.RFC3339), want: "required-check-missing:ci/build"},
		{name: "app-authored autofix head", author: testHiveAppBotLogin, commitAuthor: githubActionsBotLogin, updatedAt: old, want: requiredCheckNeverStartedPrefix + "ci/build"},
		{name: "other trusted bot", author: "renovate[bot]", commitAuthor: githubActionsBotLogin, updatedAt: old, want: requiredCheckNeverStartedPrefix + "ci/build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var comments []string
			pr := missingRequiredPR(func(p *selfAuthoredPR) {
				p.author = tc.author
				p.updatedAt = tc.updatedAt
				p.headCommitAuthor = tc.commitAuthor
				p.commentBodies = &comments
			})
			var merged []int
			api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true, "renovate[bot]": true} }
			c.now = func() time.Time { return now }

			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
			if err != nil {
				t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
			}
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if len(comments) != 0 {
				t.Fatalf("comments = %q, want none", comments)
			}
		})
	}
}

func TestDependabotRebaseNudgeDegradesOnAPIErrors(t *testing.T) {
	posts := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"rejected"}`))
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	pr := &gh.PullRequest{User: &gh.User{Login: gh.Ptr("dependabot[bot]")}}

	if c.isAutofixPushedDependabotHead(context.Background(), "widget", "acme", "widget", pr, 23, "sha23") {
		t.Fatal("head-commit lookup failure must not be treated as an autofix head")
	}
	c.nudgeDependabotRebase(context.Background(), "widget", "acme", "widget", 23, "sha23", "ci/build")
	c.nudgeDependabotRebase(context.Background(), "widget", "acme", "widget", 23, "sha23", "ci/build")
	if posts != 1 {
		t.Fatalf("comment POSTs = %d, want 1 (a failed nudge is not retried for the same head)", posts)
	}
	if !c.rebaseNudged("acme", "widget", 23, "SHA23") {
		t.Fatal("rebaseNudged = false after an attempted nudge")
	}
}

// #12004: a trusted bot's head whose required context belongs to a workflow
// that concluded `skipped` is not blocked on it; a never-scheduled context,
// a non-bot author, and a fork-headed run still are.
func TestTrySweepTrustedBotTreatsSkippedByDesignRequiredCheckAsSatisfied(t *testing.T) {
	now := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	skippedRun := func(name, repo string) map[string]any {
		return map[string]any{"id": 1, "name": name, "status": "completed", "conclusion": "skipped", "head_repository": map[string]any{"full_name": repo}}
	}
	for _, tc := range []struct {
		name       string
		author     string
		runs       []map[string]any
		wantBlock  bool
		wantReason string
	}{
		{name: "skipped workflow", author: "dependabot[bot]", runs: []map[string]any{skippedRun("ci/build", "acme/widget")}},
		{name: "no run for the context", author: "dependabot[bot]", runs: []map[string]any{skippedRun("other", "acme/widget")}, wantBlock: true},
		{name: "fork-headed skipped run", author: "dependabot[bot]", runs: []map[string]any{skippedRun("ci/build", "mallory/widget")}, wantBlock: true},
		// The App's own lane is not "trusted-bot", so the skipped run is not
		// vouched for and the required context stays missing.
		{name: "author not a trusted bot", author: testHiveAppBotLogin, runs: []map[string]any{skippedRun("ci/build", "acme/widget")}, wantBlock: true},
		// A human author never reaches the check walk: the sweep rejects the
		// PR as not its own before any required-context evaluation.
		{name: "human author", author: "mallory", runs: []map[string]any{skippedRun("ci/build", "acme/widget")}, wantReason: "not-app-authored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := missingRequiredPR(func(p *selfAuthoredPR) {
				p.author = tc.author
				p.updatedAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
				p.workflowRuns = tc.runs
			})
			var merged []int
			api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
			c.now = func() time.Time { return now }

			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
			if err != nil {
				t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
			}
			if tc.wantReason != "" {
				if reason != tc.wantReason {
					t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
				}
				return
			}
			blocked := strings.HasPrefix(reason, "required-check-")
			if blocked != tc.wantBlock {
				t.Fatalf("reason = %q, blocked = %v, want blocked = %v", reason, blocked, tc.wantBlock)
			}
		})
	}
}

func TestTrySweepTrustedBotSurfacesActionRequiredPendingRuns(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = "dependabot[bot]"
		p.omitStatus = true
		p.statusState = "success"
		p.checkStatus = "completed"
		p.checkConclusion = "success"
		p.extraChecks = []map[string]string{{"name": "ci/build", "status": "in_progress"}}
		p.workflowRuns = []map[string]any{heldRun(7, "acme/widget")}
		p.commentBodies = &comments
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("reason = %q, want %q", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	if len(comments) != 1 || comments[0] != workflowApprovalCommandBody("sha23") {
		t.Fatalf("comments = %q, want one %q command with the head marker", comments, workflowApprovalCommand)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merge while workflow approval is required", merged)
	}
}

func TestTrySweepTrustedBotWorkflowApprovalMissingWorkflowAfterGrace(t *testing.T) {
	now := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = "dependabot[bot]"
		p.omitStatus = true
		p.statusState = "success"
		p.checkStatus = "completed"
		p.checkConclusion = "success"
		p.extraChecks = []map[string]string{{"name": "ci/build", "status": "in_progress"}}
		p.workflowRuns = []map[string]any{heldRun(7, "acme/widget")}
		p.commentBodies = &comments
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	c.now = func() time.Time { return now }

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("first tick returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("first reason = %q, want %q", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	now = now.Add(workflowApprovalGrace + time.Minute)
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("second tick returned error: %v", err)
	}
	if reason != workflowApprovalWorkflowMissingPrefix+"ci/build" {
		t.Fatalf("second reason = %q, want %q", reason, workflowApprovalWorkflowMissingPrefix+"ci/build")
	}
	if len(comments) != 1 || comments[0] != workflowApprovalCommandBody("sha23") {
		t.Fatalf("comments = %q, want one %q command with the head marker", comments, workflowApprovalCommand)
	}
}

func TestTrySweepTrustedBotSentinelBlocksWorkflowApprovalAttention(t *testing.T) {
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = "dependabot[bot]"
		p.statusState = "success"
		p.checkStatus = "in_progress"
		p.workflowRuns = []map[string]any{heldRun(7, "acme/widget")}
		p.extraLabels = append(p.extraLabels, "sentinel-alert")
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	c.sentinelLabel = func() string { return "sentinel-alert" }

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if strings.HasPrefix(reason, workflowApprovalRequiredPrefix) {
		t.Fatalf("reason = %q, want sentinel to block workflow-approval attention", reason)
	}
}

func TestSweepSurfacesTrustedRegistryClankerWorkflowApprovalWithoutMerging(t *testing.T) {
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = "relay-bot"
		p.extraLabels = []string{"role/trusted", "author/clanker"}
		p.workflowRuns = []map[string]any{heldRun(9, "acme/widget")}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	res, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result skipped=%d, want workflow approval attention", res.Skipped)
	}
	if len(merged) != 0 || len(res.Merged) != 0 {
		t.Fatalf("merged server=%v result=%v, want no merge", merged, res.Merged)
	}
}

// prowComment is a comment Prow's approve or trigger plugin would post.
func prowComment(id int, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]string{"login": "kubestellar-prow[bot]"}}
}

// appComment is a comment the hive's own App login posted.
func appComment(id int, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]string{"login": testHiveAppBotLogin}}
}

// The hosted hive posted /ok-to-test fifteen times on homebrew-hive#110:
// Prow owns that command there and only honours it from its trusted users,
// so every post got "Cannot trigger testing until a trusted user reviews the
// PR". Once that reply is on the thread the sweep must stop posting, say so
// once, and park the PR for a person.
func TestTrySweepSelfAuthoredPRStopsOkToTestAfterProwRejection(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.existingComments = &[]map[string]any{
			prowComment(1, "[APPROVALNOTIFIER] This PR is **NOT APPROVED**"),
			appComment(2, workflowApprovalCommandBody("oldhead")),
			prowComment(3, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
		}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	want := workflowApprovalUntrustedPrefix + "ci/build"
	for tick := 1; tick <= 3; tick++ {
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
		if err != nil {
			t.Fatalf("tick %d returned error: %v", tick, err)
		}
		if reason != want {
			t.Fatalf("tick %d reason = %q, want %q", tick, reason, want)
		}
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %q, want exactly one needs-human note and no further %q", comments, workflowApprovalCommand)
	}
	if !strings.HasPrefix(comments[0], workflowApprovalRejectedMarker) || !strings.Contains(comments[0], "needs-human") || !strings.Contains(comments[0], prowOkToTestRejection) {
		t.Fatalf("note = %q, want the rejected marker, a needs-human note and Prow's reply quoted", comments[0])
	}
	if strings.Contains(comments[0], "\n"+workflowApprovalCommand+"\n") {
		t.Fatalf("note = %q, must not itself carry the command on its own line", comments[0])
	}
	if !isTerminalRequiredCheckHold(want) {
		t.Fatalf("isTerminalRequiredCheckHold(%q) = false, want true", want)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merge", merged)
	}
}

// The needs-human note is posted once per PR, even across a restart: a
// thread that already carries it gets nothing new.
func TestTrySweepSelfAuthoredPRProwRejectionNoteIsOncePerPR(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.existingComments = &[]map[string]any{
			appComment(1, workflowApprovalCommandBody("oldhead")),
			prowComment(2, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
			appComment(3, workflowApprovalUntrustedNote(testHiveAppBotLogin, true)),
		}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != workflowApprovalUntrustedPrefix+"ci/build" {
		t.Fatalf("reason = %q, want %q", reason, workflowApprovalUntrustedPrefix+"ci/build")
	}
	if len(comments) != 0 {
		t.Fatalf("comments = %q, want none", comments)
	}
}

// On a Prow-managed repository without the hive-ok-to-test workflow the App
// is never a trusted commenter: do not post at all, park for a person.
func TestTrySweepSelfAuthoredPRNeverPostsOkToTestOnProwRepoWithoutWorkflow(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.existingComments = &[]map[string]any{prowComment(1, "[APPROVALNOTIFIER] This PR is **NOT APPROVED**")}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != workflowApprovalUntrustedPrefix+"ci/build" {
		t.Fatalf("reason = %q, want %q", reason, workflowApprovalUntrustedPrefix+"ci/build")
	}
	if len(comments) != 1 || strings.Contains(comments[0], "\n"+workflowApprovalCommand+"\n") || !strings.HasPrefix(comments[0], workflowApprovalRejectedMarker) {
		t.Fatalf("comments = %q, want only the needs-human note and no %q", comments, workflowApprovalCommand)
	}
	if !strings.Contains(comments[0], "is not one") {
		t.Fatalf("note = %q, want the untrusted-commenter wording when Prow has not replied yet", comments[0])
	}
}

// A Prow-managed repository that carries the hive-ok-to-test workflow still
// gets the command: the repository's own GITHUB_TOKEN approves the runs on
// it whatever Prow replies.
func TestTrySweepSelfAuthoredPRPostsOkToTestOnProwRepoWithWorkflow(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.okToTestWorkflowStatus = http.StatusOK
		p.existingComments = &[]map[string]any{
			prowComment(1, "[APPROVALNOTIFIER] This PR is **NOT APPROVED**"),
			prowComment(2, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
		}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("reason = %q, want %q", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	if len(comments) != 1 || comments[0] != workflowApprovalCommandBody("sha23") {
		t.Fatalf("comments = %q, want one %q command", comments, workflowApprovalCommand)
	}
}

// The once-per-head marker survives a restart: a thread that already carries
// the App's command for this head gets no second post from a fresh process.
func TestTrySweepSelfAuthoredPRDoesNotRepostOkToTestForSameHeadAfterRestart(t *testing.T) {
	var comments []string
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.existingComments = &[]map[string]any{appComment(1, workflowApprovalCommandBody("SHA23"))}
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("reason = %q, want %q", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	if len(comments) != 0 {
		t.Fatalf("comments = %q, want no second %q for the same head", comments, workflowApprovalCommand)
	}
}

// Prow replies seconds after the command, so the rejection must be noticed
// on a later sweep of the SAME head, not only when a new head arrives. The
// re-read is rate-limited to workflowApprovalRecheck.
func TestTrySweepSelfAuthoredPRNoticesProwRejectionOnLaterTick(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	var comments []string
	thread := &[]map[string]any{}
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.commentBodies = &comments
		p.existingComments = thread
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.now = func() time.Time { return now }

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("first tick returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("first reason = %q, want %q", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	if len(comments) != 1 {
		t.Fatalf("comments after first tick = %q, want the command", comments)
	}
	// Prow replies; a tick inside the recheck window does not re-read.
	*thread = []map[string]any{
		appComment(1, comments[0]),
		prowComment(2, "@"+testHiveAppBotLogin+": Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message."),
	}
	now = now.Add(time.Minute)
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("second tick returned error: %v", err)
	}
	if reason != workflowApprovalRequiredPrefix+"ci/build" {
		t.Fatalf("second reason = %q, want %q (inside recheck window)", reason, workflowApprovalRequiredPrefix+"ci/build")
	}
	now = now.Add(workflowApprovalRecheck)
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("third tick returned error: %v", err)
	}
	if reason != workflowApprovalUntrustedPrefix+"ci/build" {
		t.Fatalf("third reason = %q, want %q", reason, workflowApprovalUntrustedPrefix+"ci/build")
	}
	if len(comments) != 2 || !strings.HasPrefix(comments[1], workflowApprovalRejectedMarker) {
		t.Fatalf("comments = %q, want the command then one needs-human note", comments)
	}
}

func TestWorkflowApprovalUntrustedReasonParksAtNeedsDecision(t *testing.T) {
	reason := workflowApprovalUntrustedPrefix + "Tap CI"
	v := classifyStage(stageObservation{Reason: reason, Lane: stageLaneSelf})
	if v.Stage != stagelabels.NeedsDecision || !v.Human {
		t.Fatalf("classifyStage(%q) = %+v, want 085 needs-decision for a person", reason, v)
	}
	cause := decisionCause(reason)
	if !strings.Contains(cause, "Prow") || !strings.Contains(cause, workflowApprovalCommand) || !strings.Contains(cause, oktotest.WorkflowPath) {
		t.Fatalf("decisionCause(%q) = %q, want Prow, the command and the workflow path named", reason, cause)
	}
}
