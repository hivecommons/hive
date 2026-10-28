package automerge

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func newIssueComment(login, body string, at time.Time) *gh.IssueComment {
	return &gh.IssueComment{User: &gh.User{Login: gh.Ptr(login)}, Body: gh.Ptr(body), CreatedAt: &gh.Timestamp{Time: at}}
}

func ghErrorResponse(status int, msg string) error {
	return &gh.ErrorResponse{Response: &http.Response{StatusCode: status, Request: &http.Request{Method: "PUT", URL: &url.URL{Path: "/x"}}}, Message: msg}
}

// workflowPathFiles is a contributor-lane diff that trips the workflow-path
// fence.
func workflowPathFiles() []map[string]any {
	return []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 3, "deletions": 1}}
}

func ownerApprovedReview(login, commit string) map[string]any {
	return map[string]any{"id": 1, "state": "APPROVED", "commit_id": commit, "user": map[string]string{"login": login}}
}

// dashboardApproveComment is the `/approve` the dashboard posts as the App on
// a maintainer's behalf (hgithub.RecordOperatorPullRequestApproval), with
// the head marker when head is non-empty.
func dashboardApproveComment(id int, actor, head, createdAt string) map[string]any {
	body := "/approve\n\nPosted by @" + actor + " from the Hive dashboard; the merge gate decides from here."
	if head != "" {
		body += "\n<!-- hive:pr-approval head=" + head + " by=" + actor + " -->"
	}
	c := map[string]any{"id": id, "user": map[string]string{"login": testHiveAppBotLogin}, "body": body}
	if createdAt != "" {
		c["created_at"] = createdAt
	}
	return c
}

// Owner approval on the current head releases the workflow-path fence and the
// lane merges the PR itself; anything short of that leaves the fence as it
// was (hivecommons/hive#14664).
func TestContributorLaneWorkflowFenceOwnerApproval(t *testing.T) {
	const clockAt = "2026-10-27T13:00:00Z"
	for _, tc := range []struct {
		name       string
		toggleOff  bool
		mutate     func(fx *contributorLaneFixture)
		trusted    []string
		wantReason string
		wantMerges int
		// wantMarker expects the one-per-head audit marker naming by.
		wantMarker string
	}{
		{
			name: "APPROVED review by trusted merger on current head releases",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{ownerApprovedReview("clubanderson", "sha7")}
			},
			trusted:    []string{"clubanderson"},
			wantMerges: 1,
			wantMarker: "<!-- hive:workflow-approval head=sha7 by=clubanderson -->",
		},
		{
			name: "APPROVED review by non-owner does not release",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{ownerApprovedReview("passerby", "sha7")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "APPROVED review on a stale head does not release",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{ownerApprovedReview("clubanderson", "sha6")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "dashboard decision approve bound to the head by marker releases",
			mutate: func(fx *contributorLaneFixture) {
				fx.prComments = []map[string]any{dashboardApproveComment(5, "clubanderson", "sha7", "")}
			},
			trusted:    []string{"clubanderson"},
			wantMerges: 1,
			wantMarker: "<!-- hive:workflow-approval head=sha7 by=clubanderson -->",
		},
		{
			name: "dashboard decision approve marked for an older head does not release",
			mutate: func(fx *contributorLaneFixture) {
				fx.prComments = []map[string]any{dashboardApproveComment(5, "clubanderson", "sha6", "")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "unmarked dashboard approve posted after the head push releases",
			mutate: func(fx *contributorLaneFixture) {
				fx.headCommittedAt = "2026-10-27T11:00:00Z"
				fx.prComments = []map[string]any{dashboardApproveComment(5, "clubanderson", "", "2026-10-27T12:00:00Z")}
			},
			trusted:    []string{"clubanderson"},
			wantMerges: 1,
			wantMarker: "<!-- hive:workflow-approval head=sha7 by=clubanderson -->",
		},
		{
			name: "unmarked dashboard approve posted before the head push does not release",
			mutate: func(fx *contributorLaneFixture) {
				fx.headCommittedAt = "2026-10-27T11:00:00Z"
				fx.prComments = []map[string]any{dashboardApproveComment(5, "clubanderson", "", "2026-10-27T10:00:00Z")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "dashboard approve for a non-owner does not release",
			mutate: func(fx *contributorLaneFixture) {
				fx.prComments = []map[string]any{dashboardApproveComment(5, "passerby", "sha7", "")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "typed /hive decision approve by trusted merger after the push releases",
			mutate: func(fx *contributorLaneFixture) {
				fx.headCommittedAt = "2026-10-27T11:00:00Z"
				fx.prComments = []map[string]any{{"id": 5, "user": map[string]string{"login": "clubanderson"}, "body": "/hive decision approve\n\nShip it.", "created_at": "2026-10-27T12:00:00Z"}}
			},
			trusted:    []string{"clubanderson"},
			wantMerges: 1,
			wantMarker: "<!-- hive:workflow-approval head=sha7 by=clubanderson -->",
		},
		{
			name: "plain /hive approve is not consumed",
			mutate: func(fx *contributorLaneFixture) {
				fx.headCommittedAt = "2026-10-27T11:00:00Z"
				fx.prComments = []map[string]any{{"id": 5, "user": map[string]string{"login": "clubanderson"}, "body": "/hive approve", "created_at": "2026-10-27T12:00:00Z"}}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:      "toggle off keeps the fence absolute",
			toggleOff: true,
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{ownerApprovedReview("clubanderson", "sha7")}
			},
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "no approval behaves as before",
			trusted:    []string{"clubanderson"},
			wantReason: contributorLaneReasonWorkflowPath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.files = workflowPathFiles()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.OwnerApprovalReleasesWorkflowFence = !tc.toggleOff
			clock := &contributorLaneClock{at: mustParseTime(t, clockAt)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			engine.SetMergerAuthorizer(func(login string) bool {
				return slices.ContainsFunc(tc.trusted, func(l string) bool { return strings.EqualFold(l, login) })
			})
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d (comments=%v)", reason, fx.merges, tc.wantReason, tc.wantMerges, fx.allComments)
			}
			markers := 0
			for _, body := range fx.allComments {
				if strings.HasPrefix(body, workflowApprovalMarkerPrefix) {
					markers++
					if tc.wantMarker == "" || !strings.HasPrefix(body, tc.wantMarker) {
						t.Fatalf("audit comment = %q, want prefix %q", body, tc.wantMarker)
					}
				}
			}
			if want := 0; tc.wantMarker != "" {
				want = 1
				if markers != want {
					t.Fatalf("workflow-approval markers = %d, want %d: %v", markers, want, fx.allComments)
				}
			} else if markers != 0 {
				t.Fatalf("unexpected workflow-approval marker: %v", fx.allComments)
			}
		})
	}
}

// The audit marker is left once per head: a second tick on a head already
// marked merges without a second comment.
func TestContributorLaneWorkflowFenceMarkerOncePerHead(t *testing.T) {
	fx := validContributorFixture()
	fx.files = workflowPathFiles()
	fx.reviews = []map[string]any{ownerApprovedReview("clubanderson", "sha7")}
	fx.prComments = []map[string]any{{"id": 5, "user": map[string]string{"login": testHiveAppBotLogin}, "body": workflowApprovalComment("sha7", "clubanderson")}}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.OwnerApprovalReleasesWorkflowFence = true
	clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || fx.merges != 1 {
		t.Fatalf("reason=%q merges=%d, want merged", reason, fx.merges)
	}
	for _, body := range fx.allComments {
		if strings.HasPrefix(body, workflowApprovalMarkerPrefix) {
			t.Fatalf("marker re-posted for an already-marked head: %v", fx.allComments)
		}
	}
}

// A merge GitHub refuses for want of the App's workflows permission falls
// back to the pre-#14664 outcome for that head: hive:unauthorized for
// merge-workflow-path (label + one comment), the workflow-path reason so the
// stage engine parks the approved head at 090 for a person's click, and no
// second merge attempt on later ticks or after a restart that sees the marker.
func TestContributorLaneWorkflowFenceMergeUnauthorizedFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		// restart pre-seeds the unauthorized marker as a restarted hive sees it.
		restart    bool
		serialized bool
		wantMerges int
	}{
		{name: "403 resource not accessible", status: http.StatusForbidden, message: "Resource not accessible by integration", wantMerges: 1},
		{name: "422 refusing to update workflow without permission", status: http.StatusUnprocessableEntity, message: "refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission", wantMerges: 1},
		{name: "restart sees marker and does not retry", status: http.StatusForbidden, message: "Resource not accessible by integration", restart: true, wantMerges: 0},
		{name: "serialized 403 resource not accessible", status: http.StatusForbidden, message: "Resource not accessible by integration", serialized: true, wantMerges: 1},
		{name: "serialized 422 workflow permission", status: http.StatusUnprocessableEntity, message: "refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission", serialized: true, wantMerges: 1},
		{name: "serialized restart sees marker", status: http.StatusForbidden, message: "Resource not accessible by integration", serialized: true, restart: true, wantMerges: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.files = workflowPathFiles()
			fx.reviews = []map[string]any{ownerApprovedReview("clubanderson", "sha7")}
			fx.mergeStatus = tc.status
			fx.mergeMessage = tc.message
			if tc.restart {
				fx.labels = []string{unauthorizedLabel}
				fx.prComments = []map[string]any{{
					"id":   5,
					"user": map[string]string{"login": testHiveAppBotLogin},
					"body": unauthorizedComment("sha7", unauthorizedStepMergeWorkflowPath, unauthorizedPermissionWorkflowsWrite, "click"),
				}}
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.OwnerApprovalReleasesWorkflowFence = true
			clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			if tc.serialized {
				gate := func(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
					if req.FrontOnly {
						return hgithub.LaneMergeResult{Outcome: hgithub.LaneOutcomeLeft}, nil
					}
					if err := req.Authorize(ctx, "sha7"); err != nil {
						t.Fatalf("lane authorization: %v", err)
					}
					_, _, mergeErr := engine.gh.PullRequests.Merge(ctx, "acme", "widget", 7, "", &gh.PullRequestOptions{SHA: "sha7", MergeMethod: req.Method})
					if mergeErr == nil {
						t.Fatal("expected a workflow permission merge failure")
					}
					return hgithub.LaneMergeResult{Outcome: hgithub.LaneOutcomeWaiting, Reason: "merge call failed", MergeError: mergeErr}, nil
				}
				engine = newContributorLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, gate, clock, Options{})
			}
			engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })

			for tick := 1; tick <= 2; tick++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil {
					t.Fatalf("tick %d err = %v", tick, err)
				}
				if reason != contributorLaneReasonWorkflowPath {
					t.Fatalf("tick %d reason = %q, want %q", tick, reason, contributorLaneReasonWorkflowPath)
				}
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merge attempts = %d, want %d across two ticks", fx.merges, tc.wantMerges)
			}
			if step := engine.unauthorizedStep("acme", "widget", 7, "sha7"); step != unauthorizedStepMergeWorkflowPath {
				t.Fatalf("unauthorizedStep = %q, want %q", step, unauthorizedStepMergeWorkflowPath)
			}
			if !slices.Contains(fx.labels, unauthorizedLabel) {
				t.Fatalf("labels = %v, want %q", fx.labels, unauthorizedLabel)
			}
			marker := unauthorizedMarker("sha7", unauthorizedStepMergeWorkflowPath)
			unauthorizedComments := 0
			for _, body := range fx.allComments {
				if strings.HasPrefix(body, marker) {
					unauthorizedComments++
					if !strings.Contains(body, "`"+unauthorizedPermissionWorkflowsWrite+"`") || !strings.Contains(body, "**Merge**") {
						t.Fatalf("unauthorized comment = %q, want the permission and the merge click", body)
					}
				}
			}
			wantAdds, wantComments := 1, 1
			if tc.restart {
				wantAdds, wantComments = 0, 0
			}
			if unauthorizedComments != wantComments || len(fx.labelsAdded) != wantAdds {
				t.Fatalf("unauthorized comments = %d labels added = %v, want %d/%d", unauthorizedComments, fx.labelsAdded, wantComments, wantAdds)
			}
			// The stage route for this head now reads as a manual merge.
			if !engine.workflowFenceManual("acme", "widget", 7, "sha7") {
				t.Fatal("workflowFenceManual = false after an unauthorized merge, want true")
			}
		})
	}
}

// The 085 text for an unapproved workflow-path PR asks for owner approval
// when the release is on and reads as a manual-merge rule when it is off.
func TestWorkflowPathCauseText(t *testing.T) {
	if got := workflowPathCause(false); !strings.Contains(got, "awaiting owner approval") || !strings.Contains(got, "re-approve after a push") {
		t.Fatalf("released cause = %q", got)
	}
	if got := workflowPathCause(true); !strings.Contains(got, "never merges unattended") {
		t.Fatalf("manual cause = %q", got)
	}
	if manualMergeRuleReasonFor(contributorLaneReasonWorkflowPath, false) || !manualMergeRuleReasonFor(contributorLaneReasonWorkflowPath, true) {
		t.Fatal("manualMergeRuleReasonFor must follow the workflow fence's standing")
	}
	if !manualMergeRuleReasonFor(contributorLaneReasonTestsRemoved, false) {
		t.Fatal("tests-removed stays a manual-merge rule")
	}
}

// workflowApprovalCommentActor reads dashboard and typed approvals, binds
// them to the head, and ignores everything else.
func TestWorkflowApprovalCommentActor(t *testing.T) {
	pushed := mustParseTime(t, "2026-10-27T11:00:00Z")
	after := mustParseTime(t, "2026-10-27T12:00:00Z")
	before := mustParseTime(t, "2026-10-27T10:00:00Z")
	comment := newIssueComment
	dashboard := func(actor, head string) string {
		b := "/approve\n\nPosted by @" + actor + " from the Hive dashboard; the merge gate decides from here."
		if head != "" {
			b += "\n<!-- hive:pr-approval head=" + head + " by=" + actor + " -->"
		}
		return b
	}
	for _, tc := range []struct {
		name      string
		comment   *gh.IssueComment
		wantLogin string
		wantOK    bool
	}{
		{name: "dashboard approve marked for head", comment: comment(testHiveAppBotLogin, dashboard("clubanderson", "sha7"), before), wantLogin: "clubanderson", wantOK: true},
		{name: "dashboard approve marked for other head", comment: comment(testHiveAppBotLogin, dashboard("clubanderson", "sha6"), after)},
		{name: "dashboard approve unmarked after push", comment: comment(testHiveAppBotLogin, dashboard("clubanderson", ""), after), wantLogin: "clubanderson", wantOK: true},
		{name: "dashboard approve unmarked before push", comment: comment(testHiveAppBotLogin, dashboard("clubanderson", ""), before)},
		{name: "dashboard lgtm is not the approval", comment: comment(testHiveAppBotLogin, "/lgtm\n\nPosted by @clubanderson from the Hive dashboard; the merge gate decides from here.", after)},
		{name: "dashboard-shaped comment from a person is not trusted as the App", comment: comment("passerby", dashboard("clubanderson", "sha7"), after)},
		{name: "typed decision approve after push", comment: comment("clubanderson", "/hive decision approve\n\nfine", after), wantLogin: "clubanderson", wantOK: true},
		{name: "typed decision approve before push", comment: comment("clubanderson", "/hive decision approve", before)},
		{name: "quoted decision approve", comment: comment("clubanderson", "> /hive decision approve", after)},
		{name: "plain hive approve", comment: comment("clubanderson", "/hive approve", after)},
		{name: "decision with other text", comment: comment("clubanderson", "/hive decision approve the rename later", after)},
		{name: "bot typed decision", comment: comment("someone[bot]", "/hive decision approve", after)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login, ok := workflowApprovalCommentActor(tc.comment, "sha7", pushed, testHiveAppBotLogin)
			if ok != tc.wantOK || login != tc.wantLogin {
				t.Fatalf("got (%q, %v), want (%q, %v)", login, ok, tc.wantLogin, tc.wantOK)
			}
		})
	}
	if _, ok := workflowApprovalCommentActor(comment("clubanderson", "/hive decision approve", after), "sha7", time.Time{}, testHiveAppBotLogin); ok {
		t.Fatal("an unmarked comment must not bind to a head whose push time is unknown")
	}
}

// isWorkflowsPermissionMergeError recognises the two shapes GitHub uses and
// nothing else.
func TestIsWorkflowsPermissionMergeError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		want    bool
	}{
		{name: "403 not accessible", status: http.StatusForbidden, message: "Resource not accessible by integration", want: true},
		{name: "422 workflows permission", status: http.StatusUnprocessableEntity, message: "refusing to allow a GitHub App to create or update workflow without `workflows` permission", want: true},
		{name: "403 workflow permission", status: http.StatusForbidden, message: "You need the workflows permission", want: true},
		{name: "405 not mergeable", status: http.StatusMethodNotAllowed, message: "Pull Request is not mergeable", want: false},
		{name: "422 head mismatch", status: http.StatusUnprocessableEntity, message: "Head branch was modified. Review and try the merge again.", want: false},
		{name: "403 rate limit", status: http.StatusForbidden, message: "API rate limit exceeded", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkflowsPermissionMergeError(ghErrorResponse(tc.status, tc.message)); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if isWorkflowsPermissionMergeError(nil) || isWorkflowsPermissionMergeError(context.Canceled) {
		t.Fatal("non-GitHub errors are not permission errors")
	}
}

// ParsePRApprovalCommandComment round-trips what the dashboard posts.
func TestParsePRApprovalCommandCommentRoundTrip(t *testing.T) {
	command, actor, head, ok := hgithub.ParsePRApprovalCommandComment("/approve\n\nPosted by @ClubAnderson from the Hive dashboard; the merge gate decides from here.\n<!-- hive:pr-approval head=ABC123 by=clubanderson -->")
	if !ok || command != "/approve" || actor != "ClubAnderson" || head != "abc123" {
		t.Fatalf("got (%q, %q, %q, %v)", command, actor, head, ok)
	}
	if _, _, _, ok := hgithub.ParsePRApprovalCommandComment("/approve\n\nlooks good"); ok {
		t.Fatal("a bare /approve is not a dashboard approval")
	}
}
