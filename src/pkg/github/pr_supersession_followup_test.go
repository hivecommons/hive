package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// supersessionFixtureCloserMergedAt is when supersessionSweepServer's GraphQL
// closer merged.
var supersessionFixtureCloserMergedAt = time.Date(2026, 10, 10, 11, 40, 0, 0, time.UTC)

// Danathar/zfs-aurora-complex#313 removed a pin that #300 had added. Its
// body said "the pin PR is #300 (it closed issue #299)"; the parser read
// "closed issue #299" as a closing claim, #300 resolved as the closer, the
// two PRs touched the same files, and the sweep closed live follow-up work
// as a duplicate six minutes after it opened. A PR opened after the closer
// merged that names the closer is not superseded by it; the racing duplicate
// (opened before the merge, or not naming the closer) still is.
func TestSweepSupersededOpenPRs_FollowUpNamingTheMergedCloser(t *testing.T) {
	org, repo := "hivecommons", "hive"
	after := supersessionFixtureCloserMergedAt.Add(4 * time.Hour)
	before := supersessionFixtureCloserMergedAt.Add(-time.Hour)
	pinBody := "This sets the field back and reverts the other files #300 changed for the pin. " +
		"Note: the pin PR is #300 (it closed issue #299); there is no PR #299 in this repo."

	for _, tc := range []struct {
		name      string
		author    string
		body      string
		createdAt time.Time
		// wantActed: the sweep still treats the PR as superseded (closes the
		// hive's own, writes the notice on a contributor's).
		wantActed bool
	}{
		{name: "hive follow-up opened after the closer merged and naming it", author: "hive-app[bot]", body: pinBody, createdAt: after},
		{name: "hive follow-up that references the closed issue and names the closer", author: "hive-app[bot]", body: "Follow-up to #300.\n\nRefs #299", createdAt: after},
		{name: "contributor follow-up naming the closer", author: "contributor", body: "Builds on #300.\n\nFixes #299", createdAt: after},
		{name: "same body opened before the closer merged is a competing fix", author: "hive-app[bot]", body: pinBody, createdAt: before, wantActed: true},
		{name: "opened after the closer merged without naming it lost the race", author: "hive-app[bot]", body: "Fixes #299", createdAt: after, wantActed: true},
		{name: "no creation time in the listing keeps today's answer", author: "hive-app[bot]", body: pinBody, wantActed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{"ci/defaults.json", "docs/pin.md"}
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 313, Title: "ci: follow the latest shared build tooling again", Body: tc.body, Author: tc.author, Files: files, CreatedAt: tc.createdAt},
					{Number: 300, Title: "ci: pin the build tooling", Body: "Closes #299", Author: "hive-app[bot]", Merged: true, Files: files},
				},
				[]supersessionIssueFixture{{Number: 299, State: "closed", GraphQLCloserPR: 300}},
			)
			defer server.Close()

			c, rec := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			acted := res.Actions() > 0 || len(obs.closed) > 0 || len(obs.comments[313]) > 0 || len(obs.labels[313]) > 0 || len(parkedPRs(rec)) > 0
			if acted != tc.wantActed {
				t.Fatalf("acted = %v, want %v: result=%+v closed=%v comments=%v labels=%v parked=%v",
					acted, tc.wantActed, res, obs.closed, obs.comments[313], obs.labels[313], parkedPRs(rec))
			}
			if tc.wantActed && tc.author == "hive-app[bot]" && (len(obs.closed) != 1 || obs.closed[0] != 313) {
				t.Fatalf("closed = %v, want the hive's own superseded PR #313 closed as before", obs.closed)
			}
		})
	}
}

func TestSupersessionFollowsCloser(t *testing.T) {
	c := &Client{org: "o"}
	merged := supersessionFixtureCloserMergedAt
	closer := supersessionCloser{Repo: "o/r", Number: 300, MergedAt: merged}
	pr := func(title, body string, created time.Time) *gh.PullRequest {
		p := &gh.PullRequest{Number: gh.Ptr(313), Title: gh.Ptr(title), Body: gh.Ptr(body)}
		if !created.IsZero() {
			p.CreatedAt = &gh.Timestamp{Time: created}
		}
		return p
	}
	after := merged.Add(time.Minute)

	for _, tc := range []struct {
		name   string
		prRepo string
		pr     *gh.PullRequest
		closer supersessionCloser
		want   bool
	}{
		{"bare number in the body", "r", pr("t", "reverts what #300 changed", after), closer, true},
		{"bare number in the title", "o/r", pr("revert #300", "", after), closer, true},
		{"owner/repo form", "r", pr("t", "see o/r#300", after), closer, true},
		{"repository compared case-insensitively", "r", pr("t", "see O/R#300", after), closer, true},
		{"pull request URL", "r", pr("t", "see https://github.com/o/r/pull/300 for the pin", after), closer, true},
		{"closer named by its bare repository", "o/r", pr("t", "after #300", after), supersessionCloser{Repo: "r", Number: 300, MergedAt: merged}, true},
		{"another repository's number", "r", pr("t", "see other/r#300", after), closer, false},
		{"another repository's pull request URL", "r", pr("t", "see https://github.com/o/other/pull/300", after), closer, false},
		{"a longer number", "r", pr("t", "see #3000 and https://github.com/o/r/pull/3000", after), closer, false},
		{"a shorter number", "r", pr("t", "see #30", after), closer, false},
		{"an HTML numeric entity", "r", pr("t", "by @&#300;user", after), closer, false},
		{"closer not named", "r", pr("t", "Fixes #299", after), closer, false},
		{"opened before the closer merged", "r", pr("t", "alternative to #300", merged.Add(-time.Minute)), closer, false},
		{"opened at the merge instant", "r", pr("t", "after #300", merged), closer, false},
		{"no creation time", "r", pr("t", "after #300", time.Time{}), closer, false},
		{"no merge time on the closer", "r", pr("t", "after #300", after), supersessionCloser{Repo: "o/r", Number: 300}, false},
		{"no closer", "r", pr("t", "after #300", after), supersessionCloser{}, false},
		{"nil PR", "r", nil, closer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.supersessionFollowsCloser(tc.prRepo, tc.pr, tc.closer); got != tc.want {
				t.Fatalf("supersessionFollowsCloser = %v, want %v", got, tc.want)
			}
		})
	}
}

// Danathar/zfs-aurora-complex#313, second half: the sweep closed it, the
// scanner reopened it with the body corrected, and it was reviewed, approved
// and merged twelve hours later still wearing `hive/superseded` under a
// notice that said it was being closed. An open PR the sweep does not find
// superseded loses the label and has the sweep's notice rewritten, in one
// pass; a label the sweep cannot attribute to its own notice stays.
func TestSweepSupersededOpenPRs_ReopenedPRLosesTheCloseMark(t *testing.T) {
	org, repo := "hivecommons", "hive"
	closer := supersessionCloser{Repo: org + "/" + repo, Number: 22, URL: fmt.Sprintf("https://github.com/%s/%s/pull/22", org, repo)}
	closeNotice := renderSupersessionComment(
		SupersessionSweepEvent{Repo: repo, Number: 12, Author: "hive-app[bot]", IssueRepo: repo, Issue: 3, CloserPR: 22}, closer,
		"Closing this hive-authored PR because the claimed issue was already closed by the merged PR below.")
	notice := func(body string) []supersessionCommentFixture {
		return []supersessionCommentFixture{{ID: 800, Body: body, Author: "hive-app[bot]", Bot: true, CreatedAt: time.Now().Add(-12 * time.Hour)}}
	}

	for _, tc := range []struct {
		name       string
		author     string
		body       string
		issueState string
		labels     []string
		comments   []supersessionCommentFixture
		wantClear  bool
		wantClosed bool
		wantEdit   bool
	}{
		{
			name: "reopened with the claim rewritten away", author: "hive-app[bot]",
			body: "Removes the pin. The pin's issue was already closed.", issueState: "closed",
			labels: []string{SupersededLabel}, comments: notice(closeNotice),
			wantClear: true, wantEdit: true,
		},
		{
			name: "reopened after the claimed issue was reopened", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "open",
			labels: []string{SupersededLabel}, comments: notice(closeNotice),
			wantClear: true, wantEdit: true,
		},
		{
			name: "a contributor's reopened PR", author: "contributor",
			body: "Fixes #3", issueState: "open",
			labels: []string{SupersededLabel}, comments: notice(closeNotice),
			wantClear: true, wantEdit: true,
		},
		{
			name: "notice already rewritten, label removal retried", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "open",
			labels: []string{SupersededLabel}, comments: notice(renderSupersessionRetractedComment(closeNotice)),
			wantClear: true,
		},
		{
			name: "label with no sweep notice is another closer's and stays", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "open",
			labels: []string{SupersededLabel},
		},
		{
			name: "no label, nothing to take back", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "open",
			comments: notice(closeNotice),
		},
		{
			name: "held PR is left alone", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "open",
			labels: []string{SupersededLabel, "hold"}, comments: notice(closeNotice),
		},
		{
			name: "reopened but still superseded is closed again", author: "hive-app[bot]",
			body: "Fixes #3", issueState: "closed",
			labels: []string{SupersededLabel}, comments: notice(closeNotice),
			wantClosed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 12, Title: "remove the pin", Body: tc.body, Author: tc.author, Files: []string{"pkg/a.go"}, Labels: tc.labels, Comments: tc.comments},
					{Number: 22, Title: "landed", Body: "Fixes #3", Author: "hive-app[bot]", Merged: true, Files: []string{"pkg/a.go"}},
				},
				[]supersessionIssueFixture{{Number: 3, State: tc.issueState, GraphQLCloserPR: 22}},
			)
			defer server.Close()

			c, rec := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			removed := obs.removedLabels[12]
			if tc.wantClear {
				if len(removed) != 1 || removed[0] != SupersededLabel {
					t.Fatalf("removed labels = %v, want [%s]", removed, SupersededLabel)
				}
				if len(res.Resolved) != 1 || res.Resolved[0].Action != SupersessionActionClearedSuperseded || res.Resolved[0].Number != 12 {
					t.Fatalf("result = %+v, want one %q for #12", res, SupersessionActionClearedSuperseded)
				}
			} else if len(removed) != 0 {
				t.Fatalf("removed labels = %v, want none", removed)
			}
			edited, ok := obs.edits[800]
			if !tc.wantClosed && ok != tc.wantEdit {
				t.Fatalf("notice edited = %v (%q), want %v", ok, edited, tc.wantEdit)
			}
			if tc.wantEdit {
				for _, want := range []string{supersessionSweepMarker, supersessionRetractedPhrase, "`" + SupersededLabel + "` label has been removed", "- Claimed issue: hive#3", "- Closing PR: [#22]"} {
					if !strings.Contains(edited, want) {
						t.Errorf("rewritten notice lacks %q:\n%s", want, edited)
					}
				}
				for _, stale := range []string{"Closing this hive-authored PR", supersessionEscalationMark} {
					if strings.Contains(edited, stale) {
						t.Errorf("rewritten notice still says %q:\n%s", stale, edited)
					}
				}
			}
			if closed := len(obs.closed) == 1 && obs.closed[0] == 12; closed != tc.wantClosed {
				t.Fatalf("closed = %v, want closed=%v", obs.closed, tc.wantClosed)
			}
			if len(obs.comments[12]) != 0 || len(parkedPRs(rec)) != 0 {
				t.Fatalf("sweep wrote too much: comments=%v parked=%v", obs.comments[12], parkedPRs(rec))
			}
		})
	}
}

func TestRenderSupersessionRetractedComment(t *testing.T) {
	closer := supersessionCloser{Repo: "o/r", Number: 22, URL: "https://github.com/o/r/pull/22"}
	earlier := renderSupersessionComment(SupersessionSweepEvent{Repo: "r", Number: 12, IssueRepo: "r", Issue: 3, CloserPR: 22}, closer, "Closing this hive-authored PR because the claimed issue was already closed by the merged PR below.")
	got := renderSupersessionRetractedComment(earlier)
	if again := renderSupersessionRetractedComment(got); again != got {
		t.Fatalf("rendering the rewritten notice again changed it:\n%s\n---\n%s", got, again)
	}
	if !strings.HasPrefix(got, supersessionSweepMarker+"\n") {
		t.Fatalf("rewritten notice does not open with the sweep marker:\n%s", got)
	}
	bare := renderSupersessionRetractedComment(supersessionSweepMarker + "\nsomething else\n")
	if strings.Contains(bare, "The earlier notice named") || !strings.Contains(bare, supersessionRetractedPhrase) {
		t.Fatalf("notice with no issue or closer line rendered wrongly:\n%s", bare)
	}
}
