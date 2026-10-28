package github

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// Hive self-resolution of a superseded own PR (#13799): the hive rebases,
// measures, and either keeps the PR (claim rewritten to Refs) or closes it;
// it never parks its own PR at 085 for this at L6.

const (
	resolveOrg  = "hivecommons"
	resolveRepo = "hive"
	resolveBot  = "hive-app[bot]"
	resolveHead = "83661f49aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	resolveBase = "0badcafe0000000000000000000000000000000"
)

func resolveFixtures(open supersessionPRFixture) []supersessionPRFixture {
	open.Number = 13782
	if open.Title == "" {
		open.Title = "needs-human facet"
	}
	if open.Body == "" {
		open.Body = "Adds the facet.\n\nCloses #13780"
	}
	if open.Author == "" {
		open.Author = resolveBot
	}
	if open.Files == nil {
		open.Files = []string{"changelog.d/changed-needs-human-facet.md", "pkg/dashboard/label_facets.test.js", "pkg/dashboard/label_presets.go"}
	}
	open.HeadSHA = resolveHead
	open.BaseRef = "v5"
	open.BaseSHA = resolveBase
	return []supersessionPRFixture{
		open,
		{Number: 13760, Title: "landed", Body: "Closes #13780", Author: resolveBot, Merged: true, Files: []string{"pkg/dashboard/label_presets.go", "pkg/dashboard/static/index.html"}},
	}
}

func runResolveSweep(t *testing.T, open supersessionPRFixture, level int) (*SupersessionSweepResult, *supersessionObservations, []int) {
	t.Helper()
	return runResolveSweepOpts(t, open, SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return level }})
}

func runResolveSweepOpts(t *testing.T, open supersessionPRFixture, opts SupersessionSweepOptions) (*SupersessionSweepResult, *supersessionObservations, []int) {
	t.Helper()
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, resolveFixtures(open),
		[]supersessionIssueFixture{{Number: 13780, State: "closed", GraphQLCloserPR: 13760}})
	defer server.Close()
	c, rec := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)
	res, err := c.SweepSupersededOpenPRs(context.Background(), opts)
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	return res, obs, parkedPRs(rec)
}

func assertNoEscalation(t *testing.T, obs *supersessionObservations, parked []int) {
	t.Helper()
	if len(parked) != 0 {
		t.Fatalf("hive PR must not be parked at 085: %v", parked)
	}
	for _, l := range obs.labels[13782] {
		if l == issueNeedsHumanLabel {
			t.Fatalf("hive PR must not get a bare needs-human: %v", obs.labels[13782])
		}
	}
}

func TestSupersessionResolve_BehindBaseRequestsUpdateAndReleasesOwnPark(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{
		BehindBy: 2,
		Labels:   []string{issueNeedsHumanLabel},
		LabelEvents: []supersessionLabelEventFixture{
			{Event: "labeled", Label: issueNeedsHumanLabel, Actor: resolveBot},
		},
	}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if len(res.Resolved) != 1 || res.Resolved[0].Action != SupersessionActionRebased {
		t.Fatalf("result = %+v, want one %q", res, SupersessionActionRebased)
	}
	if got := obs.updated[13782]; got != resolveHead {
		t.Fatalf("update-branch expected_head_sha = %q, want %q", got, resolveHead)
	}
	if got := obs.removedLabels[13782]; len(got) != 1 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("removed labels = %v, want the hive's own needs-human released", got)
	}
	if len(obs.closed) != 0 || obs.bodies[13782] != "" {
		t.Fatalf("rebase pass must not close or rewrite: closed=%v body=%q", obs.closed, obs.bodies[13782])
	}
	if len(obs.comments[13782]) != 1 || !strings.Contains(obs.comments[13782][0], "updating head 83661f4 onto `v5`") {
		t.Fatalf("comments = %v, want one update notice naming the head", obs.comments[13782])
	}
}

func TestSupersessionResolve_PersonParkedPRIsLeftAlone(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{
		BehindBy: 2,
		Labels:   []string{issueNeedsHumanLabel},
		LabelEvents: []supersessionLabelEventFixture{
			{Event: "labeled", Label: issueNeedsHumanLabel, Actor: "maintainer"},
		},
	}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if res.Actions() != 0 || len(obs.comments[13782]) != 0 || obs.updated[13782] != "" || len(obs.removedLabels[13782]) != 0 {
		t.Fatalf("a person's park must not be resolved over: res=%+v obs=%+v", res, obs)
	}
}

func TestSupersessionResolve_SameHeadIsIdempotent(t *testing.T) {
	first, obs1, _ := runResolveSweep(t, supersessionPRFixture{BehindBy: 2}, acmmLevelAutonomous)
	if len(first.Resolved) != 1 {
		t.Fatalf("first pass = %+v", first)
	}
	notice := obs1.comments[13782][0]
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{
		BehindBy: 2,
		Comments: []supersessionCommentFixture{{ID: 700, Body: notice, Author: resolveBot, Bot: true, CreatedAt: time.Now().Add(-time.Minute)}},
	}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if res.Actions() != 0 || obs.updated[13782] != "" || len(obs.comments[13782]) != 0 {
		t.Fatalf("second pass on the same head must not re-request the update: res=%+v updated=%q comments=%v", res, obs.updated[13782], obs.comments[13782])
	}
	if _, edited := obs.edits[700]; edited {
		t.Fatalf("unchanged notice must not be re-edited: %q", obs.edits[700])
	}
}

// autoCloseOff runs the sweep with governor.auto_close_superseded: false,
// the pre-#14965 behaviour where a conflicting hive PR is reported and left
// open for a person.
func autoCloseOff(level int) SupersessionSweepOptions {
	return SupersessionSweepOptions{
		ACMMLevelForRepo:    func(string) int { return level },
		AutoCloseSuperseded: func() bool { return false },
	}
}

func TestSupersessionResolve_UpdateRefusedIsReportedOnce(t *testing.T) {
	res, obs, parked := runResolveSweepOpts(t, supersessionPRFixture{BehindBy: 2, UpdateBranchStatus: http.StatusUnprocessableEntity}, autoCloseOff(acmmLevelAutonomous))
	assertNoEscalation(t, obs, parked)
	if len(res.Resolved) != 1 || res.Resolved[0].Action != SupersessionActionRebaseFailed {
		t.Fatalf("result = %+v, want %q", res, SupersessionActionRebaseFailed)
	}
	if len(obs.comments[13782]) != 1 {
		t.Fatalf("want exactly one notice, got %v", obs.comments[13782])
	}
	refused := obs.comments[13782][0]
	if !strings.Contains(refused, supersessionRebaseRefusedPhrase) || !strings.Contains(refused, " head 83661f4 ") {
		t.Fatalf("refusal notice does not name the refusal and head: %q", refused)
	}
	if len(obs.removedLabels[13782]) != 0 {
		t.Fatalf("a refused update must not release a park: %v", obs.removedLabels[13782])
	}
	res2, obs2, _ := runResolveSweepOpts(t, supersessionPRFixture{
		BehindBy: 2, UpdateBranchStatus: http.StatusUnprocessableEntity,
		Comments: []supersessionCommentFixture{{ID: 701, Body: refused, Author: resolveBot, Bot: true, CreatedAt: time.Now().Add(-time.Minute)}},
	}, autoCloseOff(acmmLevelAutonomous))
	if res2.Actions() != 0 || obs2.updated[13782] != "" {
		t.Fatalf("refused update must not be retried on the same head: res=%+v", res2)
	}
}

// TestSupersessionResolve_UpdateRefusedForConflictClosesHivePR is the
// hivecommons/hive#14965 case: the hive's own PR fixes an issue that merged
// #13760 already closed, and GitHub refuses the rebase because the head
// conflicts with the base. With governor.auto_close_superseded on (the
// default) the hive closes its own PR as superseded instead of parking it
// at needs-decision for a person.
func TestSupersessionResolve_UpdateRefusedForConflictClosesHivePR(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{BehindBy: 2, UpdateBranchStatus: http.StatusUnprocessableEntity}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if len(res.Closed) != 1 || res.Closed[0].Action != SupersessionActionClosedConflicting || res.Closed[0].CloserPR != 13760 {
		t.Fatalf("result = %+v, want %q by #13760", res, SupersessionActionClosedConflicting)
	}
	if len(obs.closed) != 1 || obs.closed[0] != 13782 {
		t.Fatalf("closed = %v, want the hive's own #13782", obs.closed)
	}
	if !slices.Contains(obs.labels[13782], SupersededLabel) {
		t.Fatalf("labels on #13782 = %v, want %s", obs.labels[13782], SupersededLabel)
	}
	if len(obs.comments[13782]) != 1 || !strings.Contains(obs.comments[13782][0], "#13760") {
		t.Fatalf("close comment must name the superseding PR: %v", obs.comments[13782])
	}
	if slices.Contains(obs.labels[13782], issueNeedsHumanLabel) {
		t.Fatalf("hive-authored superseded PR must not ask a person: %v", obs.labels[13782])
	}
}

func TestSupersessionResolve_CurrentHeadWithExtraFilesRewritesClaim(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{
		BehindBy: 0,
		Labels:   []string{issueNeedsHumanLabel},
		LabelEvents: []supersessionLabelEventFixture{
			{Event: "labeled", Label: issueNeedsHumanLabel, Actor: resolveBot},
		},
	}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if len(res.Resolved) != 1 || res.Resolved[0].Action != SupersessionActionResolved {
		t.Fatalf("result = %+v, want %q", res, SupersessionActionResolved)
	}
	if len(obs.closed) != 0 || obs.updated[13782] != "" {
		t.Fatalf("resolved PR must stay open and not be updated: closed=%v updated=%q", obs.closed, obs.updated[13782])
	}
	body := obs.bodies[13782]
	if strings.Contains(body, "Closes #13780") || !strings.Contains(body, "Refs #13780 (closed by #13760; this PR adds `changelog.d/changed-needs-human-facet.md`, `pkg/dashboard/label_facets.test.js`)") {
		t.Fatalf("body not rewritten: %q", body)
	}
	if !strings.Contains(body, supersessionResolvedMarker(13780, 13760)) || !strings.HasPrefix(body, "Adds the facet.") {
		t.Fatalf("body must keep its prose and carry the marker: %q", body)
	}
	if got := obs.removedLabels[13782]; len(got) != 1 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("removed labels = %v, want the hive's own needs-human released", got)
	}
	if len(obs.comments[13782]) != 1 || !strings.Contains(obs.comments[13782][0], "still adds 2 file(s) #13760 did not touch") {
		t.Fatalf("comments = %v", obs.comments[13782])
	}
}

func TestSupersessionResolve_ResolvedBodyIsSkipped(t *testing.T) {
	body, ok := rewriteSupersededClaim("Adds the facet.\n\nCloses #13780", 13780, 13760, []string{"x.go"})
	if !ok {
		t.Fatal("first rewrite must apply")
	}
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{BehindBy: 0, Body: body}, acmmLevelAutonomous)
	assertNoEscalation(t, obs, parked)
	if res.Actions() != 0 || res.Seen != 1 || len(obs.comments[13782]) != 0 || obs.bodies[13782] != "" {
		t.Fatalf("already-resolved PR must be skipped: res=%+v comments=%v", res, obs.comments[13782])
	}
}

func TestSupersessionResolve_CurrentHeadWithNoRemainingDiffSelfCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"empty diff", []string{}, "the diff left after #13760 merged is empty"},
		{"only overlap", []string{"pkg/dashboard/label_presets.go"}, "only re-touches files that PR already changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The sweep first sees the listing's files (with an extra file)
			// and then re-reads them against the current head; the fixture
			// serves the same list both times, so model "extra at detection,
			// none after" by a closer that covers everything but keeping the
			// own path: a hive PR whose remaining files are all covered.
			res, obs, parked := runResolveSweep(t, supersessionPRFixture{BehindBy: 0, Files: tc.files}, acmmLevelAutonomous)
			assertNoEscalation(t, obs, parked)
			if len(res.Closed) != 1 || len(obs.closed) != 1 || obs.closed[0] != 13782 {
				t.Fatalf("result = %+v closed=%v, want self-close", res, obs.closed)
			}
			if got := obs.labels[13782]; len(got) != 1 || got[0] != SupersededLabel {
				t.Fatalf("labels = %v, want %s", got, SupersededLabel)
			}
			if len(obs.comments[13782]) != 1 || !strings.Contains(obs.comments[13782][0], "Closing this hive-authored PR") {
				t.Fatalf("comments = %v", obs.comments[13782])
			}
		})
	}
}

func TestSupersessionResolve_BelowL6EscalatesOwnPRThroughStageEngine(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{BehindBy: 2}, acmmLevelAutonomous-1)
	if len(res.Commented) != 1 || res.Commented[0].Action != SupersessionActionEscalated {
		t.Fatalf("result = %+v, want escalation", res)
	}
	if len(parked) != 1 || parked[0] != 13782 {
		t.Fatalf("parked = %v, want #13782 at 085", parked)
	}
	if got := obs.labels[13782]; len(got) != 2 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("labels = %v", got)
	}
	if obs.updated[13782] != "" || len(obs.closed) != 0 {
		t.Fatalf("below L6 must not update or close: updated=%q closed=%v", obs.updated[13782], obs.closed)
	}
}

func TestSupersessionResolve_ContributorExtraFilesEscalatesWithFiles(t *testing.T) {
	res, obs, parked := runResolveSweep(t, supersessionPRFixture{Author: "contributor", BehindBy: 2}, acmmLevelAutonomous)
	if len(res.Commented) != 1 || res.Commented[0].Action != SupersessionActionEscalated {
		t.Fatalf("result = %+v, want escalation", res)
	}
	if len(parked) != 1 || parked[0] != 13782 {
		t.Fatalf("parked = %v, want #13782 at 085", parked)
	}
	if got := obs.labels[13782]; len(got) != 2 || got[0] != issueNeedsHumanLabel {
		t.Fatalf("labels = %v", got)
	}
	comment := obs.comments[13782][0]
	if !strings.Contains(comment, "Files the merged PR did not touch: `changelog.d/changed-needs-human-facet.md`, `pkg/dashboard/label_facets.test.js`") {
		t.Fatalf("comment does not list the files: %q", comment)
	}
	if obs.updated[13782] != "" || obs.bodies[13782] != "" || len(obs.closed) != 0 {
		t.Fatalf("contributor PR must not be rebased, rewritten or closed: %+v", obs)
	}
}

func TestRewriteSupersededClaim(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"closes", "Closes #5\nmore", "Refs #5 (closed by #9; this PR adds `a.go`)\nmore"},
		{"qualified fixes", "Fixes hivecommons/hive#5", "Refs hivecommons/hive#5 (closed by #9; this PR adds `a.go`)"},
		{"other issue untouched", "Closes #6", "Closes #6\n\nRefs #5 (closed by #9; this PR adds `a.go`)"},
		{"empty body", "", "Refs #5 (closed by #9; this PR adds `a.go`)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rewriteSupersededClaim(tc.body, 5, 9, []string{"a.go"})
			if !ok {
				t.Fatal("ok = false")
			}
			want := tc.want + "\n\n" + supersessionResolvedMarker(5, 9) + "\n"
			if got != want {
				t.Fatalf("got %q\nwant %q", got, want)
			}
			if again, ok := rewriteSupersededClaim(got, 5, 9, []string{"a.go"}); ok || again != got {
				t.Fatalf("second rewrite must be a no-op: ok=%v %q", ok, again)
			}
		})
	}
}

func TestSupersessionFilesSummary(t *testing.T) {
	if got := supersessionFilesSummary(nil); got != "no files" {
		t.Fatalf("nil = %q", got)
	}
	if got := supersessionFilesSummary([]string{"b", "a"}); got != "`a`, `b`" {
		t.Fatalf("sorted = %q", got)
	}
	if got := supersessionFilesSummary([]string{"g", "f", "e", "d", "c", "b", "a"}); got != "`a`, `b`, `c`, `d`, `e` and 2 more" {
		t.Fatalf("capped = %q", got)
	}
}
