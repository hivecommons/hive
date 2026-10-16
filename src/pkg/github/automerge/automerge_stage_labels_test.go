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
			api.labels[body["name"]] = body["color"]
			api.edits = append(api.edits, body["name"]+"="+body["color"])
			_ = json.NewEncoder(w).Encode(body)
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

func (a *stageAPI) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.added, a.removed, a.comments, a.edits, a.creates = nil, nil, nil, nil, nil
}

func stagePR(number int, labels ...string) *gh.PullRequest {
	pr := &gh.PullRequest{Number: gh.Ptr(number), Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha")}}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return pr
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
	})
	return e, tracker, store
}

func TestClassifyStageMapping(t *testing.T) {
	approve := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}}
	reject := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	incomplete := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}}
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
		{name: "hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "label:hold"}, want: stagelabels.NeedsHuman, rank: stageRankOwned},
		{name: "sentinel blocked", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked}, want: stagelabels.NeedsHuman, rank: stageRankOwned},
		{name: "needs-rebase hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "excluded-label:needs-rebase", Labels: []string{"needs-rebase"}}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "no claim is a weak admission opinion", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim}, want: stagelabels.NeedsHuman, rank: stageRankAdmission},
		{name: "claim tier below require_tiers", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier}, want: stagelabels.NeedsHuman, rank: stageRankAdmission},
		{name: "no claim but needs-rebase", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Labels: []string{"needs-rebase"}}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "untrusted role weak", obs: stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}, want: stagelabels.NeedsHuman, rank: stageRankRole},
		{name: "human merge path", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonHumanMergePath}, want: stagelabels.NeedsHuman, rank: stageRankOwned},
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
	if v.Stage != stagelabels.NeedsHuman || author != stagelabels.AuthorUntrusted {
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
			stage: stagelabels.NeedsHuman, rank: stageRankAdmission, author: stagelabels.AuthorContributor, lane: stageLaneContributor,
		},
		{
			name: "unknown author with no claim is needs-human untrusted",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Author: stagelabels.AuthorUntrusted}},
			},
			stage: stagelabels.NeedsHuman, rank: stageRankAdmission, author: stagelabels.AuthorUntrusted, lane: stageLaneContributor,
		},
		{
			// A hold-class reason applies whoever the author is.
			name: "trusted author sentinel-blocked stays needs-human",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked, Detail: trusted},
			},
			stage: stagelabels.NeedsHuman, rank: stageRankOwned, author: stagelabels.AuthorTrusted, lane: stageLaneContributor,
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

	// Hold applied by a human → needs-human replaces review-pending; the
	// human's hold label is left alone.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "author/contributor", "stage/050-review-pending"), "excluded-label:hold", false, nil)
	e.noteStageAuthor("acme", "widget", 7, stagelabels.AuthorContributor)
	if _, err = e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/090-needs-human"}}) || !reflect.DeepEqual(api.removed, []string{"stage/050-review-pending"}) {
		t.Fatalf("hold transition added=%v removed=%v", api.added, api.removed)
	}

	// Merged → every owned label removed, record forgotten.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "author/contributor", "stage/090-needs-human", "stage/stalled"), "", true, nil)
	if _, err = e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(api.removed)
	if len(api.added) != 0 || !reflect.DeepEqual(api.removed, []string{"author/contributor", "stage/090-needs-human", "stage/stalled"}) {
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
