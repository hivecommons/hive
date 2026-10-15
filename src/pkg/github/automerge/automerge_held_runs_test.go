package automerge

// Held / never-started required checks on trusted-lane PRs (#11552, #11553,
// #11877). These pin that the self-authored sweep releases workflow runs
// GitHub holds for approval on a same-repo head exactly once, never on a fork
// head, degrades to a held reason when the App lacks Actions: write, and
// names a head whose runs were never created instead of polling it forever.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
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

func TestTrySweepSelfAuthoredPRApprovesHeldWorkflowRunsOnce(t *testing.T) {
	approves := 0
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget"), heldRun(2, "acme/widget")}
		p.approveCount = &approves
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	rec := &recordingBoundary{}
	c.mutation = rec

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != approvedHeldWorkflowRunsReason {
		t.Fatalf("reason = %q, want %q", reason, approvedHeldWorkflowRunsReason)
	}
	if approves != 2 {
		t.Fatalf("approve POSTs = %d, want 2 (one per held run)", approves)
	}
	if len(rec.claims) != 2 || rec.claims[0].Kind != effects.KindWorkflowRunApprove || rec.claims[0].Target != "1" || rec.claims[0].Inputs["pr"] != "23" {
		t.Fatalf("mutation claims = %#v, want two workflow_run_approve claims for PR 23", rec.claims)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want no merge on the approving tick", merged)
	}

	// Second tick on the same head: the runs are still reported held (GitHub
	// lags) — the sweep must not spend another approve call.
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("second trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != requiredCheckHeldPrefix+"ci/build" {
		t.Fatalf("second reason = %q, want %q", reason, requiredCheckHeldPrefix+"ci/build")
	}
	if approves != 2 {
		t.Fatalf("approve POSTs after second tick = %d, want still 2", approves)
	}
}

func TestTrySweepSelfAuthoredPRHeldRunsForbiddenDegradesToHeldReason(t *testing.T) {
	approves := 0
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "acme/widget")}
		p.approveStatus = 403
		p.approveCount = &approves
	})
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	for tick := 1; tick <= 2; tick++ {
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
		if err != nil {
			t.Fatalf("tick %d returned error: %v", tick, err)
		}
		if reason != requiredCheckHeldPrefix+"ci/build" {
			t.Fatalf("tick %d reason = %q, want %q", tick, reason, requiredCheckHeldPrefix+"ci/build")
		}
	}
	if approves != 1 {
		t.Fatalf("approve POSTs = %d, want exactly 1 (403 is not retried per head)", approves)
	}
}

func TestTrySweepSelfAuthoredPRNeverApprovesForkHeadRuns(t *testing.T) {
	approves := 0
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.workflowRuns = []map[string]any{heldRun(1, "mallory/widget"), {
			"id": 2, "name": "v2 CI", "status": "completed", "conclusion": "action_required",
		}}
		p.approveCount = &approves
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
	if approves != 0 {
		t.Fatalf("approve POSTs = %d, want 0 for fork / unattributed heads", approves)
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
		{"held", "required-check-held:ci/build", true},
		{"never started", "required-check-never-started:ci/build", true},
		{"missing", "required-check-missing:ci/build", false},
		{"approved", approvedHeldWorkflowRunsReason, false},
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
		{"first", 7, "abc", "required-check-held:ci/build", true},
		{"repeat tick", 7, "abc", "required-check-held:ci/build", false},
		{"sha case", 7, "ABC", "required-check-held:ci/build", false},
		{"new head", 7, "def", "required-check-held:ci/build", true},
		{"other pr", 8, "abc", "required-check-held:ci/build", true},
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
