package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/testsuite#991 (2026-10-18..25): the hive pushed its own fix
// commit, signed off as the App, onto j1mc's fork PR (role/contributor). The
// stage pass sent it to 072 on hive-signoff-attribution and nudged j1mc twice
// to re-sign a commit they never made. A maintainer approving the head and
// merging with a merge commit was the real resolution.

// testsuite991Commits is #991's head without the merge-from-base commit:
// the contributor's self-signed commit and the hive's App-signed fix.
var testsuite991Commits = []stageCommitFixture{
	{message: "feat(gnome-e2e): gate extension-validation runs\n\nSigned-off-by: j1mc <j1mc@example.com>", authorLogin: "j1mc", authorName: "j1mc", authorEmail: "j1mc@example.com"},
	{message: "test(gnome-e2e): pin extension-validation gate step\n\nSigned-off-by: hive[bot] <12345+hive[bot]@users.noreply.github.com>", authorLogin: testHiveAppBotLogin, authorName: "hive[bot]", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
}

type signoffCase struct {
	commits    []stageCommitFixture
	reason     string
	squashOnly bool
	// approvedHead, when set, is the head a trusted maintainer approved.
	approvedHead string
	// squashGate: the head's checks include `Sign-off survives the squash`.
	squashGate bool
	// files is the head's diff (GET /pulls/991/files); labels are extra
	// labels on the PR; setup adjusts the engine before the sweep.
	files  []string
	labels []string
	setup  func(*Engine)
	// display is the sweep's display name for acme/widget: "acme/widget"
	// when empty, "widget" as in a single-org hive. squashOnlyList, when
	// set, is auto_merge.squash_only matched by the real SquashOnlyFor.
	display        string
	squashOnlyList []string
}

func runSignoffCase(t *testing.T, tc signoffCase) (*stageAPI, stagelabels.Record, *Engine, func()) {
	t.Helper()
	const n = 991
	api := &stageAPI{labels: map[string]string{}, commitDetails: map[int][]stageCommitFixture{n: tc.commits}}
	if tc.files != nil {
		api.prFiles = map[int][]string{n: tc.files}
	}
	reviewReads := 0
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/991/reviews" {
			return false
		}
		reviewReads++
		out := []map[string]any{}
		if tc.approvedHead != "" {
			out = append(out, map[string]any{"id": 1, "state": "APPROVED", "commit_id": tc.approvedHead, "user": map[string]string{"login": "maintainer"}})
		}
		_ = json.NewEncoder(w).Encode(out)
		return true
	}
	srv := newStageAPI(t, api)
	clock := &contributorLaneClock{at: time.Date(2026, 10, 24, 17, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.SetMergerAuthorizer(func(login string) bool { return login == "maintainer" })
	e.stageLabelPolicy = func() StageLabelPolicy {
		// j1mc is a registered relay author (author/clanker), as on #991.
		p := StageLabelPolicy{Enabled: true, ContributorNudge: true, DCORequired: func(string) (bool, bool) { return true, true }, RelayKnown: func(login string) bool { return login == "j1mc" }}
		if tc.squashOnly {
			p.SquashOnly = func(string) bool { return true }
		}
		if tc.squashOnlyList != nil {
			p.SquashOnly = config.AutoMergeConfig{SquashOnly: tc.squashOnlyList}.SquashOnlyFor
		}
		return p
	}
	if tc.setup != nil {
		tc.setup(e)
	}
	pr := stagePR(n, append([]string{"role/contributor", "author/clanker"}, tc.labels...)...)
	pr.User = &gh.User{Login: gh.Ptr("j1mc")}
	display := tc.display
	if display == "" {
		display = "acme/widget"
	}
	e.recordStage(stageLaneContributor, display, "acme", "widget", pr, tc.reason, false, nil)
	e.noteStageRole("acme", "widget", n, stagelabels.RoleContributor)
	if tc.squashGate {
		e.noteStageDCOGate("acme", "widget", n, pr.GetHead().GetSHA(), nil, map[string]bool{squashSignoffCheck: true})
	}
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(stageKey("acme", "widget", n))
	return api, rec, e, func() {
		// Same head, next sweep, carrying the labels the first one wrote:
		// nothing new is posted.
		before := len(api.comments)
		pr = stagePR(n, append(append([]string{"role/contributor", "author/clanker"}, tc.labels...), addedAll(api)...)...)
		pr.User = &gh.User{Login: gh.Ptr("j1mc")}
		e.recordStage(stageLaneContributor, display, "acme", "widget", pr, tc.reason, false, nil)
		e.noteStageRole("acme", "widget", n, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.comments) != before {
			t.Fatalf("second sweep posted again: %q", api.comments[before:])
		}
		srv.Close()
	}
}

func commentsWith(api *stageAPI, prefix string) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func assertNoContributorAsk(t *testing.T, api *stageAPI) {
	t.Helper()
	for _, c := range api.comments {
		for _, bad := range []string{ContributorNudgeMarkerPrefix, SignoffAttributionMarkerPrefix, "back in your court", "git config user.email", "--reset-author"} {
			if strings.Contains(c, bad) {
				t.Fatalf("contributor asked to fix the hive's commit (%q):\n%s", bad, c)
			}
		}
	}
}

func addedAll(api *stageAPI) []string {
	var out []string
	for _, a := range api.added {
		out = append(out, a...)
	}
	return out
}

func TestHiveCausedSignoffAttributionParksForMaintainer(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict})
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 085 on hive-signoff-attribution", rec)
	}
	added := addedAll(api)
	if !containsLabel(added, stagelabels.NeedsDecision.Label()) || containsLabel(added, stagelabels.ReturnedToContributor.Label()) || containsLabel(added, stagelabels.MergeCommitLabel) {
		t.Fatalf("labels added %v", added)
	}
	assertNoContributorAsk(t, api)
	notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
	if len(notes) != 1 {
		t.Fatalf("maintainer note: %q", api.comments)
	}
	for _, want := range []string{"head=sha", "`stage/085-needs-decision`", "1 of 2 commits", "@j1mc did not make those commits and has nothing to fix", "**Approve**", "`gh pr merge 991 --merge`, not squash", "Ask (maintainer)"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("note missing %q:\n%s", want, notes[0])
		}
	}
	again()
}

func TestHiveCausedSignoffAttributionSquashOnlyParksNamingPolicy(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, squashOnly: true})
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 085", rec)
	}
	assertNoContributorAsk(t, api)
	notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
	if len(notes) != 1 || !strings.Contains(notes[0], "auto_merge.squash_only") || !strings.Contains(notes[0], "lift the squash-only policy") || strings.Contains(notes[0], "--merge") {
		t.Fatalf("squash-only note: %q", api.comments)
	}
	if containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("merge commit elected in a squash-only repo: %v", api.added)
	}
	again()
}

func TestHiveCausedSignoffAttributionApprovedHeadElectsMergeCommit(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: "sha"})
	if rec.Stage == stagelabels.NeedsDecision || rec.Stage == stagelabels.ReturnedToContributor || rec.Reason == stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("approved head still held on attribution: %+v", rec)
	}
	if !containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("merge-method label not written: %v", api.added)
	}
	if len(mergeCommitComments(api)) != 1 || len(commentsWith(api, SignoffAttributionDecisionMarkerPrefix)) != 0 {
		t.Fatalf("comments: %q", api.comments)
	}
	assertNoContributorAsk(t, api)
	again()
}

func TestHiveCausedSignoffAttributionStaleApprovalStillParks(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: "0ld0000"})
	if rec.Stage != stagelabels.NeedsDecision || containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("approval of an older head elected a merge commit: %+v added=%v", rec, api.added)
	}
	assertNoContributorAsk(t, api)
	again()
}

// projectbluefin/testsuite#1037: the sign-off attribution note asked for an
// Approve review on a head that edits CI workflow files while the 085 entry
// comment said no approval overrides the lane's manual-merge rule. When the
// lane never merges the head unattended, the note offers only the by-hand
// merge and names the rule; an open sentinel alert is named as well.
func TestHiveCausedSignoffAttributionUnderManualMergeRuleAsksHandMerge(t *testing.T) {
	for name, tc := range map[string]signoffCase{
		// The owner-approval release is off (the test policy's default).
		"workflow fence, release off": {files: []string{".github/workflows/e2e.yml"}},
		"workflow fence, App lacks workflows": {files: []string{".github/workflows/e2e.yml"}, setup: func(e *Engine) {
			releaseWorkflowFence(e)
			e.unauthorized = map[string]unauthorizedState{e.unauthorizedKey("acme", "widget", 991): {Head: "sha", Step: unauthorizedStepMergeWorkflowPath}}
		}},
		// The lane's own reason, replaced by the sign-off one in the stage
		// pass, still names the rule; no diff read is needed.
		"tests removed lane reason": {reason: contributorLaneReasonTestsRemoved},
	} {
		t.Run(name, func(t *testing.T) {
			tc.commits = testsuite991Commits
			if tc.reason == "" {
				tc.reason = contributorLaneReasonNoVerdict
			}
			tc.labels = []string{"sentinel-alert"}
			api, rec, _, again := runSignoffCase(t, tc)
			if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
				t.Fatalf("record %+v, want 085 on hive-signoff-attribution", rec)
			}
			notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
			if len(notes) != 1 {
				t.Fatalf("maintainer note: %q", api.comments)
			}
			rule := "edits CI workflow / action files, which the lane never merges unattended"
			if tc.reason == contributorLaneReasonTestsRemoved {
				rule = "removes tests, which the lane never merges unattended"
			}
			for _, want := range []string{"The hive will not merge this head itself, approved or not", rule, "merge it by hand with a merge commit (`gh pr merge 991 --merge`, not squash)", "an approving review would not land it", "A sentinel alert is open on this PR: resolve it and have a maintainer clear its label first."} {
				if !strings.Contains(notes[0], want) {
					t.Fatalf("note missing %q:\n%s", want, notes[0])
				}
			}
			if strings.Contains(notes[0], "approve the current head") || strings.Contains(notes[0], "selects the merge commit itself") {
				t.Fatalf("note offers an approval that cannot land the head:\n%s", notes[0])
			}
			again()
		})
	}
}

// A workflow fence the owner's approval releases (hivecommons/hive#14664)
// keeps the approve route: the hive then merges the head itself.
func TestHiveCausedSignoffAttributionReleasableWorkflowFenceKeepsApproveRoute(t *testing.T) {
	api, _, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, files: []string{".github/workflows/e2e.yml"}, setup: releaseWorkflowFence})
	notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
	if len(notes) != 1 || !strings.Contains(notes[0], "approve the current head with a GitHub **Approve** review") || strings.Contains(notes[0], "approved or not") || strings.Contains(notes[0], "sentinel") {
		t.Fatalf("note: %q", api.comments)
	}
	again()
}

func TestMergeCommitRefusalTextManualMergeRuleAndSentinel(t *testing.T) {
	obs := stageObservation{Number: 1037, Display: "acme/widget", Detail: stageDetail{MergeCommitRefused: mergeCommitRefusedRole}}
	if got := mergeCommitRefusalText(obs); !strings.Contains(got, "approve the current head") || strings.Contains(got, "sentinel") {
		t.Fatalf("no rule: %q", got)
	}
	obs.Detail.ManualMergeRule = "the PR touches a human-merge path (auto_merge.human_merge_paths)"
	if got := mergeCommitRefusalText(obs); strings.Contains(got, "approve the current head") || !strings.Contains(got, "approved or not: the PR touches a human-merge path") || !strings.Contains(got, "`gh pr merge 1037 --merge`") {
		t.Fatalf("role, rule: %q", got)
	}
	obs.Detail.MergeCommitRefused = mergeCommitRefusedSquashOnly
	if got := mergeCommitRefusalText(obs); strings.Contains(got, "the hive then merges") || !strings.Contains(got, "a maintainer still merges it by hand with a merge commit: the PR touches a human-merge path") {
		t.Fatalf("squash-only, rule: %q", got)
	}
	obs.Detail.ManualMergeRule = ""
	if got := mergeCommitRefusalText(obs); !strings.Contains(got, "the hive then merges with a merge commit") {
		t.Fatalf("squash-only: %q", got)
	}
	for _, refused := range []string{mergeCommitRefusedRole, mergeCommitRefusedSquashOnly, mergeCommitRefusedHiveOnly, mergeCommitRefusedUnsigned, mergeCommitRefusedLabelsSuppressed, ""} {
		obs.Detail = stageDetail{MergeCommitRefused: refused, SentinelAlert: true}
		if got := mergeCommitRefusalText(obs); !strings.HasSuffix(got, " A sentinel alert is open on this PR: resolve it and have a maintainer clear its label first.") {
			t.Fatalf("%q sentinel: %q", refused, got)
		}
	}
}

// A red check on a PR made only of the contributor's own commits is still
// theirs: 072, as before.
func TestContributorFixableFailureStillReturnsToContributor(t *testing.T) {
	own := []stageCommitFixture{testsuite991Commits[0]}
	_, rec, _, again := runSignoffCase(t, signoffCase{commits: own, reason: "check-failing:unit"})
	if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != "check-failing:unit" {
		t.Fatalf("record %+v, want 072 on check-failing:unit", rec)
	}
	again()
}

// hivecommons/pluk#315's shape on a contributor PR: every commit carries the
// App's identity and the hive never pushed the head, so it is the author's
// own commit made with the wrong identity and still goes back to them.
func TestAuthorsOwnAppSignedCommitStillReturnsToAuthor(t *testing.T) {
	appOnly := []stageCommitFixture{testsuite991Commits[1]}
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: appOnly, reason: contributorLaneReasonNoVerdict})
	if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v, want 072 on hive-signoff-attribution", rec)
	}
	if len(commentsWith(api, SignoffAttributionDecisionMarkerPrefix)) != 0 {
		t.Fatalf("maintainer note on an author-fixable head: %q", api.comments)
	}
	again()
}

// projectbluefin/utah#1005 (2026-10-29): utah is in auto_merge.squash_only as
// the dashboard stores it, "projectbluefin/utah", but a single-org hive's
// sweep displays the bare name, and the election matched the policy on the
// display. It never matched: the PR was labelled merge-method/merge and every
// merge attempt was refused as method-not-allowed. With a bare display and an
// owner/repo entry the election is refused as squash-only, approved head or
// not, and the PR parks once at 085 with the squash-only note. The tests
// above pass "acme/widget" as the display, which is why they never saw it.
func TestBareDisplaySquashOnlyOwnerRepoRefusesMergeCommit(t *testing.T) {
	for _, approvedHead := range []string{"", "sha"} {
		t.Run("approved="+approvedHead, func(t *testing.T) {
			api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: approvedHead, display: "widget", squashOnlyList: []string{"acme/widget"}})
			if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
				t.Fatalf("record %+v, want 085", rec)
			}
			if containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
				t.Fatalf("merge commit elected in a squash-only repo: %v", api.added)
			}
			notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
			if len(notes) != 1 || !strings.Contains(notes[0], "auto_merge.squash_only") || strings.Contains(notes[0], "--merge") {
				t.Fatalf("squash-only note: %q", api.comments)
			}
			again()
		})
	}
}

// A repo not on the list keeps electing a merge commit for an approved
// mixed-authorship head under a bare display.
func TestBareDisplayNotSquashOnlyElectsMergeCommit(t *testing.T) {
	api, rec, _, again := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, approvedHead: "sha", display: "widget", squashOnlyList: []string{"acme/other"}})
	if rec.Stage == stagelabels.NeedsDecision || rec.Reason == stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("approved head still held on attribution: %+v", rec)
	}
	if !containsLabel(addedAll(api), stagelabels.MergeCommitLabel) {
		t.Fatalf("merge-method label not written: %v", api.added)
	}
	again()
}

// governor.labels.stage.dco.required has the same shape: RequiredFor tries the
// exact key, then the bare name, then "*", so a bare display never reached an
// "owner/repo" key. Resolved on the owner/repo name, every key form applies.
func TestDCORequiredResolvesOwnerRepoKeyForBareDisplay(t *testing.T) {
	obs := stageObservation{Display: "widget", Owner: "acme", Repo: "widget", HeadSHA: "h1"}
	detected := obs
	detected.Detail.DCOGate, detected.Detail.DCOGateHead = true, "h1"
	for _, tc := range []struct {
		name     string
		required map[string]bool
		obs      stageObservation
		want     bool
	}{
		{"owner/repo key requires", map[string]bool{"acme/widget": true}, obs, true},
		{"owner/repo key waives a detected gate", map[string]bool{"Acme/Widget": false}, detected, false},
		{"bare key still applies", map[string]bool{"widget": true}, obs, true},
		{"wildcard still applies", map[string]bool{"*": true}, obs, true},
		{"another repo's key falls back to detection", map[string]bool{"acme/other": false}, detected, true},
		{"no key and no gate", map[string]bool{"acme/other": true}, obs, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := StageLabelPolicy{DCORequired: config.StageDCOConfig{Required: tc.required}.RequiredFor}
			if got := dcoRequired(policy, tc.obs); got != tc.want {
				t.Fatalf("dcoRequired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStageRepoFullName(t *testing.T) {
	if got := stageRepoFullName(stageObservation{Display: "widget", Owner: "acme", Repo: "widget"}); got != "acme/widget" {
		t.Fatalf("bare display = %q", got)
	}
	if got := stageRepoFullName(stageObservation{Display: "acme/widget"}); got != "acme/widget" {
		t.Fatalf("no owner/repo falls back to the display: %q", got)
	}
}
