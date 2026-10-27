package automerge

// Issue-author credit at merge time (hivecommons/hive#14581): the sweep's
// merge request carries `Co-authored-by: <issue author>` for the humans who
// opened the issues the PR closes, the branch's own trailers are kept, a
// trailer the branch already carries is not duplicated, and the switch
// turns the whole thing off.

import (
	"context"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestContributorLaneMergeCreditsIssueAuthor(t *testing.T) {
	fx := validContributorFixture()
	fx.issueAuthor, fx.issueAuthorID = "Danathar", 12345
	fx.commitMessages = []string{
		"Fix the widget\n\nSigned-off-by: " + contributorLaneTestAuthor + " <" + contributorLaneTestAuthor + "@example.com>",
		"Address review\n\nSigned-off-by: " + contributorLaneTestAuthor + " <" + contributorLaneTestAuthor + "@example.com>",
	}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want merge", reason, err)
	}
	if fx.merges != 1 {
		t.Fatalf("merges = %d, want 1", fx.merges)
	}
	got, _ := fx.mergeBody["commit_message"].(string)
	want := "* Fix the widget\n\nSigned-off-by: " + contributorLaneTestAuthor + " <" + contributorLaneTestAuthor + "@example.com>\n\n" +
		"* Address review\n\nSigned-off-by: " + contributorLaneTestAuthor + " <" + contributorLaneTestAuthor + "@example.com>\n\n" +
		"Co-authored-by: Danathar <12345+Danathar@users.noreply.github.com>"
	if got != want {
		t.Fatalf("squash commit_message =\n%s\nwant\n%s", got, want)
	}
	if fx.mergeBody["merge_method"] != "squash" || fx.mergeBody["sha"] != "sha7" {
		t.Fatalf("merge request lost its method/sha: %+v", fx.mergeBody)
	}
	if strings.Count(got, "Signed-off-by:") != 2 {
		t.Fatalf("branch sign-offs not preserved:\n%s", got)
	}
	if fx.issueGets != 1 || fx.commitLists != 1 {
		t.Fatalf("issue GETs = %d, commit lists = %d; want one each", fx.issueGets, fx.commitLists)
	}
}

func TestContributorLaneMergeCreditOff(t *testing.T) {
	fx := validContributorFixture()
	fx.issueAuthor, fx.issueAuthorID = "Danathar", 12345
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	engine.transport.(*hgithub.Client).SetCreditIssueAuthors(func(string) bool { return false })
	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want merge", reason, err)
	}
	if _, has := fx.mergeBody["commit_message"]; has || fx.merges != 1 {
		t.Fatalf("credit off: merge body %+v (merges %d), want GitHub's default message", fx.mergeBody, fx.merges)
	}
	// The one issue GET is the linked-issue gate's (hold/parked check); credit
	// off must add no read of its own and never list the commits.
	if fx.issueGets != 1 || fx.commitLists != 0 {
		t.Fatalf("credit off: issue GETs = %d (want 1, the linked-issue gate), commits = %d (want 0)", fx.issueGets, fx.commitLists)
	}
}

// A PR author who filed their own issue is not their own co-author, and the
// merge then sends no message at all (no commit read either).
func TestContributorLaneMergeCreditSkipsSelf(t *testing.T) {
	fx := validContributorFixture()
	fx.issueAuthor = contributorLaneTestAuthor
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want merge", reason, err)
	}
	if _, has := fx.mergeBody["commit_message"]; has || fx.commitLists != 0 {
		t.Fatalf("self-filed issue: merge body %+v (commit lists %d)", fx.mergeBody, fx.commitLists)
	}
}

// The self-authored lane: a hive-opened PR already carries the trailer from
// issue-coauthor.sh, so the merge adds nothing; a hive PR whose branch lacks
// it gains it.
func TestSelfAuthoredSweepMergeCredit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		commits []string
		want    string
	}{
		{
			name:    "branch already credits the issue author",
			commits: []string{"Fix\n\nSigned-off-by: hive[bot] <hive@example.com>\nCo-authored-by: Dana Thar <12345+Danathar@users.noreply.github.com>"},
		},
		{
			name:    "branch lacks the trailer",
			commits: []string{"Fix\n\nSigned-off-by: hive[bot] <hive@example.com>"},
			want:    "Signed-off-by: hive[bot] <hive@example.com>\n\nCo-authored-by: Danathar <12345+Danathar@users.noreply.github.com>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			var mergeBody map[string]any
			fx := selfSweepFixture{
				body:           testIntentLinkedBody,
				mergeApplied:   true,
				mergeCalls:     &merges,
				issueAuthor:    "Danathar",
				issueAuthorID:  12345,
				commitMessages: tc.commits,
				mergeBody:      &mergeBody,
			}
			api := newSelfSweepGuardAPI(t, fx)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" {
				t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want merge", reason, err)
			}
			got, _ := mergeBody["commit_message"].(string)
			if merges != 1 || got != tc.want {
				t.Fatalf("merges = %d, commit_message = %q, want %q", merges, got, tc.want)
			}
		})
	}
}
