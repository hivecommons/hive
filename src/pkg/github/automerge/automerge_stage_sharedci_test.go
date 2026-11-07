package automerge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#12606: a red PR deferred to an open shared-CI incident is
// not the author's to fix. It reads as CI pending (050/060) with the incident
// named; conflicts and requested changes stay author-facing.
func TestClassifyStageSharedCIIncidentKeepsRedOutOfRepairStages(t *testing.T) {
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	reject := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, VerdictKnown: true}
	redOptional := approve
	redOptional.RedChecks, redOptional.RedChecksHead = []string{"lint"}, "sha"
	const incident = "shared-ci-incident:4242"
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		reason string
	}{
		{name: "contributor red required", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build", SharedCIIncident: 4242}, want: stagelabels.ReviewPending, reason: incident},
		{name: "trusted red approved", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "check-failing:unit", Detail: approve, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "self-authored red", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "status-failure", SharedCIIncident: 4242}, want: stagelabels.ReviewPending, reason: incident},
		{name: "red optional while soaking", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h)", Detail: redOptional, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "lane error with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, Detail: redOptional, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "pending CI is unchanged", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:build", SharedCIIncident: 4242}, want: stagelabels.ReviewPending},
		{name: "conflicting stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "conflicting", SharedCIIncident: 4242}, want: stagelabels.NeedsAuthor},
		{name: "changes requested stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build", Detail: reject, SharedCIIncident: 4242}, want: stagelabels.NeedsAuthor, reason: "reviewer-verdict:changes_requested"},
		{name: "no incident is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build"}, want: stagelabels.NeedsAuthor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStage(tc.obs)
			if got.Stage != tc.want || got.Reason != tc.reason || got.Rank != stageRankOwned {
				t.Fatalf("classifyStage=%+v want owned stage %v reason %q", got, tc.want, tc.reason)
			}
		})
	}
}

func TestStageActivitySharedCIAndBudgetWaitOnHive(t *testing.T) {
	for _, reason := range []string{"shared-ci-incident:4242", autoMergeReasonBehindBudgetExhausted} {
		if a := stageActivity(stageObservation{HeadSHA: "sha", Reason: reason}, stagelabels.ReviewPending); !a.WaitingOnHive {
			t.Fatalf("%q does not wait on the hive", reason)
		}
	}
	if a := stageActivity(stageObservation{HeadSHA: "sha", Reason: "conflicting"}, stagelabels.ReturnedToContributor); a.WaitingOnHive {
		t.Fatal("conflicting waits on the hive")
	}
}

// A contributor PR skipped for admission is waiting on a person, so
// the stage-dwell "waits on the hive" warning must not fire for it.
func TestStageActivityAdmissionSkipDoesNotWaitOnHive(t *testing.T) {
	for _, reason := range []string{
		contributorLaneReasonNoLinkedIssue, contributorLaneReasonNoClaim, contributorLaneReasonClaimUnverified,
		contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive, contributorLaneReasonClaimTier,
		contributorLaneReasonAuthorAssociation,
	} {
		obs := stageObservation{HeadSHA: "sha", Reason: reason}
		if a := stageActivity(obs, stagelabels.ReviewPending); a.WaitingOnHive {
			t.Errorf("%q waits on the hive", reason)
		}
	}
	if a := stageActivity(stageObservation{HeadSHA: "sha"}, stagelabels.ReviewPending); !a.WaitingOnHive {
		t.Fatal("verdict-less review-pending head no longer waits on the hive")
	}
}

// A base-red clanker PR stays at 050 across repeated sweeps — no 072, no
// nudge comment or relay interrupt, no stall — and is handed back to its
// author with a nudge only once the incident closes and the head is still
// red.
func TestApplyStageLabelsSharedCIIncidentStableWithoutNudge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	open := 4242
	var asked []string
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, StalledRequireQuiet: true}
	policy.SharedCIIncident = func(repo string, number int, head string) int {
		asked = append(asked, repo+"|"+head)
		if number != 7 {
			return 0
		}
		return open
	}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	ctx := context.Background()
	observe := func(pr *gh.PullRequest) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:build", false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe(nudgePR(7, "aaa"))
	if len(asked) == 0 || asked[0] != "acme/widget|aaa" {
		t.Fatalf("SharedCIIncident asked %v, want acme/widget|aaa", asked)
	}
	if !hasAdded(api, "stage/050-review-pending") || hasAdded(api, "stage/072-returned-to-contributor") {
		t.Fatalf("first sweep added=%v, want 050 and no 072", api.added)
	}
	if len(nudgeComments(api)) != 0 || len(sink.nudges) != 0 {
		t.Fatalf("base-red PR nudged: comments=%v sink=%v", api.comments, sink.nudges)
	}
	key := stageKey("acme", "widget", 7)
	if rec, _ := e.stageStoreOrShared().Get(key); rec.Reason != "shared-ci-incident:4242" {
		t.Fatalf("stored reason %q", rec.Reason)
	}

	labels := []string{"stage/050-review-pending", "role/contributor", "author/clanker"}
	for i := 0; i < 4; i++ {
		clock.at = clock.at.Add(2 * time.Hour)
		observe(nudgePR(7, "aaa", labels...))
		if api.writes() != 0 || len(sink.nudges) != 0 {
			t.Fatalf("sweep %d: added=%v removed=%v comments=%v sink=%v", i+2, api.added, api.removed, api.comments, sink.nudges)
		}
	}

	// Incident closed and the head is still red: now it is the author's.
	open = 0
	clock.at = clock.at.Add(time.Minute)
	observe(nudgePR(7, "aaa", labels...))
	if !hasAdded(api, "stage/072-returned-to-contributor") || len(nudgeComments(api)) != 1 || len(sink.nudges) != 1 {
		t.Fatalf("after recovery: added=%v comments=%v sink=%v", api.added, api.comments, sink.nudges)
	}
}

func hasAdded(api *stageAPI, label string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, batch := range api.added {
		for _, l := range batch {
			if strings.EqualFold(l, label) {
				return true
			}
		}
	}
	return false
}

// A clanker PR nudged at 072 before the scanner deferred its red to a
// shared-CI incident has that nudge edited in place to say it was
// withdrawn, once, with nothing new posted; the head's nudge is forgotten,
// so a head still red after the incident closes is nudged afresh. Seen on
// hivecommons/hive#16993: nudged at 15:21 for `native-queue-policy`, deferred
// to incident #17003 at 15:31, and the nudge kept saying "the hive will not
// merge it until the head changes".
func TestApplyStageLabelsSharedCIIncidentWithdrawsEarlierNudge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 11, 7, 15, 21, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued"}}
	open := 0
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, StalledRequireQuiet: true}
	policy.SharedCIIncident = func(string, int, string) int { return open }
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	ctx := context.Background()
	key := stageKey("acme", "widget", 7)
	observe := func(labels ...string) {
		t.Helper()
		api.reset()
		api.mu.Lock()
		api.commentEdits = nil
		api.mu.Unlock()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa", labels...), "check-failure", false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe()
	if !hasAdded(api, "stage/072-returned-to-contributor") || len(nudgeComments(api)) != 1 {
		t.Fatalf("red head: added=%v comments=%v, want 072 and one nudge", api.added, api.comments)
	}

	open = 17003
	clock.at = clock.at.Add(10 * time.Minute)
	observe("stage/072-returned-to-contributor", "role/contributor", "author/clanker")
	if !hasAdded(api, "stage/050-review-pending") || len(api.comments) != 0 {
		t.Fatalf("deferred: added=%v comments=%v, want 050 and nothing posted", api.added, api.comments)
	}
	if len(api.commentEdits) != 1 {
		t.Fatalf("deferred: edits=%v, want the nudge edited once", api.commentEdits)
	}
	edit := api.commentEdits[0]
	for _, want := range []string{ContributorNudgeMarkerPrefix + " reason=check-failure head=aaa n=1 mode=queued -->", "<!-- hive:contributor-nudge-withdrawn -->", "**Withdrawn:**", "shared CI incident #17003", "once #17003 closes"} {
		if !strings.Contains(edit, want) {
			t.Errorf("withdrawn nudge lacks %q:\n%s", want, edit)
		}
	}
	for _, gone := range []string{"@mallory", "until the head changes"} {
		if strings.Contains(edit, gone) {
			t.Errorf("withdrawn nudge still says %q:\n%s", gone, edit)
		}
	}
	if rec, _ := e.stageStoreOrShared().Get(key); rec.NudgeHead != "" || rec.NudgeCount != 0 {
		t.Fatalf("nudge not forgotten: head=%q count=%d", rec.NudgeHead, rec.NudgeCount)
	}

	clock.at = clock.at.Add(time.Hour)
	observe("stage/050-review-pending", "role/contributor", "author/clanker")
	if len(api.commentEdits) != 0 || len(api.comments) != 0 {
		t.Fatalf("second deferred pass: edits=%v comments=%v, want none", api.commentEdits, api.comments)
	}

	// The incident closed and the re-run left the head red: the author's
	// now, with a fresh first nudge rather than silence.
	open = 0
	clock.at = clock.at.Add(time.Hour)
	observe("stage/050-review-pending", "role/contributor", "author/clanker")
	// One nudge comment per PR: the withdrawn comment is rewritten into
	// the fresh first nudge rather than a second one posted.
	got := append(nudgeComments(api), api.commentEdits...)
	if !hasAdded(api, "stage/072-returned-to-contributor") || len(got) != 1 || !strings.Contains(got[0], "n=1 ") || strings.Contains(got[0], "<!-- hive:contributor-nudge-withdrawn -->") {
		t.Fatalf("after recovery: added=%v nudges=%v, want 072 and a fresh first nudge", api.added, got)
	}
}

// The withdrawal text for a base-branch red names the checks.
func TestWithdrawnNudgeCommentBaseBranchRed(t *testing.T) {
	prev := ContributorNudgeMarkerPrefix + " head=aaa n=1 mode=comment -->\n@mallory this PR is back in your court."
	got := WithdrawnNudgeComment(prev, stageReasonBaseBranchRed+"build,lint")
	if !strings.HasPrefix(got, ContributorNudgeMarkerPrefix+" head=aaa n=1 mode=comment -->\n"+nudgeWithdrawnMarker+"\n") {
		t.Fatalf("marker lines not kept:\n%s", got)
	}
	for _, want := range []string{"(`build`, `lint`) are red on the base branch too", "once the base branch is green"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "@mallory") {
		t.Errorf("withdrawal keeps the mention:\n%s", got)
	}
}

// When the CI gate's reason is its bare `check-failure` token (a base branch
// with no required checks), the nudge names the checks red on the head.
func TestContributorNudgeNamesChecksForBareCIToken(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 7, 15, 21, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued"}}
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, StalledRequireQuiet: true}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	var asked []string
	e.checkSummary = func(_ context.Context, owner, repo, sha string) (hgithub.CommitChecksSummary, error) {
		asked = append(asked, owner+"/"+repo+"@"+sha)
		return hgithub.CommitChecksSummary{Total: 3, Green: 1, Red: []string{"native-queue-policy", "unit"}}, nil
	}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa"), "check-failure", false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := nudgeComments(api)
	if len(got) != 1 {
		t.Fatalf("nudges=%v, want one", got)
	}
	if !strings.Contains(got[0], "- `native-queue-policy`\n- `unit`\n") || strings.Contains(got[0], "`check-failure`") {
		t.Fatalf("nudge does not name the red checks:\n%s", got[0])
	}
	if !slices.Contains(asked, "acme/widget@aaa") {
		t.Fatalf("check digest asked %v, want acme/widget@aaa", asked)
	}
	if len(sink.nudges) != 1 || !slices.Equal(sink.nudges[0].FailingChecks, []string{"native-queue-policy", "unit"}) {
		t.Fatalf("relay task checks = %v", sink.nudges)
	}
}

func TestBareCIToken(t *testing.T) {
	for entry, want := range map[string]bool{
		"check-failure": true, "status-error": true, "check-timed_out": true,
		"check-failing:unit": false, "native-queue-policy": false, "check-links": false, "check-pending": false,
	} {
		if got := bareCIToken(entry); got != want {
			t.Errorf("bareCIToken(%q) = %v, want %v", entry, got, want)
		}
	}
}
