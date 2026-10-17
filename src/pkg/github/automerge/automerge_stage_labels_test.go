package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageAPI is a minimal GitHub label/comment API that records every write.
type stageAPI struct {
	mu       sync.Mutex
	labels   map[string]string // repo label → colour
	added    [][]string
	removed  []string
	comments []string
	edits    []string
	creates  []string
	lists    int
	gets     int
	url      string
	// extra handles routes beyond labels/comments (returns true when it
	// served the request); the adoption tests use it for refs and pulls.
	extra func(w http.ResponseWriter, r *http.Request) bool
}

func newStageAPI(t *testing.T, api *stageAPI) *httptest.Server {
	t.Helper()
	if api.labels == nil {
		api.labels = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		path := r.URL.Path
		if api.extra != nil && api.extra(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && path == "/repos/acme/widget/labels":
			api.lists++
			out := []map[string]string{}
			for name, color := range api.labels {
				out = append(out, map[string]string{"name": name, "color": color})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && path == "/repos/acme/widget/labels":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			api.labels[body["name"]] = body["color"]
			api.creates = append(api.creates, body["name"])
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/acme/widget/labels/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if old := strings.TrimPrefix(path, "/repos/acme/widget/labels/"); old != body["name"] {
				delete(api.labels, old) // rename in place
			}
			api.labels[body["name"]] = body["color"]
			api.edits = append(api.edits, body["name"]+"="+body["color"])
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/issues/") && !strings.Contains(strings.TrimPrefix(path, "/repos/acme/widget/issues/"), "/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(path, "/repos/acme/widget/issues/"))
			api.gets++
			stagePRTruthMu.Lock()
			labels := make([]map[string]string, 0)
			for _, l := range stagePRTruth[n] {
				labels = append(labels, map[string]string{"name": l})
			}
			stagePRTruthMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "labels": labels})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			api.added = append(api.added, body)
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodDelete && strings.Contains(path, "/labels/"):
			api.removed = append(api.removed, path[strings.LastIndex(path, "/labels/")+len("/labels/"):])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			api.comments = append(api.comments, body["body"])
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	api.url = srv.URL
	return srv
}

func (a *stageAPI) writes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.added) + len(a.removed) + len(a.comments)
}

func (a *stageAPI) getCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets
}

func (a *stageAPI) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.added, a.removed, a.comments, a.edits, a.creates = nil, nil, nil, nil, nil
}

// stagePRTruth is what GET /issues/N answers: the fresh read Enforce takes
// before writing (hivecommons/hive#12512). stagePR seeds it with the labels
// the fixture carries so the read agrees with the observation unless a test
// drifts it on purpose (see TestApplyStageLabels_HealsDuplicateStage).
var (
	stagePRTruthMu sync.Mutex
	stagePRTruth   = map[int][]string{}
)

func setStagePRTruth(number int, labels ...string) {
	stagePRTruthMu.Lock()
	defer stagePRTruthMu.Unlock()
	stagePRTruth[number] = append([]string(nil), labels...)
}

func stagePR(number int, labels ...string) *gh.PullRequest {
	pr := &gh.PullRequest{Number: gh.Ptr(number), Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha")}}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	setStagePRTruth(number, labels...)
	return pr
}

// greenChecks is the default head digest for stage-engine tests: every
// gating check green, DCO signed. Tests that need another picture swap
// Engine.checkSummary.
func greenChecks(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
	return hgithub.CommitChecksSummary{Total: 3, Green: 3, DCO: 1}, nil
}

func newStageEngine(t *testing.T, apiURL string, clock *contributorLaneClock) (*Engine, *StageTracker, *stagelabels.Store) {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	tracker := NewStageTracker()
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	e := New(client, Options{
		Now:              clock.now,
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
		StageTracker:     tracker,
		StageStore:       store,
		CheckSummary:     greenChecks,
	})
	return e, tracker, store
}

func TestClassifyStageMapping(t *testing.T) {
	approve := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}}
	reject := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	incomplete := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}}
	human := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}}
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		rank   int
		term   bool
		reason string
	}{
		{name: "merged", obs: stageObservation{Lane: stageLaneSelf, Merged: true}, rank: stageRankOwned, term: true},
		{name: "closed", obs: stageObservation{Lane: stageLaneSelf, Reason: "closed"}, rank: stageRankOwned, term: true},
		{name: "draft", obs: stageObservation{Lane: stageLaneSelf, Reason: "draft"}, rank: stageRankOwned},
		{name: "handoff", obs: stageObservation{Lane: stageLaneSelf, Reason: "not-app-authored"}},
		{name: "error", obs: stageObservation{Lane: stageLaneSelf, Reason: "fetch-pr", Err: true}},
		{name: "review pending", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "incomplete verdict is still pending", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: incomplete}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "requires_human behind no-verdict hands off to a person", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: human}, want: stagelabels.NeedsDecision, rank: stageRankOwned, reason: "reviewer-verdict:requires_human"},
		{name: "ci pending no verdict", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-pending"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "required check pending no verdict", obs: stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:build-and-test"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "required check pending approved", obs: stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:build-and-test", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "ci pending approved", obs: stageObservation{Lane: stageLaneSelf, Reason: "required-check-missing:unit", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "tide pending is not red", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-check-pending:tide"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "head too young is pending", obs: stageObservation{Lane: stageLaneContributor, Reason: "pending: head pushed 33s ago (< min_head_age)"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "self ci red", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-failing:unit"}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "contributor ci red", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-check-failing:unit"}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "trusted conflicting", obs: stageObservation{Lane: stageLaneTrusted, Reason: "conflicting"}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "self changes requested", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-pending", Detail: reject}, want: stagelabels.Fixing, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "changes requested while head too young", obs: stageObservation{Lane: stageLaneContributor, Reason: "pending: head pushed 1m0s ago (< min_head_age)", Detail: reject}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "changes requested behind no-verdict", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: reject}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "contributor changes requested", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonChangesRequested}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "soaking", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSoaking + "(30m)"}, want: stagelabels.MergeQueue, rank: stageRankOwned},
		{name: "merge pool", obs: stageObservation{Lane: stageLaneSelf, Reason: "head-changed-since-eval"}, want: stagelabels.MergeQueue, rank: stageRankOwned},
		{name: "hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "label:hold"}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "sentinel blocked", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "needs-rebase hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "excluded-label:needs-rebase", Labels: []string{"needs-rebase"}}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "no claim is a weak admission opinion", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim}, want: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission},
		{name: "claim tier below require_tiers", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier}, want: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission},
		{name: "no claim but needs-rebase", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Labels: []string{"needs-rebase"}}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "untrusted role weak", obs: stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}, want: stagelabels.ReadyForHumanMerge, rank: stageRankRole},
		{name: "human merge path", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonHumanMergePath}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "approved green unclassified reason", obs: stageObservation{Lane: stageLaneContributor, Reason: "something-new", Detail: approve}, want: stagelabels.AwaitingMergeAdmission, rank: stageRankRole},
		{name: "approved green lane error", obs: stageObservation{Lane: stageLaneContributor, Reason: "review-state-check", Err: true, Detail: approve}, want: stagelabels.AwaitingMergeAdmission, rank: stageRankRole},
		{name: "unapproved unclassified reason", obs: stageObservation{Lane: stageLaneContributor, Reason: "something-new"}},
		{name: "approved but ci pending stays reviewed", obs: stageObservation{Lane: stageLaneContributor, Reason: "check-pending", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "approved soaking is merge queue", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSoaking + "(5m)", Detail: approve}, want: stagelabels.MergeQueue, rank: stageRankOwned},
	}
	for _, tc := range cases {
		got := classifyStage(tc.obs)
		if got.Stage != tc.want || got.Rank != tc.rank || got.Terminal != tc.term || got.Reason != tc.reason {
			t.Errorf("%s: got %+v want stage=%v rank=%d terminal=%v reason=%q", tc.name, got, tc.want, tc.rank, tc.term, tc.reason)
		}
	}
	// requires_human is a human hand-off too, so routeHuman posts the 085
	// entry comment for it like every other needs-decision arrival.
	if got := classifyStage(stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict, Detail: human}); !got.Human {
		t.Fatalf("requires_human=%+v want human", got)
	}
}

// TestClassifyCIReasonNeverRedForPendingOrInconclusive pins the CI reason
// classes: a pending, missing, too-young or inconclusive (neutral/skipped)
// check is waiting, never failing (hivecommons/hive#12403: 54/55 checks
// green with `tide` pending must not read as red).
func TestClassifyCIReasonNeverRedForPendingOrInconclusive(t *testing.T) {
	cases := map[string]ciReasonKind{
		"":                                      ciReasonNone,
		"required-check-pending:tide":           ciReasonPending,
		"required-check-pending:build-and-test": ciReasonPending,
		"required-check-missing:build-and-test": ciReasonPending,
		"status-pending":                        ciReasonPending,
		"check-pending":                         ciReasonPending,
		"ci-unverified":                         ciReasonPending,
		"pending: head pushed 33s ago (< min_head_age)": ciReasonPending,
		"pending: unit has not started":                 ciReasonPending,
		"check-neutral":                                 ciReasonPending,
		"check-skipped":                                 ciReasonPending,
		"required-check-failing:tide":                   ciReasonRed,
		"check-failing:unit":                            ciReasonRed,
		"status-failure":                                ciReasonRed,
		"status-error":                                  ciReasonRed,
		"check-cancelled":                               ciReasonRed,
		"check-timed_out":                               ciReasonRed,
		"contributor-lane:no-claim":                     ciReasonNone,
	}
	for reason, want := range cases {
		if got := classifyCIReason(reason); got != want {
			t.Errorf("%q: got %d want %d", reason, got, want)
		}
	}
}

func TestResolveStageLanePrecedence(t *testing.T) {
	// Trusted-author lane says "not trusted"; contributor lane refines it.
	v, author, _ := resolveStage([]stageObservation{
		{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
		{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{Author: stagelabels.AuthorContributor}},
	})
	if v.Stage != stagelabels.ReviewPending || author != stagelabels.AuthorContributor {
		t.Fatalf("got %v/%v", v.Stage, author)
	}
	// Only the weak opinion: needs-human + author/untrusted.
	v, author, _ = resolveStage([]stageObservation{{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}})
	if v.Stage != stagelabels.ReadyForHumanMerge || author != stagelabels.AuthorUntrusted {
		t.Fatalf("weak-only got %v/%v", v.Stage, author)
	}
	// Self lane outranks the contributor lane's hand-off.
	v, _, _ = resolveStage([]stageObservation{
		{Lane: stageLaneSelf, Reason: "check-failing:unit", Detail: stageDetail{Author: stagelabels.AuthorTrusted}},
		{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored},
	})
	if v.Stage != stagelabels.Fixing {
		t.Fatalf("self lane got %v", v.Stage)
	}
}

// TestResolveStageAdmissionOpinions covers the weak "cannot vouch for the
// author" opinions (hivecommons/hive#12403, #12497): a lane that does not
// admit the author only says needs-human when no lane admits them.
func TestResolveStageAdmissionOpinions(t *testing.T) {
	trusted := stageDetail{Author: stagelabels.AuthorTrusted}
	cases := []struct {
		name   string
		list   []stageObservation
		stage  stagelabels.Stage
		rank   int
		author stagelabels.Author
		lane   string
	}{
		{
			// #12403: registry-trusted author; the trusted-author lane's role
			// check must not flip the PR to 090 while the contributor lane has
			// no classified opinion (unmapped reason, transient error).
			name: "registry-trusted author with role check and no contributor opinion",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: "something-new", Detail: trusted},
			},
			rank: stageRankNone, author: stagelabels.AuthorTrusted, lane: stageLaneTrusted,
		},
		{
			// #12497: maintainer PR; the trusted-author lane owns it, the
			// contributor lane's no-claim is only its own inability to admit.
			name: "maintainer with trusted lane error and contributor no-claim",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "fetch-pr", Err: true, Detail: trusted},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Author: stagelabels.AuthorUntrusted}},
			},
			rank: stageRankNone, author: stagelabels.AuthorTrusted, lane: stageLaneTrusted,
		},
		{
			name: "maintainer ci pending beats contributor no-claim",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "required-check-pending:golangci-lint", Detail: trusted},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: trusted},
			},
			stage: stagelabels.ReviewPending, rank: stageRankOwned, author: stagelabels.AuthorTrusted, lane: stageLaneTrusted,
		},
		{
			// Contributor tier below require_tiers: nobody admits, 090 is right
			// and the contributor lane's reason is the one to show.
			name: "contributor tier is needs-human from the contributor lane",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier, Detail: stageDetail{Author: stagelabels.AuthorContributor}},
			},
			stage: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission, author: stagelabels.AuthorContributor, lane: stageLaneContributor,
		},
		{
			name: "unknown author with no claim is needs-human untrusted",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Author: stagelabels.AuthorUntrusted}},
			},
			stage: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission, author: stagelabels.AuthorUntrusted, lane: stageLaneContributor,
		},
		{
			// A hold-class reason applies whoever the author is.
			name: "trusted author sentinel-blocked is needs-decision",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked, Detail: trusted},
			},
			stage: stagelabels.NeedsDecision, rank: stageRankOwned, author: stagelabels.AuthorTrusted, lane: stageLaneContributor,
		},
	}
	for _, tc := range cases {
		v, author, obs := resolveStage(tc.list)
		if v.Stage != tc.stage || v.Rank != tc.rank || author != tc.author || obs.Lane != tc.lane {
			t.Errorf("%s: got stage=%v rank=%d author=%v lane=%s; want stage=%v rank=%d author=%v lane=%s", tc.name, v.Stage, v.Rank, author, obs.Lane, tc.stage, tc.rank, tc.author, tc.lane)
		}
	}
}

// TestStageAuthorTierSurvivesContributorLane reproduces hivecommons/hive#12490:
// a maintainer's PR is evaluated by the trusted-author lane (CI pending) and
// then listed by the contributor lane, which knows no registry profile for
// the author and reports "untrusted" + needs-human. The trusted tier and the
// trusted lane's stage must win regardless of the order the lanes ran in.
func TestStageAuthorTierSurvivesContributorLane(t *testing.T) {
	for _, trustedFirst := range []bool{true, false} {
		api := &stageAPI{}
		srv := newStageAPI(t, api)
		clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
		e, _, _ := newStageEngine(t, srv.URL, clock)

		trusted := func() {
			e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(12484), "required-check-pending:build-and-test", false, nil)
			e.noteStageAuthor("acme", "widget", 12484, stagelabels.AuthorTrusted)
		}
		contributor := func() {
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12484), contributorLaneReasonNoLinkedIssue, false, nil)
			e.noteStageAuthor("acme", "widget", 12484, stagelabels.AuthorFromTier("", map[string]bool{"trusted": true}))
		}
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(12484), "not-app-authored", false, nil)
		if trustedFirst {
			trusted()
			contributor()
		} else {
			contributor()
			trusted()
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.added) != 1 || !reflect.DeepEqual(api.added[0], []string{"author/trusted", "stage/050-review-pending"}) {
			t.Fatalf("trustedFirst=%v added %v", trustedFirst, api.added)
		}
		srv.Close()
	}
}

// TestApplyStageLabelsWeakOpinionWaitsForEveryLane reproduces the split
// drain seen on the hive spoke: two drivers share one tracker, so a
// maintainer PR's trusted-author observation (050, author trusted) is
// drained by one pass and the contributor lane's `no-claim` for the same
// PR lands alone in the next. The weak admission opinion must not be
// written while a lane that is enabled for the repo has not reported;
// once the trusted-author lane's observation is in the same drain, the
// owned stage wins as before.
func TestApplyStageLabelsWeakOpinionWaitsForEveryLane(t *testing.T) {
	api := &stageAPI{}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true} }
	e.contributorLanePolicy = func() ContributorLanePolicy { return ContributorLanePolicy{Enabled: true} }
	ctx := context.Background()
	const number = 12561
	pr := func() *gh.PullRequest { return stagePR(number, "author/trusted", "stage/050-review-pending") }

	// Drain 1: only the contributor lane's weak opinion → nothing written,
	// nothing recorded; the labels the trusted-author lane wrote stand.
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr(), contributorLaneReasonNoClaim, false, nil)
	e.noteStageAuthor("acme", "widget", number, stagelabels.AuthorUntrusted)
	res, err := e.ApplyStageLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Writes != 0 || api.writes() != 0 {
		t.Fatalf("contributor-only drain: res=%+v writes=%d added=%v removed=%v", res, api.writes(), api.added, api.removed)
	}
	if _, ok := store.Get(stageKey("acme", "widget", number)); ok {
		t.Fatal("weak opinion from an incomplete drain must not be recorded")
	}

	// Drain 2: only the trusted-author lane, with an owned stage → written.
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(number, "author/trusted"), "required-check-pending:build-and-test", false, nil)
	e.noteStageAuthor("acme", "widget", number, stagelabels.AuthorTrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/050-review-pending"}}) || len(api.removed) != 0 {
		t.Fatalf("trusted-only drain: added=%v removed=%v", api.added, api.removed)
	}

	// Drain 3: both lanes present, both weak (outside author) → the human
	// hand-off is written; completeness, not weakness, is the gate. With no
	// reviewer configured RouteHuman lands it at 085 (#12531).
	api.reset()
	setStagePRTruth(77, "author/untrusted")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(77), "untrusted-author-role", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(77), contributorLaneReasonNoClaim, false, nil)
	e.noteStageAuthor("acme", "widget", 77, stagelabels.AuthorUntrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/untrusted", "stage/085-needs-decision"}}) {
		t.Fatalf("complete drain: added=%v", api.added)
	}

	// A lane disabled for the repo is not waited for.
	api.reset()
	e.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{} }
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(78), contributorLaneReasonNoClaim, false, nil)
	e.noteStageAuthor("acme", "widget", 78, stagelabels.AuthorUntrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/untrusted", "stage/085-needs-decision"}}) {
		t.Fatalf("trusted lane disabled: added=%v", api.added)
	}
}

func TestApplyStageLabelsProvisionsWritesOnceAndClearsOnTerminal(t *testing.T) {
	api := &stageAPI{labels: map[string]string{"stage/050-review-pending": "0000ff", "bug": "ff0000"}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()

	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "claimed", "🐝 covered by PR"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
	res, err := e.ApplyStageLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 1 || res.Writes != 3 {
		t.Fatalf("result %+v", res)
	}
	if len(api.added) != 1 || !reflect.DeepEqual(api.added[0], []string{"author/contributor", "stage/050-review-pending"}) {
		t.Fatalf("added %v", api.added)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"claimed", "🐝 covered by PR"}) {
		t.Fatalf("removed %v", api.removed)
	}
	// Provisioning: existing wrong colour patched, missing ones created,
	// human labels untouched, exactly one list call.
	if api.lists != 1 || !reflect.DeepEqual(api.edits, []string{"stage/050-review-pending=" + stagelabels.StageColor}) {
		t.Fatalf("lists=%d edits=%v", api.lists, api.edits)
	}
	if len(api.creates) != len(stagelabels.AllLabelDefs())-1 || api.labels["bug"] != "ff0000" {
		t.Fatalf("creates=%d labels=%v", len(api.creates), api.labels)
	}

	// Same state next cycle (labels now as written): no writes, no re-list.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "author/contributor", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
	if res, err = e.ApplyStageLabels(ctx); err != nil || res.Writes != 0 || api.writes() != 0 || api.lists != 1 {
		t.Fatalf("idempotent pass: res=%+v err=%v writes=%d lists=%d", res, err, api.writes(), api.lists)
	}

	// Hold applied by a human → needs-decision (085) replaces review-pending
	// (no reviewer is configured here, which is itself a decision); the
	// human's hold label is left alone and the entry comment names both.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "author/contributor", "stage/050-review-pending"), "excluded-label:hold", false, nil)
	e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
	if _, err = e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/085-needs-decision"}}) || !reflect.DeepEqual(api.removed, []string{"stage/050-review-pending"}) {
		t.Fatalf("hold transition added=%v removed=%v", api.added, api.removed)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) || !strings.Contains(api.comments[0], "the `hold` label is set") || !strings.Contains(api.comments[0], "no reviewer agent is configured") {
		t.Fatalf("085 entry comments = %q", api.comments)
	}
	// Same state again: no second entry comment.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "author/contributor", "stage/085-needs-decision"), "excluded-label:hold", false, nil)
	e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
	if _, err = e.ApplyStageLabels(ctx); err != nil || len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("085 dedupe: err=%v comments=%q writes=%d", err, api.comments, api.writes())
	}

	// Merged → every owned label removed, record forgotten.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "author/contributor", "stage/085-needs-decision", "stage/stalled"), "", true, nil)
	if _, err = e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(api.removed)
	if len(api.added) != 0 || !reflect.DeepEqual(api.removed, []string{"author/contributor", "stage/085-needs-decision", "stage/stalled"}) {
		t.Fatalf("terminal added=%v removed=%v", api.added, api.removed)
	}
	if _, ok := store.Get(stageKey("acme", "widget", 7)); ok {
		t.Fatal("terminal item still tracked")
	}
}

func TestApplyStageLabelsStalledAndRegressed(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	observe := func(labels []string, reason string) {
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(9, labels...), reason, false, nil)
		e.noteStageAuthor("acme", "widget", 9, stagelabels.AuthorTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe(nil, "head-changed-since-eval") // → merge-queue
	api.reset()
	// Dwell for merge-queue is 3h: nothing at 2h59, stalled at 3h.
	clock.advance(3*time.Hour - time.Minute)
	observe([]string{"stage/100-merge-queue", "author/trusted"}, "head-changed-since-eval")
	if api.writes() != 0 {
		t.Fatalf("stalled too early: added=%v", api.added)
	}
	clock.advance(time.Minute)
	observe([]string{"stage/100-merge-queue", "author/trusted"}, "head-changed-since-eval")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled not set: %v", api.added)
	}

	// New head goes red: 80 → 70, regressed replaces stalled, one comment.
	api.reset()
	observe([]string{"stage/100-merge-queue", "stage/stalled", "author/trusted"}, "check-failing:unit")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/070-fixing", "stage/regressed"}}) {
		t.Fatalf("regressed added=%v", api.added)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/100-merge-queue", "stage/stalled"}) {
		t.Fatalf("regressed removed=%v", api.removed)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 100-merge-queue → 070-fixing: check-failing:unit") {
		t.Fatalf("comments=%v", api.comments)
	}
	// Repeat observation: no second comment, no writes.
	api.reset()
	observe([]string{"stage/070-fixing", "stage/regressed", "author/trusted"}, "check-failing:unit")
	if api.writes() != 0 {
		t.Fatalf("regression re-posted: comments=%v added=%v", api.comments, api.added)
	}
	// Partial recovery (70 → 60) keeps regressed; full recovery clears it.
	api.reset()
	observe([]string{"stage/070-fixing", "stage/regressed", "author/trusted"}, "required-check-missing:unit")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/050-review-pending"}}) || len(api.comments) != 0 {
		t.Fatalf("partial recovery added=%v comments=%v", api.added, api.comments)
	}
	api.reset()
	observe([]string{"stage/050-review-pending", "stage/regressed", "author/trusted"}, "head-changed-since-eval")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/100-merge-queue"}}) || !reflect.DeepEqual(api.removed, []string{"stage/050-review-pending", "stage/regressed"}) {
		t.Fatalf("recovery added=%v removed=%v", api.added, api.removed)
	}
}

// A stall in awaiting-merge-admission points at the lane, so it earns one
// comment carrying the lane's last skip reason; other stalls do not.
func TestApplyStageLabelsAwaitingAdmissionStallNote(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	approve := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	observe := func(labels []string, reason string, err error) {
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(11, labels...), reason, false, err)
		e.noteStageVerdict("acme", "widget", 11, approve)
		e.noteStageAuthor("acme", "widget", 11, stagelabels.AuthorTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observe(nil, "review-state-check", errors.New("boom"))
	if !reflect.DeepEqual(api.added, [][]string{{"author/trusted", "stage/080-awaiting-merge-admission"}}) {
		t.Fatalf("added=%v", api.added)
	}
	api.reset()
	clock.advance(30 * time.Minute)
	observe([]string{"stage/080-awaiting-merge-admission", "author/trusted"}, "review-state-check", errors.New("boom"))
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled added=%v", api.added)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 080-awaiting-merge-admission stalled after 30m0s: review-state-check") {
		t.Fatalf("stall note=%v", api.comments)
	}
	api.reset()
	clock.advance(time.Hour)
	observe([]string{"stage/080-awaiting-merge-admission", "stage/stalled", "author/trusted"}, "review-state-check", errors.New("boom"))
	if api.writes() != 0 || len(api.comments) != 0 {
		t.Fatalf("stall note must be posted once: added=%v comments=%v", api.added, api.comments)
	}
	// Lane admits the PR: forward to the merge queue, stalled cleared, no note.
	api.reset()
	observe([]string{"stage/080-awaiting-merge-admission", "stage/stalled", "author/trusted"}, contributorLaneReasonSoaking+"(10m)", nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/100-merge-queue"}}) || len(api.comments) != 0 {
		t.Fatalf("admission added=%v comments=%v", api.added, api.comments)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/080-awaiting-merge-admission", "stage/stalled"}) {
		t.Fatalf("admission removed=%v", api.removed)
	}
}

func TestApplyStageLabelsDisabledIsSilent(t *testing.T) {
	srv := newStageAPI(t, &stageAPI{})
	defer srv.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	e := New(client, Options{StageTracker: NewStageTracker()})
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(1), "check-failing:x", false, nil)
	if res, err := e.ApplyStageLabels(context.Background()); err != nil || res.Items != 0 {
		t.Fatalf("disabled: %+v %v", res, err)
	}
}

// TestApplyStageLabelsRecordsVerdictReason reproduces hivecommons/hive#12403:
// the swarm requested changes at the head while the lane's own reason was a
// CI wait, so the PR is 072 — and the stored reason, the regression
// comment and the log must say so rather than quoting the CI wait.
func TestApplyStageLabelsRecordsVerdictReason(t *testing.T) {
	api := &stageAPI{}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	reject := ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}

	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12403), "head-changed-since-eval", false, nil)
	e.noteStageAuthor("acme", "widget", 12403, stagelabels.AuthorTrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	api.reset()
	e.recordStage(stageLaneTrusted, "acme", "acme", "widget", stagePR(12403, "stage/100-merge-queue", "author/trusted"), "untrusted-author-role", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12403, "stage/100-merge-queue", "author/trusted"), "pending: head pushed 1m0s ago (< min_head_age)", false, nil)
	e.noteStageAuthor("acme", "widget", 12403, stagelabels.AuthorTrusted)
	e.noteStageVerdict("acme", "widget", 12403, reject)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/072-returned-to-contributor", "stage/regressed"}}) {
		t.Fatalf("added=%v", api.added)
	}
	rec, _ := store.Get(stageKey("acme", "widget", 12403))
	if rec.Reason != "reviewer-verdict:changes_requested" {
		t.Fatalf("stored reason %q", rec.Reason)
	}
	if len(api.comments) == 0 || !strings.Contains(api.comments[0], "stage 100-merge-queue → 072-returned-to-contributor: reviewer-verdict:changes_requested") {
		t.Fatalf("comments=%v", api.comments)
	}
}

// hivecommons/hive#12538 (seen on #12457): the lane gates on required
// checks only, so it soaks a PR whose rollup still shows a red optional
// check and its reason reads as queued/pending. Any red check on the head
// is red CI: 070/072, never 080 or 100. A red required check is already the
// lane's reason; a red optional one is named; a note left for an older head
// does not bind the new one.
func TestClassifyStageRedCheckIsRepairStageNever080Or100(t *testing.T) {
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	red := approve
	red.RedChecks, red.RedChecksHead = []string{"relative links and anchors resolve"}, "sha"
	named := "optional-check-failing:relative links and anchors resolve"
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		reason string
	}{
		// would be 100
		{name: "soaking with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h39m)", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "merge-failed with red optional", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "merge-failed", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "self-authored release-hold with red optional", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "release-hold", Detail: red}, want: stagelabels.Fixing, reason: named},
		// would be 080
		{name: "author side done with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "lane error with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		// would be 050/060
		{name: "pending with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:lint", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "no verdict with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict, Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		// red required: the lane's own reason
		{name: "red required", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build-gate", Detail: red}, want: stagelabels.ReturnedToContributor},
		{name: "red required self-authored", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "required-check-failing:build-gate", Detail: approve}, want: stagelabels.Fixing},
		// green
		{name: "soaking all green", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h39m)", Detail: approve}, want: stagelabels.MergeQueue},
		{name: "author side done all green", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Detail: approve}, want: stagelabels.AwaitingMergeAdmission},
		{name: "note for an older head does not bind", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha2", Reason: contributorLaneReasonSoaking + "(1h)", Detail: red}, want: stagelabels.MergeQueue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStage(tc.obs)
			if got.Stage != tc.want || got.Reason != tc.reason {
				t.Fatalf("classifyStage=%+v want stage %v reason %q", got, tc.want, tc.reason)
			}
			if tc.want != stagelabels.AwaitingMergeAdmission && got.Rank != stageRankOwned {
				t.Fatalf("rank=%d want owned", got.Rank)
			}
		})
	}
	// Modifiers that park the PR still outrank a red optional check.
	held := classifyStage(stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "label:hold", Detail: red})
	if held.Stage != stagelabels.NeedsDecision || !held.Human {
		t.Fatalf("held=%+v want needs-decision (human)", held)
	}
}

// hivecommons/hive#12538 end to end: a PR soaking behind a red optional
// check moves 100 → 072 (regressed, check named in the comment and store
// reason); a PR awaiting admission moves 080 → 072 the same way; once the
// check is green the same soaking reason yields 100; and a head push drops
// the old head's note.
func TestApplyStageLabelsRedCheckDisplacesMergeQueueAndAwaitingAdmission(t *testing.T) {
	api := &stageAPI{}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	approve := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	soaking := contributorLaneReasonSoaking + "(remaining=1h39m0s)"
	redName := "relative links and anchors resolve"
	key := stageKey("acme", "widget", 12457)
	cycle := func(pr *gh.PullRequest, reason string, red []string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageAuthor("acme", "widget", 12457, stagelabels.AuthorTrusted)
		e.noteStageVerdict("acme", "widget", 12457, approve)
		e.noteStageRedChecks("acme", "widget", 12457, "sha", red)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Earlier cycle: all green, soaking → 100 (already on the PR).
	cycle(stagePR(12457, "stage/100-merge-queue", "author/trusted"), soaking, nil)
	if len(api.added) != 0 || len(api.removed) != 0 {
		t.Fatalf("green soak changed labels: added=%v removed=%v", api.added, api.removed)
	}

	// An optional check on the same head went red: 100 → 072, named.
	cycle(stagePR(12457, "stage/100-merge-queue", "author/trusted"), soaking, []string{redName})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/072-returned-to-contributor", "stage/regressed"}}) || !reflect.DeepEqual(api.removed, []string{"stage/100-merge-queue"}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	rec, _ := store.Get(key)
	if rec.Reason != "optional-check-failing:"+redName {
		t.Fatalf("stored reason %q", rec.Reason)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 100-merge-queue → 072-returned-to-contributor: optional-check-failing:"+redName) {
		t.Fatalf("comments=%v", api.comments)
	}

	// Same head, check re-run green: the lane's soaking reason is 100 again.
	cycle(stagePR(12457, "stage/072-returned-to-contributor", "stage/regressed", "author/trusted"), soaking, nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/100-merge-queue"}}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}

	// Red again while the lane stops on a reason no stage claims (would be
	// 080): still 072.
	cycle(stagePR(12457, "stage/100-merge-queue", "author/trusted"), "review-state-check", []string{redName})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/072-returned-to-contributor", "stage/regressed"}}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	cycle(stagePR(12457, "stage/080-awaiting-merge-admission", "author/trusted"), "review-state-check", []string{redName})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/072-returned-to-contributor", "stage/regressed"}}) || !reflect.DeepEqual(api.removed, []string{"stage/080-awaiting-merge-admission"}) {
		t.Fatalf("080 with red CI: added=%v removed=%v", api.added, api.removed)
	}

	// New head pushed: a note recorded for the old head does not bind it.
	cycle(stagePR(12457, "stage/072-returned-to-contributor", "stage/regressed", "author/trusted"), soaking, nil)
	pr := stagePR(12457, "stage/100-merge-queue", "author/trusted")
	pr.Head.SHA = gh.Ptr("sha2")
	cycle(pr, soaking, []string{redName})
	if len(api.added) != 0 || len(api.removed) != 0 {
		t.Fatalf("stale note changed labels: added=%v removed=%v", api.added, api.removed)
	}
}

// hivecommons/hive#12502: every observation records the PR author on the
// stage store so the dashboard can list "PRs returned to this contributor".
func TestApplyStageLabelsRecordsAuthor(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)

	pr := stagePR(21)
	pr.User = &gh.User{Login: gh.Ptr("Alice")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Author != "alice" {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	if got := store.ByAuthor("alice", stagelabels.ReturnedToContributor); len(got) != 1 {
		t.Fatalf("ByAuthor = %+v", got)
	}
}

// hivecommons/hive#12512: the lane's snapshot of the PR is from the start of
// the cycle; by the time the labels are written another writer may have put
// a second stage on it. Enforce plans from a fresh read, so the write
// leaves exactly one stage and the result counts the heal.
func TestApplyStageLabels_HealsDuplicateStage(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 19, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)

	// Snapshot: 050 + contributor. Truth drifted: 020 landed as well.
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(21, "author/contributor", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageAuthor("acme", "widget", 21, stagelabels.AuthorContributor)
	setStagePRTruth(21, "author/contributor", "stage/050-review-pending", "stage/020-claimed")
	// Snapshot says nothing changes for 050/contributor, so without a
	// reason to write there is no read: the duplicate waits for a write.
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gets := api.getCount(); gets != 0 || res.Healed != 0 {
		t.Fatalf("no planned write must mean no read; gets=%d res=%+v", gets, res)
	}

	// Now the lane moves it to 072: the write re-reads and strips 020 too.
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(21, "author/contributor", "stage/050-review-pending"), "changes-requested", false, nil)
	e.noteStageAuthor("acme", "widget", 21, stagelabels.AuthorContributor)
	setStagePRTruth(21, "author/contributor", "stage/050-review-pending", "stage/020-claimed")
	api.reset()
	res, err = e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gets := api.getCount(); res.Healed != 1 || gets != 1 {
		t.Fatalf("healed=%d gets=%d, want one heal from one fresh read (%+v)", res.Healed, gets, res)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/020-claimed", "stage/050-review-pending"}) {
		t.Fatalf("removed %v, want both 020 and 050", api.removed)
	}
	if len(api.added) != 1 || strings.Join(api.added[0], ",") != "stage/072-returned-to-contributor" {
		t.Fatalf("added %v, want only 072", api.added)
	}
}

// hivecommons/hive#12545: agent/<lane> (routing) and hive/<id> (provenance)
// mean "the hive owns / authored this PR". A review-fix kick's `gh pr edit
// --body` on a contributor's PR stamped both onto it; the reconciler strips
// them from any PR the App did not author, and leaves self-authored PRs alone.
func TestStrayOwnershipLabels(t *testing.T) {
	labels := []string{"agent/scanner", "hive/oke-11", "stage/050-review-pending", "author/trusted", "hive/other-hive"}
	got := strayOwnershipLabels(labels, "mendezr", "kubestellar-hive[bot]", "oke-11")
	if !reflect.DeepEqual(got, []string{"agent/scanner", "hive/oke-11"}) {
		t.Fatalf("contributor PR: stray = %v", got)
	}
	if got := strayOwnershipLabels(labels, "kubestellar-hive[bot]", "kubestellar-hive[bot]", "oke-11"); got != nil {
		t.Fatalf("self-authored PR must keep its labels, got %v", got)
	}
	if got := strayOwnershipLabels(labels, "kubestellar-hive", "kubestellar-hive[bot]", "oke-11"); got != nil {
		t.Fatalf("[bot] suffix must be tolerated, got %v", got)
	}
	if got := strayOwnershipLabels(labels, "", "kubestellar-hive[bot]", "oke-11"); got != nil {
		t.Fatalf("unknown author must fail closed, got %v", got)
	}
	if got := strayOwnershipLabels(labels, "mendezr", "", "oke-11"); got != nil {
		t.Fatalf("unknown App login must fail closed, got %v", got)
	}
	if got := strayOwnershipLabels(labels, "mendezr", "kubestellar-hive[bot]", ""); !reflect.DeepEqual(got, []string{"agent/scanner"}) {
		t.Fatalf("no hive id: only agent/* is stray, got %v", got)
	}
}

func TestApplyStageLabels_StripsStrayOwnershipLabelsFromContributorPR(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, HiveID: "oke-11"} }

	pr := stagePR(12495, "author/trusted", "stage/050-review-pending", "agent/scanner", "hive/oke-11")
	pr.User = &gh.User{Login: gh.Ptr("mendezr")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageAuthor("acme", "widget", 12495, stagelabels.AuthorTrusted)
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	removed := append([]string(nil), api.removed...)
	api.mu.Unlock()
	sort.Strings(removed)
	if !reflect.DeepEqual(removed, []string{"agent/scanner", "hive/oke-11"}) {
		t.Fatalf("removed %v, want the two stray ownership labels (res %+v)", removed, res)
	}
	if res.Writes < 2 {
		t.Fatalf("writes = %d, want the removals counted (%+v)", res.Writes, res)
	}

	// Self-authored PR with the same labels: nothing is stripped.
	api.reset()
	pr = stagePR(7, "stage/050-review-pending", "agent/scanner", "hive/oke-11")
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-pending", false, nil)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, l := range api.removed {
		if strings.HasPrefix(l, "agent/") || strings.HasPrefix(l, "hive/") {
			t.Fatalf("self-authored PR lost %q", l)
		}
	}
}

// hivecommons/hive#12557: a gate flapping between merge-queue and a pending
// stage on one unchanged head used to post a stage-regressed comment every
// sweep, because each recover re-armed the dedupe. One comment per head.
func TestApplyStageLabelsRegressionFlapOnSameHeadCommentsOnce(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 17, 15, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	observe := func(labels []string, reason string) {
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(12423, labels...), reason, false, nil)
		e.noteStageAuthor("acme", "widget", 12423, stagelabels.AuthorTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	queued := []string{"stage/100-merge-queue", "author/trusted"}
	held := []string{"stage/050-review-pending", "stage/regressed", "author/trusted"}

	observe(nil, "head-changed-since-eval")
	api.reset()
	comments := 0
	for i := 0; i < 4; i++ {
		clock.advance(6 * time.Minute)
		observe(queued, "pending: head pushed 54s ago (< min_head_age)")
		comments += len(api.comments)
		api.reset()
		clock.advance(6 * time.Minute)
		observe(held, "head-changed-since-eval")
		comments += len(api.comments)
		api.reset()
	}
	if comments != 1 {
		t.Fatalf("regression comments across 4 flaps on one head = %d, want 1", comments)
	}
}
