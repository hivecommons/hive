package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
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
	// commits is what GET /pulls/N/commits answers (one message per
	// commit); a PR not listed has no commits, so its DCO state stays
	// unknown and the test's label expectations are unaffected.
	commits       map[int][]string
	commitDetails map[int][]stageCommitFixture
	commitReads   int
	// extra handles routes beyond labels/comments (returns true when it
	// served the request); the adoption tests use it for refs and pulls.
	extra func(w http.ResponseWriter, r *http.Request) bool
	// issueComments / issueEvents are what GET /issues/N/comments and
	// /issues/N/events answer: the hold-provenance read a held PR takes
	// (hivecommons/hive#15020). Unlisted PRs answer empty lists.
	issueComments map[int][]map[string]any
	issueEvents   map[int][]map[string]any
}

type stageCommitFixture struct {
	message     string
	authorLogin string
	authorName  string
	authorEmail string
	// parents overrides the default single parent "p"; two entries make
	// the commit a merge (served as c<i>, so "c0" names the first commit).
	parents []string
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
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/issues/") && strings.HasSuffix(path, "/comments"):
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/issues/"), "/comments"))
			out := api.issueComments[n]
			if out == nil {
				out = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/issues/") && strings.HasSuffix(path, "/events"):
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/issues/"), "/events"))
			out := api.issueEvents[n]
			if out == nil {
				out = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/") && strings.HasSuffix(path, "/commits"):
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), "/commits"))
			api.commitReads++
			out := []map[string]any{}
			if details := api.commitDetails[n]; len(details) > 0 {
				for i, c := range details {
					var author any
					if c.authorLogin != "" {
						author = map[string]string{"login": c.authorLogin}
					}
					parents := []map[string]string{{"sha": "p"}}
					if len(c.parents) > 0 {
						parents = parents[:0]
						for _, p := range c.parents {
							parents = append(parents, map[string]string{"sha": p})
						}
					}
					out = append(out, map[string]any{"sha": fmt.Sprintf("c%d", i), "author": author, "committer": author, "commit": map[string]any{"message": c.message, "author": map[string]any{"name": c.authorName, "email": c.authorEmail}, "committer": map[string]any{"name": c.authorName, "email": c.authorEmail}}, "parents": parents})
				}
			} else {
				for i, msg := range api.commits[n] {
					out = append(out, map[string]any{"sha": fmt.Sprintf("c%d", i), "commit": map[string]any{"message": msg}, "parents": []map[string]string{{"sha": "p"}}})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
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
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, StalledRequireQuiet: true} },
		StageTracker:     tracker,
		StageStore:       store,
		CheckSummary:     greenChecks,
	})
	return e, tracker, store
}

func TestNormalizeContributorLaneVerdictPreservesPolicyOwnedHolds(t *testing.T) {
	base := ContributorLaneVerdict{
		Configured:     true,
		Verdict:        "requires_human",
		HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."},
	}
	knownHive := base
	knownHive.HiveAuthoredKnown = true
	knownHive.HiveAuthored = true
	if got := normalizeContributorLaneVerdict(knownHive); got.Verdict != "changes_requested" {
		t.Fatalf("known hive verdict normalized to %q, want changes_requested", got.Verdict)
	}
	knownHuman := base
	knownHuman.HiveAuthoredKnown = true
	if got := normalizeContributorLaneVerdict(knownHuman); got.Verdict != "requires_human" {
		t.Fatalf("known human verdict normalized to %q, want requires_human", got.Verdict)
	}
	capped := base
	capped.FixCycleCapReached = true
	if got := normalizeContributorLaneVerdict(capped); got.Verdict != "requires_human" {
		t.Fatalf("capped verdict normalized to %q, want requires_human", got.Verdict)
	}
	if got := normalizeContributorLaneVerdict(base); got.Verdict != "changes_requested" {
		t.Fatalf("legacy verdict normalized to %q, want changes_requested", got.Verdict)
	}
}

func TestResolveStageNormalizesRawConcreteRequiresHumanAcrossObservers(t *testing.T) {
	raw := ContributorLaneVerdict{
		Configured:     true,
		Verdict:        "requires_human",
		HumanSummaries: []string{"correctness requires human: LaneQueuedWork is plumbed but never populated; add the producer in AgentActivityFor."},
	}
	list := []stageObservation{
		{Lane: stageLaneSelf, HeadSHA: "bbb5676", Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: raw}},
		{Lane: stageLaneTrusted, HeadSHA: "bbb5676", Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: raw}},
	}

	got, _, obs := resolveStage(list)

	if got.Stage != stagelabels.Fixing || got.Reason != stagelabels.LaneReasonChangesRequested || obs.Lane != stageLaneSelf {
		t.Fatalf("stage=%s reason=%q lane=%s, want 070 fixing from normalized changes_requested", got.Stage, got.Reason, obs.Lane)
	}
}

func TestPreserveNormalizedFixingStageBlocksRawHumanFlap(t *testing.T) {
	prev := stagelabels.Record{
		Stage:        stagelabels.Fixing,
		Reason:       stagelabels.LaneReasonChangesRequested,
		ActivityHead: "bbb5676",
	}
	obs := stageObservation{HeadSHA: "bbb5676", Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."}}}}
	rawHuman := stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Reason: stageReasonVerdictRequiresHuman, Human: true}

	got, ok := preserveNormalizedFixingStage(prev, true, obs, rawHuman)

	if !ok || got.Stage != stagelabels.Fixing || got.Reason != stagelabels.LaneReasonChangesRequested || got.Human {
		t.Fatalf("guard = (%+v, %v), want preserved fixing verdict", got, ok)
	}
}

func TestPreserveNormalizedFixingStageAllowsCurrentHumanHold(t *testing.T) {
	prev := stagelabels.Record{
		Stage:        stagelabels.Fixing,
		Reason:       stagelabels.LaneReasonChangesRequested,
		ActivityHead: "bbb5676",
	}
	obs := stageObservation{HeadSHA: "bbb5676", Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", HiveAuthoredKnown: true, HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."}}}}
	rawHuman := stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Reason: stageReasonVerdictRequiresHuman, Human: true}

	got, ok := preserveNormalizedFixingStage(prev, true, obs, rawHuman)

	if ok || got.Rank != stageRankNone {
		t.Fatalf("guard = (%+v, %v), want current policy-owned human hold to pass through", got, ok)
	}
}

func TestClassifyStageMapping(t *testing.T) {
	approve := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}}
	reject := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	incomplete := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}}
	human := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}}
	humanApproved := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, HumanApprover: "clubanderson", HumanApprovalHead: "sha"}
	decision := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "approve", DecisionRequested: true, DecisionQuestion: "@clubanderson should decide."}}
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
		{name: "trusted human approval clears requires_human on current head", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict, Detail: humanApproved}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "trusted human approval does not clear requires_human on new head", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "newsha", Reason: contributorLaneReasonNoVerdict, Detail: humanApproved}, want: stagelabels.NeedsDecision, rank: stageRankOwned, reason: "reviewer-verdict:requires_human"},
		{name: "decision request behind no-verdict hands off to a person", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: decision}, want: stagelabels.NeedsDecision, rank: stageRankOwned, reason: stageReasonDecisionRequested},
		{name: "decision request behind pending ci still hands off to a person", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-check-pending:unit", Detail: decision}, want: stagelabels.NeedsDecision, rank: stageRankOwned, reason: stageReasonDecisionRequested},
		{name: "ci pending no verdict", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-pending"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "required check pending no verdict", obs: stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:build-and-test"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "required check pending approved", obs: stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:build-and-test", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "ci pending approved", obs: stageObservation{Lane: stageLaneSelf, Reason: "required-check-missing:unit", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "tide pending is not red", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-check-pending:tide"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "head too young is pending", obs: stageObservation{Lane: stageLaneContributor, Reason: "pending: head pushed 33s ago (< min_head_age)"}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "self ci red", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-failing:unit"}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "contributor ci red", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: "required-check-failing:unit"}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "trusted conflicting", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneTrusted, Reason: "conflicting"}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		// The author-blocked outcome splits by author kind (FixingStageFor):
		// a human's PR → 071, a clanker's → 072, the hive's own → 070; the
		// kind is unknown until the apply pass, when a non-self PR reads 071.
		{name: "human ci red", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneContributor, Reason: "required-check-failing:unit"}, want: stagelabels.NeedsAuthor, rank: stageRankOwned},
		{name: "human conflicting", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneTrusted, Reason: "conflicting"}, want: stagelabels.NeedsAuthor, rank: stageRankOwned},
		{name: "human changes requested", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneContributor, Reason: contributorLaneReasonChangesRequested}, want: stagelabels.NeedsAuthor, rank: stageRankOwned},
		{name: "hive kind ci red on contributor lane", obs: stageObservation{AuthorKind: authorkind.Hive, Lane: stageLaneContributor, Reason: "required-check-failing:unit"}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "unknown kind ci red", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-check-failing:unit"}, want: stagelabels.NeedsAuthor, rank: stageRankOwned},
		// The trusted-author lane's enforcing review gate: held for the
		// swarm is 060, never no opinion (the PR sat unlabelled otherwise).
		{name: "review gate no bundle is review pending", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneTrusted, Reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeNoBundle}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "review gate awaiting approval is review pending", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneTrusted, Reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting}, want: stagelabels.ReviewPending, rank: stageRankOwned},
		{name: "review gate changes requested is the author's", obs: stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneTrusted, Reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested + ":security"}, want: stagelabels.NeedsAuthor, rank: stageRankOwned, reason: stagelabels.LaneReasonChangesRequested},
		{name: "self changes requested", obs: stageObservation{Lane: stageLaneSelf, Reason: "check-pending", Detail: reject}, want: stagelabels.Fixing, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "changes requested while head too young", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: "pending: head pushed 1m0s ago (< min_head_age)", Detail: reject}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "changes requested behind no-verdict", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: reject}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned, reason: "reviewer-verdict:changes_requested"},
		{name: "contributor changes requested", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: contributorLaneReasonChangesRequested}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "soaking is pre-merge soak", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSoaking + "(remaining=30m)"}, want: stagelabels.PreMergeSoak, rank: stageRankOwned},
		{name: "soak complete waiting on github is merge queue", obs: stageObservation{Lane: stageLaneContributor, Reason: "required-checks-expected"}, want: stagelabels.MergeQueue, rank: stageRankOwned},
		{name: "merge pool", obs: stageObservation{Lane: stageLaneSelf, Reason: "head-changed-since-eval"}, want: stagelabels.MergeQueue, rank: stageRankOwned},
		{name: "hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "label:hold"}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "workflow approval held", obs: stageObservation{Lane: stageLaneSelf, Reason: workflowApprovalRequiredPrefix + "build"}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "sentinel blocked", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "needs-rebase hold", obs: stageObservation{Lane: stageLaneSelf, Reason: "excluded-label:needs-rebase", Labels: []string{"needs-rebase"}}, want: stagelabels.Fixing, rank: stageRankOwned},
		{name: "no claim is a weak admission opinion", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim}, want: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission},
		{name: "claim tier below require_tiers", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier}, want: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission},
		{name: "no claim but needs-rebase", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Labels: []string{"needs-rebase"}}, want: stagelabels.ReturnedToContributor, rank: stageRankOwned},
		{name: "untrusted role weak", obs: stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}, want: stagelabels.ReadyForHumanMerge, rank: stageRankRole},
		{name: "human merge path", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonHumanMergePath}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		// A breaking-titled / needs-owner-merge PR owes no decision, only
		// the owner's merge click: an OWNED 090 whatever the lane
		// (hivecommons/hive#15442), fixing while it wears needs-rebase.
		{name: "human merge required is owned 090", obs: stageObservation{Lane: stageLaneTrusted, Reason: autoMergeReasonHumanMergeRequired, Detail: approve}, want: stagelabels.ReadyForHumanMerge, rank: stageRankOwned},
		{name: "human merge required self lane", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonHumanMergeRequired}, want: stagelabels.ReadyForHumanMerge, rank: stageRankOwned},
		{name: "human merge required needs-rebase", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonHumanMergeRequired, Labels: []string{"needs-rebase"}}, want: stagelabels.Fixing, rank: stageRankOwned},
		// A merge GitHub refuses for a missing approving review or a
		// disabled merge method is not imminent: a person acts (085).
		{name: "merge refused review required", obs: stageObservation{Lane: stageLaneSelf, Reason: autoMergeReasonMergeRefusedReview, Detail: approve}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "merge refused method not allowed", obs: stageObservation{Lane: stageLaneContributor, Reason: autoMergeReasonMergeRefusedMethod, Detail: approve}, want: stagelabels.NeedsDecision, rank: stageRankOwned},
		{name: "merge queue required stays merge queue", obs: stageObservation{Lane: stageLaneContributor, Reason: autoMergeReasonMergeQueueRequired, Detail: approve}, want: stagelabels.MergeQueue, rank: stageRankOwned},
		{name: "approved green unclassified reason", obs: stageObservation{Lane: stageLaneContributor, Reason: "something-new", Detail: approve}, want: stagelabels.AwaitingMergeAdmission, rank: stageRankRole},
		{name: "approved green lane error", obs: stageObservation{Lane: stageLaneContributor, Reason: "review-state-check", Err: true, Detail: approve}, want: stagelabels.AwaitingMergeAdmission, rank: stageRankRole},
		{name: "unapproved unclassified reason", obs: stageObservation{Lane: stageLaneContributor, Reason: "something-new"}},
		{name: "approved but ci pending stays reviewed", obs: stageObservation{Lane: stageLaneContributor, Reason: "check-pending", Detail: approve}, want: stagelabels.Reviewed, rank: stageRankOwned},
		{name: "approved soaking is pre-merge soak", obs: stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSoaking + "(remaining=5m)", Detail: approve}, want: stagelabels.PreMergeSoak, rank: stageRankOwned},
		{name: "approved soak complete is merge queue", obs: stageObservation{Lane: stageLaneContributor, Reason: "head-changed-since-eval", Detail: approve}, want: stagelabels.MergeQueue, rank: stageRankOwned},
	}
	for _, tc := range cases {
		got := classifyStage(tc.obs)
		if got.Stage != tc.want || got.Rank != tc.rank || got.Terminal != tc.term || got.Reason != tc.reason {
			t.Errorf("%s: got %+v want stage=%v rank=%d terminal=%v reason=%q", tc.name, got, tc.want, tc.rank, tc.term, tc.reason)
		}
		// Owner decision: 045 is the ISSUE parking bay; a PR parks at 085
		// and PR derivation never emits an issue-only stage.
		if got.Stage != stagelabels.None && !got.Stage.Applies(stagelabels.PRItem) {
			t.Errorf("%s: PR derivation emitted issue-only stage %v", tc.name, got.Stage)
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
		{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{Role: stagelabels.RoleContributor}},
	})
	if v.Stage != stagelabels.ReviewPending || author != stagelabels.RoleContributor {
		t.Fatalf("got %v/%v", v.Stage, author)
	}
	// Only the weak opinion: needs-human + role/untrusted.
	v, author, _ = resolveStage([]stageObservation{{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}})
	if v.Stage != stagelabels.ReadyForHumanMerge || author != stagelabels.RoleUntrusted {
		t.Fatalf("weak-only got %v/%v", v.Stage, author)
	}
	// Self lane outranks the contributor lane's hand-off.
	v, _, _ = resolveStage([]stageObservation{
		{Lane: stageLaneSelf, Reason: "check-failing:unit", Detail: stageDetail{Role: stagelabels.RoleTrusted}},
		{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored},
	})
	if v.Stage != stagelabels.Fixing {
		t.Fatalf("self lane got %v", v.Stage)
	}
}

// TestResolveStageAppAuthoredHandoffsYieldToSelfLane reproduces
// hivecommons/hive#14482: the trusted-author and contributor-lane tiers each
// hand an App-authored PR off under their own reason, and neither hand-off
// has a say in the stage — the self-authored lane's review-gate hold owns
// it and is what the stage write is attributed to.
func TestResolveStageAppAuthoredHandoffsYieldToSelfLane(t *testing.T) {
	list := []stageObservation{
		{Lane: stageLaneSelf, Reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeNoBundle, HeadSHA: "cdf5961", Detail: stageDetail{Role: stagelabels.RoleTrusted}},
		{Lane: stageLaneTrusted, Reason: trustedAuthorReasonAppAuthored, HeadSHA: "cdf5961"},
		{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored, HeadSHA: "cdf5961"},
	}
	v, role, obs := resolveStage(list)
	if v.Stage != stagelabels.ReviewPending || v.Rank != stageRankOwned || role != stagelabels.RoleTrusted {
		t.Fatalf("got %v/%d/%v, want review-pending owned by a trusted author", v.Stage, v.Rank, role)
	}
	if obs.Lane != stageLaneSelf {
		t.Fatalf("attributed to lane %q, want %q", obs.Lane, stageLaneSelf)
	}
	for _, o := range list[1:] {
		if got := classifyStage(o); got.Rank != stageRankNone {
			t.Fatalf("%s hand-off %q classified as %+v, want no opinion", o.Lane, o.Reason, got)
		}
	}
}

func TestResolveStageKeepsExistingRoleWhenNoLaneReportsOne(t *testing.T) {
	cases := []struct {
		name   string
		list   []stageObservation
		author stagelabels.Role
	}{
		{
			name:   "owned stage keeps trusted label",
			list:   []stageObservation{{Lane: stageLaneTrusted, Reason: "required-check-pending:build", Labels: []string{"role/trusted"}}},
			author: stagelabels.RoleTrusted,
		},
		{
			name:   "weak stage keeps trusted label",
			list:   []stageObservation{{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Labels: []string{"role/trusted"}}},
			author: stagelabels.RoleTrusted,
		},
		{
			name:   "no label and no lane tier is untrusted",
			list:   []stageObservation{{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}},
			author: stagelabels.RoleUntrusted,
		},
		{
			// Owned stage: the label is only a fallback, so a lane-reported
			// tier wins. (On weak stages the label is merged first and a
			// trusted label is sticky by Role.Merge — pre-existing.)
			name:   "lane tier overrides the existing label on an owned stage",
			list:   []stageObservation{{Lane: stageLaneContributor, Reason: "required-check-pending:build", Labels: []string{"role/trusted"}, Detail: stageDetail{Role: stagelabels.RoleContributor}}},
			author: stagelabels.RoleContributor,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got, _ := resolveStage(tc.list); got != tc.author {
				t.Fatalf("role = %v, want %v", got, tc.author)
			}
		})
	}
}

func TestResolveStageTrustedHumanApprovalClearsRequiresHuman(t *testing.T) {
	humanApproved := stageDetail{
		VerdictKnown:      true,
		Verdict:           ContributorLaneVerdict{Configured: true, Verdict: "requires_human"},
		HumanApprover:     "clubanderson",
		HumanApprovalHead: "sha7",
		Role:              stagelabels.RoleContributor,
	}
	v, author, obs := resolveStage([]stageObservation{{
		Lane:    stageLaneContributor,
		HeadSHA: "sha7",
		Reason:  contributorLaneReasonSoaking + "(remaining=30m)",
		Detail:  humanApproved,
	}})
	if v.Stage != stagelabels.PreMergeSoak || v.Reason != "reviewer-verdict:requires_human:human-approved@clubanderson" || author != stagelabels.RoleContributor || obs.Lane != stageLaneContributor {
		t.Fatalf("current head got stage=%v reason=%q author=%v lane=%s", v.Stage, v.Reason, author, obs.Lane)
	}

	v, _, _ = resolveStage([]stageObservation{{
		Lane:    stageLaneContributor,
		HeadSHA: "sha8",
		Reason:  contributorLaneReasonSoaking + "(remaining=30m)",
		Detail:  humanApproved,
	}})
	if v.Stage != stagelabels.NeedsDecision || v.Reason != "reviewer-verdict:requires_human" {
		t.Fatalf("new head got stage=%v reason=%q, want 085 requires_human", v.Stage, v.Reason)
	}
}

// TestResolveStageAdmissionOpinions covers the weak "cannot vouch for the
// author" opinions (hivecommons/hive#12403, #12497): a lane that does not
// admit the author only says needs-human when no lane admits them.
func TestResolveStageAdmissionOpinions(t *testing.T) {
	trusted := stageDetail{Role: stagelabels.RoleTrusted}
	requiresHumanDetail := stageDetail{Role: stagelabels.RoleContributor, VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}}
	cases := []struct {
		name   string
		list   []stageObservation
		stage  stagelabels.Stage
		rank   int
		author stagelabels.Role
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
			rank: stageRankNone, author: stagelabels.RoleTrusted, lane: stageLaneTrusted,
		},
		{
			// #12497: maintainer PR; the trusted-author lane owns it, the
			// contributor lane's no-claim is only its own inability to admit.
			name: "maintainer with trusted lane error and contributor no-claim",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "fetch-pr", Err: true, Detail: trusted},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Role: stagelabels.RoleUntrusted}},
			},
			rank: stageRankNone, author: stagelabels.RoleTrusted, lane: stageLaneTrusted,
		},
		{
			name: "maintainer ci pending beats contributor no-claim",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "required-check-pending:golangci-lint", Detail: trusted},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: trusted},
			},
			stage: stagelabels.ReviewPending, rank: stageRankOwned, author: stagelabels.RoleTrusted, lane: stageLaneTrusted,
		},
		{
			// Contributor tier below require_tiers: nobody admits, 090 is right
			// and the contributor lane's reason is the one to show.
			name: "contributor tier is needs-human from the contributor lane",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier, Detail: stageDetail{Role: stagelabels.RoleContributor}},
			},
			stage: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission, author: stagelabels.RoleContributor, lane: stageLaneContributor,
		},
		{
			name: "unknown author with no claim is needs-human untrusted",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Role: stagelabels.RoleUntrusted}},
			},
			stage: stagelabels.ReadyForHumanMerge, rank: stageRankAdmission, author: stagelabels.RoleUntrusted, lane: stageLaneContributor,
		},
		{
			// A hold-class reason applies whoever the author is.
			name: "trusted author sentinel-blocked is needs-decision",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked, Detail: trusted},
			},
			stage: stagelabels.NeedsDecision, rank: stageRankOwned, author: stagelabels.RoleTrusted, lane: stageLaneContributor,
		},
		{
			// #13115: a requires_human verdict on an admission reason is 085,
			// as weak as the reason that carries it, from the lane that
			// cannot vouch for the author.
			name: "requires_human with author-association is needs-decision from the contributor lane",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Detail: requiresHumanDetail},
				{Lane: stageLaneContributor, Reason: contributorLaneReasonAuthorAssociation, Detail: requiresHumanDetail},
			},
			stage: stagelabels.NeedsDecision, rank: stageRankAdmission, author: stagelabels.RoleContributor, lane: stageLaneContributor,
		},
		{
			// The trusted lane's role check must not outrank the contributor
			// lane's own evaluation of the head: a conflict is the author's
			// problem first, whatever the reviewer said.
			name: "requires_human with role check loses to contributor conflicting",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Detail: requiresHumanDetail},
				{Lane: stageLaneContributor, Reason: "conflicting", Detail: requiresHumanDetail},
			},
			stage: stagelabels.NeedsAuthor, rank: stageRankOwned, author: stagelabels.RoleContributor, lane: stageLaneContributor,
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
			e.noteStageRole("acme", "widget", 12484, stagelabels.RoleTrusted)
		}
		contributor := func() {
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12484), contributorLaneReasonNoLinkedIssue, false, nil)
			e.noteStageRole("acme", "widget", 12484, stagelabels.RoleFromTier("", map[string]bool{"trusted": true}))
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
		if len(api.added) != 1 || !reflect.DeepEqual(api.added[0], []string{"author/human", "role/trusted", "stage/050-review-pending"}) {
			t.Fatalf("trustedFirst=%v added %v", trustedFirst, api.added)
		}
		srv.Close()
	}
}

func TestApplyStageLabelsContributorConflictBeatsTrustTier(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, apiURLOf(t, api), clock)

	pr := nudgePR(12822, "sha-conflict")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "conflicting", false, nil)
	e.noteStageRole("acme", "widget", 12822, stagelabels.RoleContributor)
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	added := api.added
	if len(added) != 1 || !slices.Contains(added[0], "stage/072-returned-to-contributor") || slices.Contains(added[0], "stage/090-ready-for-human-merge") {
		t.Fatalf("conflicting contributor staged incorrectly: result=%+v added=%v", res, added)
	}
	rec, ok := e.stageStoreOrShared().Get(stageKey("acme", "widget", 12822))
	if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != "conflicting" {
		t.Fatalf("record = %+v, want 072 conflicting", rec)
	}
}

func TestApplyStageLabelsUnknownMergeabilityDoesNotFlipToHumanMerge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, apiURLOf(t, api), clock)
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	pr := nudgePR(12822, "sha-unknown", labels...)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "mergeable-unknown", false, nil)
	e.noteStageRole("acme", "widget", 12822, stagelabels.RoleContributor)
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Writes != 0 || api.writes() != 0 {
		t.Fatalf("unknown mergeability should keep previous stage: result=%+v added=%v removed=%v comments=%v", res, api.added, api.removed, api.comments)
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
	pr := func() *gh.PullRequest {
		return stagePR(number, "role/trusted", "author/human", "stage/050-review-pending")
	}

	// Drain 1: only the contributor lane's weak opinion → nothing written,
	// nothing recorded; the labels the trusted-author lane wrote stand.
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr(), contributorLaneReasonNoClaim, false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleUntrusted)
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
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(number, "role/trusted", "author/human"), "required-check-pending:build-and-test", false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
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
	setStagePRTruth(77)
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(77), "untrusted-author-role", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(77), contributorLaneReasonNoClaim, false, nil)
	e.noteStageRole("acme", "widget", 77, stagelabels.RoleUntrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", "role/untrusted", "stage/085-needs-decision"}}) {
		t.Fatalf("complete drain: added=%v", api.added)
	}

	// A lane disabled for the repo is not waited for.
	api.reset()
	e.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{} }
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(78), contributorLaneReasonNoClaim, false, nil)
	e.noteStageRole("acme", "widget", 78, stagelabels.RoleUntrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", "role/untrusted", "stage/085-needs-decision"}}) {
		t.Fatalf("trusted lane disabled: added=%v", api.added)
	}
}

func TestApplyStageLabelsRoleOwnerStableAcrossLaneOrder(t *testing.T) {
	type laneFn func(*Engine, int, []string)
	withAuthor := func(pr *gh.PullRequest, login string) *gh.PullRequest {
		pr.User = &gh.User{Login: gh.Ptr(login)}
		return pr
	}
	selfHandoff := func(e *Engine, number int, labels []string) {
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), "human"), "not-app-authored", false, nil)
	}
	cases := []struct {
		name   string
		labels []string
		lanes  map[string]laneFn
	}{
		{
			name:   "app-authored",
			labels: []string{"role/trusted", "author/hive", "stage/050-review-pending"},
			lanes: map[string]laneFn{
				"self": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), testHiveAppBotLogin), "required-check-pending:build", false, nil)
					e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
				},
				"trusted": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), testHiveAppBotLogin), "untrusted-author-role", false, nil)
				},
				"contributor": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), testHiveAppBotLogin), contributorLaneReasonAppAuthored, false, nil)
					e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
				},
			},
		},
		{
			name:   "trusted-human",
			labels: []string{"role/trusted", "author/human", "stage/050-review-pending"},
			lanes: map[string]laneFn{
				"self": selfHandoff,
				"trusted": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), "maintainer"), "required-check-pending:build", false, nil)
					e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
				},
				"contributor": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), "maintainer"), contributorLaneReasonNoClaim, false, nil)
					e.noteStageRole("acme", "widget", number, stagelabels.RoleUntrusted)
				},
			},
		},
		{
			name:   "untrusted-contributor",
			labels: []string{"role/untrusted", "author/human", "stage/085-needs-decision"},
			lanes: map[string]laneFn{
				"self": selfHandoff,
				"trusted": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), "outside"), "untrusted-author-role", false, nil)
				},
				"contributor": func(e *Engine, number int, labels []string) {
					e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", withAuthor(stagePR(number, labels...), "outside"), contributorLaneReasonNoClaim, false, nil)
					e.noteStageRole("acme", "widget", number, stagelabels.RoleUntrusted)
				},
			},
		},
	}
	orders := [][]string{
		{"self", "trusted", "contributor"},
		{"self", "contributor", "trusted"},
		{"trusted", "self", "contributor"},
		{"trusted", "contributor", "self"},
		{"contributor", "self", "trusted"},
		{"contributor", "trusted", "self"},
	}
	ctx := context.Background()
	labelWrites := func(api *stageAPI) int {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.added) + len(api.removed)
	}
	for i, tc := range cases {
		for j, order := range orders {
			t.Run(tc.name+"/"+strings.Join(order, "-"), func(t *testing.T) {
				api := &stageAPI{}
				srv := newStageAPI(t, api)
				defer srv.Close()
				clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 30, 0, 0, time.UTC)}
				e, _, _ := newStageEngine(t, srv.URL, clock)
				e.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true} }
				e.contributorLanePolicy = func() ContributorLanePolicy { return ContributorLanePolicy{Enabled: true} }
				number := 12600 + i*10 + j
				for _, lane := range order {
					tc.lanes[lane](e, number, tc.labels)
				}
				if _, err := e.ApplyStageLabels(ctx); err != nil {
					t.Fatal(err)
				}
				if labelWrites(api) != 0 {
					t.Fatalf("combined drain churned labels: added=%v removed=%v comments=%d", api.added, api.removed, len(api.comments))
				}

				api.reset()
				for _, lane := range order {
					tc.lanes[lane](e, number, tc.labels)
					if _, err := e.ApplyStageLabels(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if labelWrites(api) != 0 {
					t.Fatalf("split drains churned labels: added=%v removed=%v comments=%d", api.added, api.removed, len(api.comments))
				}
			})
		}
	}
}

func TestApplyStageLabelsAppAuthoredRoleDoesNotFlapAcrossLanes(t *testing.T) {
	api := &stageAPI{}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 20, 30, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	e.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true} }
	e.contributorLanePolicy = func() ContributorLanePolicy { return ContributorLanePolicy{} }
	ctx := context.Background()
	const number = 12808
	decision := ContributorLaneVerdict{Configured: true, Verdict: "approve", DecisionRequested: true, DecisionQuestion: "@hanthor should decide whether the contribution-preset loader should land before any hivectl command consumes it"}
	appPR := func(labels ...string) *gh.PullRequest {
		pr := stagePR(number, labels...)
		pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
		return pr
	}

	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", appPR(), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
	e.noteStageVerdict("acme", "widget", number, decision)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/hive", "role/trusted", "stage/085-needs-decision"}}) {
		t.Fatalf("initial self-authored write added=%v removed=%v", api.added, api.removed)
	}

	labels := []string{"author/hive", "role/trusted", "stage/085-needs-decision"}
	for i := 0; i < 3; i++ {
		api.reset()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", appPR(labels...), "untrusted-author-role", false, nil)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		if api.writes() != 0 {
			t.Fatalf("trusted-author observation %d flapped labels: added=%v removed=%v comments=%d", i, api.added, api.removed, len(api.comments))
		}

		api.reset()
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", appPR(labels...), contributorLaneReasonNoVerdict, false, nil)
		e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
		e.noteStageVerdict("acme", "widget", number, decision)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		if api.writes() != 0 {
			t.Fatalf("self-authored observation %d flapped labels: added=%v removed=%v comments=%d", i, api.added, api.removed, len(api.comments))
		}
	}
}

func TestApplyStageLabelsNonOwningLaneKeepsRecordedRole(t *testing.T) {
	cases := []struct {
		name          string
		lane          string
		reason        string
		role          stagelabels.Role
		labels        []string
		trustedPolicy bool
		contribPolicy bool
	}{
		{
			name:          "trusted human seen by contributor lane",
			lane:          stageLaneContributor,
			reason:        contributorLaneReasonNoClaim,
			role:          stagelabels.RoleUntrusted,
			labels:        []string{"author/human", "role/trusted", "stage/050-review-pending"},
			contribPolicy: true,
		},
		{
			name:          "untrusted contributor seen by trusted lane",
			lane:          stageLaneTrusted,
			reason:        "untrusted-author-role",
			labels:        []string{"author/human", "role/untrusted", "stage/085-needs-decision"},
			trustedPolicy: true,
		},
	}
	ctx := context.Background()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &stageAPI{}
			srv := newStageAPI(t, api)
			defer srv.Close()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 21, i, 0, 0, time.UTC)}
			e, _, _ := newStageEngine(t, srv.URL, clock)
			e.trustedAuthorPolicy = func() TrustedAuthorPolicy {
				if tc.trustedPolicy {
					return TrustedAuthorPolicy{Enabled: true}
				}
				return TrustedAuthorPolicy{}
			}
			e.contributorLanePolicy = func() ContributorLanePolicy {
				if tc.contribPolicy {
					return ContributorLanePolicy{Enabled: true}
				}
				return ContributorLanePolicy{}
			}
			pr := stagePR(12900+i, tc.labels...)
			pr.User = &gh.User{Login: gh.Ptr("maintainer")}
			e.recordStage(tc.lane, "acme/widget", "acme", "widget", pr, tc.reason, false, nil)
			if tc.role != stagelabels.RoleNone {
				e.noteStageRole("acme", "widget", pr.GetNumber(), tc.role)
			}
			if _, err := e.ApplyStageLabels(ctx); err != nil {
				t.Fatal(err)
			}
			for _, add := range api.added {
				if slices.Contains(add, "role/trusted") || slices.Contains(add, "role/untrusted") {
					t.Fatalf("non-owning lane changed role: added=%v removed=%v", api.added, api.removed)
				}
			}
			if slices.Contains(api.removed, "role/trusted") || slices.Contains(api.removed, "role/untrusted") {
				t.Fatalf("non-owning lane removed role: added=%v removed=%v", api.added, api.removed)
			}
		})
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
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	res, err := e.ApplyStageLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 1 || res.Writes != 3 {
		t.Fatalf("result %+v", res)
	}
	if len(api.added) != 1 || !reflect.DeepEqual(api.added[0], []string{"author/human", "role/contributor", "stage/050-review-pending"}) {
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
	// The new 071 needs-author label is provisioned from the table like
	// any other stage, in the stage colour.
	if api.labels["stage/071-needs-author"] != stagelabels.StageColor || !slices.Contains(api.creates, "stage/071-needs-author") {
		t.Fatalf("071 not provisioned: creates=%v labels=%v", api.creates, api.labels)
	}

	// Same state next cycle (labels now as written): no writes, no re-list.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "role/contributor", "author/human", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if res, err = e.ApplyStageLabels(ctx); err != nil || res.Writes != 0 || api.writes() != 0 || api.lists != 1 {
		t.Fatalf("idempotent pass: res=%+v err=%v writes=%d lists=%d", res, err, api.writes(), api.lists)
	}

	// Hold applied by a human → needs-decision (085) replaces review-pending
	// (no reviewer is configured here, which is itself a decision); the
	// human's hold label is left alone and the entry comment names both.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "role/contributor", "author/human", "stage/050-review-pending"), "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
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
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "role/contributor", "author/human", "stage/085-needs-decision"), "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err = e.ApplyStageLabels(ctx); err != nil || len(api.comments) != 0 || api.writes() != 0 {
		t.Fatalf("085 dedupe: err=%v comments=%q writes=%d", err, api.comments, api.writes())
	}

	// Merged → every pipeline label removed, record forgotten. The
	// author/<kind> label stays: who wrote the PR is a fact about it, not
	// pipeline state, and the hub's revert-rate-by-author-kind reads it.
	api.reset()
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(7, "bug", "hold", "role/contributor", "author/human", "stage/085-needs-decision", "stage/stalled"), "", true, nil)
	if _, err = e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(api.removed)
	if len(api.added) != 0 || !reflect.DeepEqual(api.removed, []string{"role/contributor", "stage/085-needs-decision", "stage/stalled"}) {
		t.Fatalf("terminal added=%v removed=%v", api.added, api.removed)
	}
	if _, ok := store.Get(stageKey("acme", "widget", 7)); ok {
		t.Fatal("terminal item still tracked")
	}
}

func TestApplyStageLabelsStalledAndRegressionHistory(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	key := stageKey("acme", "widget", 9)
	observe := func(labels []string, reason string) {
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(9, labels...), reason, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe(nil, "head-changed-since-eval") // → merge-queue
	api.reset()
	// Dwell for merge-queue is 3h: nothing at 2h59, stalled at 3h of quiet.
	clock.advance(3*time.Hour - time.Minute)
	observe([]string{"stage/100-merge-queue", "role/trusted", "author/human"}, "head-changed-since-eval")
	if api.writes() != 0 {
		t.Fatalf("stalled too early: added=%v", api.added)
	}
	clock.advance(time.Minute)
	observe([]string{"stage/100-merge-queue", "role/trusted", "author/human"}, "head-changed-since-eval")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled not set: %v", api.added)
	}

	// New head goes red: 100 → 070. The backward move is one comment and a
	// history hop, not a label (stage/regressed is retired, #12534); the CI
	// change is activity, so stalled clears.
	api.reset()
	observe([]string{"stage/100-merge-queue", "stage/stalled", "role/trusted", "author/human"}, "check-failing:unit")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/070-fixing"}}) {
		t.Fatalf("regression added=%v", api.added)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/100-merge-queue", "stage/stalled"}) {
		t.Fatalf("regression removed=%v", api.removed)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 100-merge-queue → 070-fixing: check-failing:unit") {
		t.Fatalf("comments=%v", api.comments)
	}
	rec, _ := store.Get(key)
	if n := len(rec.History); n == 0 || !rec.History[n-1].Regressed || rec.History[n-1].From != stagelabels.MergeQueue {
		t.Fatalf("history = %+v", rec.History)
	}
	if !rec.LastActivityAt.Equal(clock.at) {
		t.Fatalf("CI change must count as activity: last=%v now=%v", rec.LastActivityAt, clock.at)
	}
	// Repeat observation: no second comment, no writes; the hive's own
	// comment did not read as outside activity.
	api.reset()
	observe([]string{"stage/070-fixing", "role/trusted", "author/human"}, "check-failing:unit")
	if api.writes() != 0 {
		t.Fatalf("regression re-posted: comments=%v added=%v", api.comments, api.added)
	}
	if rec2, _ := store.Get(key); !rec2.LastActivityAt.Equal(rec.LastActivityAt) {
		t.Fatal("hive's own regression comment moved the quiet clock")
	}
	// A stale stage/regressed left by an older hive is stripped on sight.
	api.reset()
	observe([]string{"stage/070-fixing", "stage/regressed", "role/trusted", "author/human"}, "required-check-missing:unit")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/050-review-pending"}}) || len(api.comments) != 0 {
		t.Fatalf("partial recovery added=%v comments=%v", api.added, api.comments)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/070-fixing", "stage/regressed"}) {
		t.Fatalf("retired label not stripped: removed=%v", api.removed)
	}
	api.reset()
	observe([]string{"stage/050-review-pending", "role/trusted", "author/human"}, "head-changed-since-eval")
	if !reflect.DeepEqual(api.added, [][]string{{"stage/100-merge-queue"}}) || !reflect.DeepEqual(api.removed, []string{"stage/050-review-pending"}) {
		t.Fatalf("recovery added=%v removed=%v", api.added, api.removed)
	}
}

// hivecommons/hive#12417: a trusted-author PR sat at 050-review-pending for
// 6h with CI green and no reviewer verdict, and the engine marked it
// stalled. The wait was on the hive's reviewer, not on a person, so
// stage/stalled must never be applied there; once the verdict exists and
// nobody acts for the dwell it may, and any activity clears it.
func TestApplyStageLabelsReviewPendingNeverStallsWithoutVerdict(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 13, 33, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	key := stageKey("acme", "widget", 12417)
	observe := func(labels []string, reason string, verdict *ContributorLaneVerdict, comments int) {
		pr := stagePR(12417, labels...)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", 12417, stagelabels.RoleTrusted)
		pr.Comments = gh.Ptr(comments)
		e.noteStagePR("acme", "widget", 12417, pr)
		if verdict != nil {
			e.noteStageVerdict("acme", "widget", 12417, *verdict)
		}
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	at050 := []string{"stage/050-review-pending", "role/trusted", "author/human"}

	observe(nil, contributorLaneReasonNoVerdict, nil, 0)
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", "role/trusted", "stage/050-review-pending"}}) {
		t.Fatalf("added=%v", api.added)
	}
	// Six hours, CI green, still no verdict: not stalled, not a comment.
	// Past the 1h dwell the hive-side alert fires instead (HiveHeld count,
	// one warn log), because the holder is the hive's reviewer lane.
	var hiveHeld int
	for i := 0; i < 6; i++ {
		api.reset()
		clock.advance(time.Hour)
		pr := stagePR(12417, at050...)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageRole("acme", "widget", 12417, stagelabels.RoleTrusted)
		e.noteStagePR("acme", "widget", 12417, pr)
		res, err := e.ApplyStageLabels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if api.writes() != 0 {
			t.Fatalf("hour %d: review-pending without a verdict must never stall: added=%v comments=%v", i+1, api.added, api.comments)
		}
		hiveHeld += res.HiveHeld
	}
	if hiveHeld != 6 {
		t.Fatalf("hive-held alert count = %d, want one per cycle past the dwell", hiveHeld)
	}
	rec, _ := store.Get(key)
	if !rec.WaitingOnHive || !rec.HiveHeldWarned {
		t.Fatalf("record should be waiting on the hive with the alert logged once: %+v", rec)
	}
	if stagelabels.Stalled(rec, nil, clock.at) || !stagelabels.HiveHeld(rec, nil, clock.at) {
		t.Fatal("rule inverted: hive-held, not stalled")
	}
	// Checks running is likewise waiting on the hive.
	api.reset()
	clock.advance(2 * time.Hour)
	observe(at050, "required-check-pending:unit", nil, 0)
	if api.writes() != 0 {
		t.Fatalf("checks pending must not stall: added=%v", api.added)
	}
	// A verdict lands that keeps the head at 050 (an incomplete swarm
	// aggregate): the hive has answered, so the PR is no longer waiting on
	// it. Quiet for the 1h dwell → stalled; not a minute before.
	human := ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}
	api.reset()
	observe(at050, contributorLaneReasonNoVerdict, &human, 0)
	if api.writes() != 0 {
		t.Fatalf("verdict arrival is activity, not a stall: added=%v", api.added)
	}
	clock.advance(59 * time.Minute)
	observe(at050, contributorLaneReasonNoVerdict, &human, 0)
	if api.writes() != 0 {
		t.Fatalf("stalled before the quiet dwell: added=%v", api.added)
	}
	clock.advance(time.Minute)
	observe(at050, contributorLaneReasonNoVerdict, &human, 0)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled expected after a quiet dwell with the verdict present: added=%v", api.added)
	}
	// A human comment is activity: stalled clears immediately.
	api.reset()
	clock.advance(time.Minute)
	observe([]string{"stage/050-review-pending", "stage/stalled", "role/trusted", "author/human"}, contributorLaneReasonNoVerdict, &human, 1)
	if !reflect.DeepEqual(api.removed, []string{"stage/stalled"}) || len(api.added) != 0 {
		t.Fatalf("comment must clear stalled: added=%v removed=%v", api.added, api.removed)
	}
	// requires_human (as on #12417: four approves, intent-alignment
	// requires_human) is a hand-off to a person, not a 050 stall: the PR
	// moves to 085-needs-decision at once (hivecommons/hive#12561) and 085
	// has no dwell, so it never stalls however long it sits.
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	api.reset()
	clock.advance(time.Minute)
	observe(at050, contributorLaneReasonNoVerdict, &requiresHuman, 1)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/085-needs-decision"}}) {
		t.Fatalf("requires_human must hand off to 085: added=%v", api.added)
	}
	api.reset()
	clock.advance(48 * time.Hour)
	observe([]string{"stage/085-needs-decision", "role/trusted", "author/human"}, contributorLaneReasonNoVerdict, &requiresHuman, 1)
	if api.writes() != 0 {
		t.Fatalf("085 has no dwell and never stalls: added=%v", api.added)
	}
}

// A contributor-lane admission reason (author-association) with a
// requires_human verdict must reach 085, not the 090 candidate that the
// apply pass drops to 050 where the quiet dwell stalls it
// (hivecommons/hive#13115). The specimen (Danathar/tududi#336) had both
// lanes declining on trust, the verdict hook answering requires_human for
// the head, and nothing reported on that head yet — the fresh-head exit in
// RouteHuman is what produced 050.
func TestApplyStageLabelsRequiresHumanBeatsAdmissionReason(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	e.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return requiresHuman }
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{}, nil
	}
	ctx := context.Background()
	observe := func(labels []string) {
		pr := stagePR(336, labels...)
		// A clanker PR: a person's requires_human routes to 071 instead
		// (hivecommons/hive#15202).
		pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonAuthorAssociation, false, nil)
		e.noteStageRole("acme", "widget", 336, stagelabels.RoleContributor)
		pr.Comments = gh.Ptr(0)
		e.noteStagePR("acme", "widget", 336, pr)
		e.noteStageVerdict("acme", "widget", 336, requiresHuman)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observe([]string{"stage/050-review-pending", "role/contributor", "author/clanker"})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/085-needs-decision"}}) {
		t.Fatalf("requires_human must hand off to 085: added=%v", api.added)
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 336)); !ok || rec.Stage != stagelabels.NeedsDecision || rec.Reason != stageReasonVerdictRequiresHuman {
		t.Fatalf("record = %+v, want 085 %s", rec, stageReasonVerdictRequiresHuman)
	}
	api.reset()
	clock.advance(48 * time.Hour)
	observe([]string{"stage/085-needs-decision", "role/contributor", "author/clanker"})
	if api.writes() != 0 {
		t.Fatalf("085 has no dwell and never stalls: added=%v", api.added)
	}
}

// The sweep's own comments (Dependabot rebase request, stale-approval note,
// hold release) land after the lane's deep fetch and before
// ApplyStageLabels, so the credit must go on this cycle's observation, not
// on the store's last count (which ApplyStageLabels overwrites with the
// pre-comment fetch). Otherwise the next cycle reads N+1 > N as a person
// acting and stage/stalled slips by a dwell after every hive comment.
func TestApplyStageLabelsOwnSweepCommentDoesNotRestartQuietClock(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, tracker, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	key := stageKey("acme", "widget", 42)
	// An incomplete swarm aggregate keeps the head at 050 without the PR
	// waiting on the hive (requires_human would move it to 085, which has
	// no dwell).
	human := ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}
	cycle := func(labels []string, comments int, ownComment bool) {
		pr := stagePR(42, labels...)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageRole("acme", "widget", 42, stagelabels.RoleTrusted)
		pr.Comments = gh.Ptr(comments)
		e.noteStagePR("acme", "widget", 42, pr)
		e.noteStageVerdict("acme", "widget", 42, human)
		if ownComment {
			e.noteOwnStageComment("acme", "widget", 42)
		}
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	at050 := []string{"stage/050-review-pending", "role/trusted", "author/human"}
	cycle(nil, 3, false)
	quiet0, _ := store.Get(key)

	// Cycle 2: the fetch saw 3 comments, then the sweep posted one.
	clock.advance(10 * time.Minute)
	cycle(at050, 3, true)
	rec, _ := store.Get(key)
	if rec.ActivityComments != 4 {
		t.Fatalf("stored count = %d, want the fetched 3 plus the hive's own comment", rec.ActivityComments)
	}
	if !rec.LastActivityAt.Equal(quiet0.LastActivityAt) {
		t.Fatalf("hive's own comment moved the quiet clock: %v → %v", quiet0.LastActivityAt, rec.LastActivityAt)
	}
	// Cycle 3: the fetch now includes the hive's comment: not activity.
	clock.advance(10 * time.Minute)
	cycle(at050, 4, false)
	rec, _ = store.Get(key)
	if !rec.LastActivityAt.Equal(quiet0.LastActivityAt) {
		t.Fatalf("hive's own comment read as outside activity on the next fetch: %v → %v", quiet0.LastActivityAt, rec.LastActivityAt)
	}
	// Quiet for the dwell from the original activity: stalled on time.
	api.reset()
	clock.advance(41 * time.Minute)
	cycle(at050, 4, false)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled must not slip by a dwell after the hive's own comment: added=%v", api.added)
	}
	// Without a fetch this cycle the credit has nowhere to go but the store.
	e.noteOwnStageComment("acme", "widget", 42)
	rec, _ = store.Get(key)
	if rec.ActivityComments != 5 {
		t.Fatalf("store credit = %d, want 5", rec.ActivityComments)
	}
	if tracker.creditOwnComment(key) {
		t.Fatal("no observation this cycle: tracker must decline the credit")
	}
	// A person's comment on top of it is still activity.
	api.reset()
	clock.advance(time.Minute)
	cycle([]string{"stage/050-review-pending", "stage/stalled", "role/trusted"}, 6, false)
	if !reflect.DeepEqual(api.removed, []string{"stage/stalled"}) {
		t.Fatalf("outside comment must clear stalled: removed=%v", api.removed)
	}
}

// A maintainer's body-only Approve / Request-changes adds nothing to the
// comment counts (review_comments is inline only), so the review walk's
// count is what clears stage/stalled; the hive app's own reviews do not.
func TestApplyStageLabelsBodyOnlyReviewIsActivity(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()
	key := stageKey("acme", "widget", 43)
	human := ContributorLaneVerdict{Configured: true, Verdict: "incomplete"}
	review := func(login, state string) *gh.PullRequestReview {
		return &gh.PullRequestReview{User: &gh.User{Login: gh.Ptr(login)}, State: gh.Ptr(state), Body: gh.Ptr("looks fine")}
	}
	cycle := func(labels []string, reviews ...*gh.PullRequestReview) {
		pr := stagePR(43, labels...)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageRole("acme", "widget", 43, stagelabels.RoleTrusted)
		pr.Comments = gh.Ptr(0)
		e.noteStagePR("acme", "widget", 43, pr)
		e.noteStageVerdict("acme", "widget", 43, human)
		e.noteStageReviews("acme", "widget", 43, reviews)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hiveReview := review(testHiveAppBotLogin, "APPROVED")
	cycle(nil, hiveReview)
	rec, _ := store.Get(key)
	if rec.ActivityReviews != 0 {
		t.Fatalf("hive app review counted as a person's: %d", rec.ActivityReviews)
	}
	at050 := []string{"stage/050-review-pending", "role/trusted", "author/human"}
	clock.advance(time.Hour)
	api.reset()
	cycle(at050, hiveReview)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("quiet dwell must stall: added=%v", api.added)
	}
	stalledAt := clock.at
	// A body-only approve from a maintainer: activity, stalled clears.
	api.reset()
	clock.advance(time.Minute)
	cycle([]string{"stage/050-review-pending", "stage/stalled", "role/trusted"}, hiveReview, review("maintainer", "APPROVED"))
	if !reflect.DeepEqual(api.removed, []string{"stage/stalled"}) {
		t.Fatalf("body-only review must clear stalled: removed=%v added=%v", api.removed, api.added)
	}
	rec, _ = store.Get(key)
	if rec.ActivityReviews != 1 || !rec.LastActivityAt.After(stalledAt) {
		t.Fatalf("review not recorded as activity: %+v", rec)
	}
	// The same review seen again is not new activity.
	clock.advance(time.Minute)
	cycle(at050, hiveReview, review("maintainer", "APPROVED"))
	rec2, _ := store.Get(key)
	if !rec2.LastActivityAt.Equal(rec.LastActivityAt) {
		t.Fatal("re-observing the same review moved the quiet clock")
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
		e.noteStageRole("acme", "widget", 11, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observe(nil, "review-state-check", errors.New("boom"))
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", "role/trusted", "stage/080-awaiting-merge-admission"}}) {
		t.Fatalf("added=%v", api.added)
	}
	api.reset()
	clock.advance(30 * time.Minute)
	observe([]string{"stage/080-awaiting-merge-admission", "role/trusted", "author/human"}, "review-state-check", errors.New("boom"))
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled added=%v", api.added)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 080-awaiting-merge-admission: no activity for 30m0s: review-state-check") {
		t.Fatalf("stall note=%v", api.comments)
	}
	api.reset()
	clock.advance(time.Hour)
	observe([]string{"stage/080-awaiting-merge-admission", "stage/stalled", "role/trusted", "author/human"}, "review-state-check", errors.New("boom"))
	if api.writes() != 0 || len(api.comments) != 0 {
		t.Fatalf("stall note must be posted once: added=%v comments=%v", api.added, api.comments)
	}
	// Lane admits the PR: forward to 095 pre-merge-soak, stalled cleared, no note.
	api.reset()
	observe([]string{"stage/080-awaiting-merge-admission", "stage/stalled", "role/trusted", "author/human"}, contributorLaneReasonSoaking+"(remaining=10m0s)", nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/095-pre-merge-soak"}}) || len(api.comments) != 0 {
		t.Fatalf("admission added=%v comments=%v", api.added, api.comments)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/080-awaiting-merge-admission", "stage/stalled"}) {
		t.Fatalf("admission removed=%v", api.removed)
	}
	// The soak window plus grace passes and the lane still reports soaking:
	// stalled, with the 095 note saying the lane has not promoted it.
	api.reset()
	clock.advance(stagelabels.PreMergeSoakDwell)
	observe([]string{"stage/095-pre-merge-soak", "role/trusted", "author/human"}, contributorLaneReasonSoaking+"(remaining=10m0s)", nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("soak overrun added=%v", api.added)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 095-pre-merge-soak: no activity for 45m0s: contributor-lane:soaking(remaining=10m0s)") || !strings.Contains(api.comments[0], stagelabels.PreMergeSoakStallNote) {
		t.Fatalf("soak overrun note=%v", api.comments)
	}
	// Soak complete, waiting on GitHub: 100 merge-queue, stalled cleared.
	api.reset()
	observe([]string{"stage/095-pre-merge-soak", "stage/stalled", "role/trusted", "author/human"}, "required-checks-expected", nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/100-merge-queue"}}) || len(api.comments) != 0 {
		t.Fatalf("soak complete added=%v comments=%v", api.added, api.comments)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{"stage/095-pre-merge-soak", "stage/stalled"}) {
		t.Fatalf("soak complete removed=%v", api.removed)
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
	e.noteStageRole("acme", "widget", 12403, stagelabels.RoleTrusted)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	api.reset()
	e.recordStage(stageLaneTrusted, "acme", "acme", "widget", stagePR(12403, "stage/100-merge-queue", "role/trusted", "author/human"), "untrusted-author-role", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(12403, "stage/100-merge-queue", "role/trusted", "author/human"), "pending: head pushed 1m0s ago (< min_head_age)", false, nil)
	e.noteStageRole("acme", "widget", 12403, stagelabels.RoleTrusted)
	e.noteStageVerdict("acme", "widget", 12403, reject)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	// A human author: the author-blocked stage is 071 needs-author.
	if !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) {
		t.Fatalf("added=%v", api.added)
	}
	rec, _ := store.Get(stageKey("acme", "widget", 12403))
	if rec.Reason != "reviewer-verdict:changes_requested" {
		t.Fatalf("stored reason %q", rec.Reason)
	}
	if len(api.comments) == 0 || !strings.Contains(api.comments[0], "stage 100-merge-queue → 071-needs-author: reviewer-verdict:changes_requested") {
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
		{name: "soaking with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h39m)", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "merge-failed with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "merge-failed", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "self-authored release-hold with red optional", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "release-hold", Detail: red}, want: stagelabels.Fixing, reason: named},
		// would be 080
		{name: "author side done with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "lane error with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		// would be 050/060
		{name: "pending with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:lint", Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		{name: "no verdict with red optional", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict, Detail: red}, want: stagelabels.ReturnedToContributor, reason: named},
		// red required: the lane's own reason
		{name: "red required", obs: stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build-gate", Detail: red}, want: stagelabels.ReturnedToContributor},
		{name: "red required self-authored", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "required-check-failing:build-gate", Detail: approve}, want: stagelabels.Fixing},
		// green
		{name: "soaking all green", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h39m)", Detail: approve}, want: stagelabels.PreMergeSoak},
		{name: "merge-failed all green", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "merge-failed", Detail: approve}, want: stagelabels.MergeQueue},
		{name: "author side done all green", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Detail: approve}, want: stagelabels.AwaitingMergeAdmission},
		{name: "note for an older head does not bind", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha2", Reason: contributorLaneReasonSoaking + "(1h)", Detail: red}, want: stagelabels.PreMergeSoak},
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
// check moves 095 → 071 (regressed, check named in the comment and store
// reason); a PR awaiting admission moves 080 → 071 the same way; once the
// check is green the same soaking reason yields 095; and a head push drops
// the old head's note. A pre-095 record still wearing 100 while soaking is
// reclassified to 095 without a regression.
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
		e.noteStageRole("acme", "widget", 12457, stagelabels.RoleTrusted)
		e.noteStageVerdict("acme", "widget", 12457, approve)
		e.noteStageRedChecks("acme", "widget", 12457, "sha", red)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Migration: a PR at 100 from before 095 existed, still soaking, is
	// reclassified to 095 — a downward label move that is NOT a regression.
	store.Observe(key, stagelabels.MergeQueue, soaking)
	cycle(stagePR(12457, "stage/100-merge-queue", "role/trusted", "author/human"), soaking, nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/095-pre-merge-soak"}}) || !reflect.DeepEqual(api.removed, []string{"stage/100-merge-queue"}) {
		t.Fatalf("100→095 reclassify: added=%v removed=%v", api.added, api.removed)
	}
	if rec, _ := store.Get(key); rec.Regressed() || len(api.comments) != 0 {
		t.Fatalf("100→095 must not be a regression: rec=%+v comments=%v", rec, api.comments)
	}

	// Earlier cycle: all green, soaking → 095 (already on the PR).
	cycle(stagePR(12457, "stage/095-pre-merge-soak", "role/trusted", "author/human"), soaking, nil)
	if len(api.added) != 0 || len(api.removed) != 0 {
		t.Fatalf("green soak changed labels: added=%v removed=%v", api.added, api.removed)
	}

	// An optional check on the same head went red: 095 → 071 (human author), named.
	cycle(stagePR(12457, "stage/095-pre-merge-soak", "role/trusted", "author/human"), soaking, []string{redName})
	// Retired label: the backward hop is recorded as history + comment, never as stage/regressed.
	if !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) || !reflect.DeepEqual(api.removed, []string{"stage/095-pre-merge-soak"}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	rec, _ := store.Get(key)
	if n := len(rec.History); n == 0 || !rec.History[n-1].Regressed {
		t.Fatalf("expected a regressed history hop, got %+v", rec.History)
	}
	if rec.Reason != "optional-check-failing:"+redName {
		t.Fatalf("stored reason %q", rec.Reason)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "stage 095-pre-merge-soak → 071-needs-author: optional-check-failing:"+redName) {
		t.Fatalf("comments=%v", api.comments)
	}

	// Same head, check re-run green: the lane's soaking reason is 095 again.
	cycle(stagePR(12457, "stage/071-needs-author", "role/trusted", "author/human"), soaking, nil)
	if !reflect.DeepEqual(api.added, [][]string{{"stage/095-pre-merge-soak"}}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}

	// Red again while the lane stops on a reason no stage claims (would be
	// 080): still 072.
	cycle(stagePR(12457, "stage/095-pre-merge-soak", "role/trusted", "author/human"), "review-state-check", []string{redName})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	cycle(stagePR(12457, "stage/080-awaiting-merge-admission", "role/trusted", "author/human"), "review-state-check", []string{redName})
	if !reflect.DeepEqual(api.added, [][]string{{"stage/071-needs-author"}}) || !reflect.DeepEqual(api.removed, []string{"stage/080-awaiting-merge-admission"}) {
		t.Fatalf("080 with red CI: added=%v removed=%v", api.added, api.removed)
	}

	// New head pushed: a note recorded for the old head does not bind it.
	cycle(stagePR(12457, "stage/071-needs-author", "role/trusted", "author/human"), soaking, nil)
	pr := stagePR(12457, "stage/095-pre-merge-soak", "role/trusted", "author/human")
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
	if !ok || rec.Stage != stagelabels.NeedsAuthor || rec.Author != "alice" {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	if got := store.ByAuthor("alice", stagelabels.NeedsAuthor); len(got) != 1 {
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
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(21, "role/contributor", "author/human", "stage/050-review-pending"), contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
	setStagePRTruth(21, "role/contributor", "author/human", "stage/050-review-pending", "stage/020-claimed")
	// Snapshot says nothing changes for 050/contributor, so without a
	// reason to write there is no read: the duplicate waits for a write.
	res, err := e.ApplyStageLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gets := api.getCount(); gets != 0 || res.Healed != 0 {
		t.Fatalf("no planned write must mean no read; gets=%d res=%+v", gets, res)
	}

	// Now the lane moves it to 071 (human author): the write re-reads and strips 020 too.
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(21, "role/contributor", "author/human", "stage/050-review-pending"), "changes-requested", false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
	setStagePRTruth(21, "role/contributor", "author/human", "stage/050-review-pending", "stage/020-claimed")
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
	if len(api.added) != 1 || strings.Join(api.added[0], ",") != "stage/071-needs-author" {
		t.Fatalf("added %v, want only 071", api.added)
	}
}

// hivecommons/hive#12545: agent/<lane> (routing) and hive/<id> (provenance)
// mean "the hive owns / authored this PR". A review-fix kick's `gh pr edit
// --body` on a contributor's PR stamped both onto it; the reconciler strips
// them from any PR the App did not author, and leaves self-authored PRs alone.
func TestStrayOwnershipLabels(t *testing.T) {
	labels := []string{"agent/scanner", "hive/oke-11", "stage/050-review-pending", "role/trusted", "hive/other-hive"}
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

	pr := stagePR(12495, "role/trusted", "author/human", "stage/050-review-pending", "agent/scanner", "hive/oke-11")
	pr.User = &gh.User{Login: gh.Ptr("mendezr")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 12495, stagelabels.RoleTrusted)
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

// hivecommons/hive#12542: the stage pass reads the head's commits itself.
// An unsigned head goes to 072 on its FIRST evaluation — the lane only said
// "no verdict yet" — with `no-dco`, the legacy Prow pill stripped, and a
// nudge carrying the exact rebase command. A signed head gets the green
// `dco`. One commits GET per head, none while the head is unchanged.
// clankerBody is the hive provenance marker that classifies a PR as
// author/clanker, the only kind the 072 nudge interrupts.
const clankerBody = "Adds x.\n\n— hive: backend=pi model=gpt-6\n"

func TestApplyStageLabelsDCOFromCommits(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, commits: map[int][]string{
		31: {"feat: a\n\nSigned-off-by: A <a@x>", "fix: b", "fix: c"},
		32: {"feat: a\n\nSigned-off-by: A <a@x>", "merge\n"},
	}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, ContributorNudge: true} }
	ctx := context.Background()

	pr := stagePR(31, "dco-signoff: yes", "bug")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	pr.Body = gh.Ptr(clankerBody)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 31, stagelabels.RoleContributor)
	e.noteStageDCOGate("acme", "widget", 31, "sha", nil, map[string]bool{"DCO": true, "build": true})
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 31))
	if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonDCOMissing {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	if rec.DCO("sha") != stagelabels.DCONo || rec.DCOTotal != 3 || rec.DCOUnsigned != 2 {
		t.Fatalf("dco cache %+v", rec)
	}
	if len(api.added) != 1 || !reflect.DeepEqual(api.added[0], []string{"author/clanker", stagelabels.NoDCOLabel, "role/contributor", "stage/072-returned-to-contributor"}) {
		t.Fatalf("added %v", api.added)
	}
	// Prow's `dco-signoff: yes` is foreign and stays (hivecommons/hive#13125).
	if len(api.removed) != 0 {
		t.Fatalf("removed %v", api.removed)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "@alice") || !strings.Contains(api.comments[0], "2 commits are missing the `Signed-off-by` trailer (DCO) — run `git rebase --signoff HEAD~3 && git push --force-with-lease`") {
		t.Fatalf("nudge %q", api.comments)
	}
	if api.commitReads != 1 {
		t.Fatalf("commit reads = %d", api.commitReads)
	}
	if api.labels[stagelabels.DCOLabel] != stagelabels.RoleTrustedColor || api.labels[stagelabels.NoDCOLabel] != stagelabels.StalledColor {
		t.Fatalf("provisioned colours %v", api.labels)
	}

	// Same head next cycle: cached, no second read, no writes.
	api.reset()
	pr = stagePR(31, "bug", "author/clanker", stagelabels.NoDCOLabel, "role/contributor", "stage/072-returned-to-contributor")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	pr.Body = gh.Ptr(clankerBody)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 31, stagelabels.RoleContributor)
	e.noteStageDCOGate("acme", "widget", 31, "sha", nil, map[string]bool{"DCO": true})
	if _, err := e.ApplyStageLabels(ctx); err != nil || api.writes() != 0 || api.commitReads != 1 {
		t.Fatalf("cached pass: err=%v writes=%d reads=%d", err, api.writes(), api.commitReads)
	}

	// Author signs and force-pushes: new head → re-read, green `dco`, back to 050.
	api.reset()
	api.mu.Lock()
	api.commits[31] = []string{"feat: a\n\nSigned-off-by: A <a@x>", "fix: b\n\nSigned-off-by: A <a@x>", "fix: c\n\nsigned-off-by: A <a@x>"}
	api.mu.Unlock()
	pr = stagePR(31, "bug", "author/clanker", stagelabels.NoDCOLabel, "role/contributor", "stage/072-returned-to-contributor")
	pr.Head.SHA = gh.Ptr("sha2")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	pr.Body = gh.Ptr(clankerBody)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 31, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if api.commitReads != 2 || !reflect.DeepEqual(api.added, [][]string{{stagelabels.DCOLabel, "stage/050-review-pending"}}) {
		t.Fatalf("signed head: reads=%d added=%v removed=%v", api.commitReads, api.added, api.removed)
	}
	sort.Strings(api.removed)
	if !reflect.DeepEqual(api.removed, []string{stagelabels.NoDCOLabel, "stage/072-returned-to-contributor"}) {
		t.Fatalf("signed head removed %v", api.removed)
	}

	// Self-authored lane: an unsigned head is the hive's own rework (070),
	// no contributor nudge.
	api.reset()
	api.mu.Lock()
	api.commits[33] = []string{"fix: unsigned"}
	api.mu.Unlock()
	pr = stagePR(33)
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "ci-pending", false, nil)
	e.noteStageDCOGate("acme", "widget", 33, "sha", map[string]bool{"dco": true}, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/hive", stagelabels.NoDCOLabel, "stage/070-fixing"}}) || len(api.comments) != 0 {
		t.Fatalf("self-authored: added=%v comments=%v", api.added, api.comments)
	}
}

func TestApplyStageLabelsHiveBotSignoffOnClankerHeadReturnsToContributor(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, commitDetails: map[int][]stageCommitFixture{
		80: {{
			message:     "docs: follow-up\n\nSigned-off-by: hivecommons-hive[bot] <280983584+hivecommons-hive[bot]@users.noreply.github.com>",
			authorLogin: testHiveAppBotLogin,
			authorName:  "hivecommons-hive[bot]",
			authorEmail: "280983584+hivecommons-hive[bot]@users.noreply.github.com",
		}},
	}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, ContributorNudge: true} }

	pr := stagePR(80, "role/trusted", "author/clanker", "stage/072-returned-to-contributor")
	pr.User = &gh.User{Login: gh.Ptr("arteeh")}
	pr.Body = gh.Ptr(clankerBody)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 80, stagelabels.RoleTrusted)
	e.noteStageDCOGate("acme", "widget", 80, "sha", map[string]bool{"Sign-off survives the squash": true}, map[string]bool{"Sign-off survives the squash": true})

	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The branch owner is the only one who can rewrite the commit: the
	// clanker keeps 072 (never 070, where no lane acts on a non-App
	// branch) and its nudge names the exact command.
	rec, ok := store.Get(stageKey("acme", "widget", 80))
	if !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	if rec.DCOSquashUnsafeHive != 1 || rec.DCO("sha") != stagelabels.DCOYes {
		t.Fatalf("dco cache %+v", rec)
	}
	if !reflect.DeepEqual(api.added, [][]string{{stagelabels.DCOLabel}}) || len(api.removed) != 0 {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], ContributorNudgeMarkerPrefix) || !strings.Contains(api.comments[0], "git commit --amend --reset-author -s") {
		t.Fatalf("comments %q", api.comments)
	}
}

func TestApplyStageLabelsForwardMergeSkipsSignoffAttribution(t *testing.T) {
	// hivecommons/hive#13635: a sync top-up carries other people's commits
	// by design and lands as a merge commit, so no attribution nudge and no
	// 071 for it. The skip keys on the head ref only; a title anyone can
	// type ("sync: …", "top-up: …") does not relax the gate, and the skip
	// leaves no DCO cache entry for the head.
	cases := []struct {
		name, ref, title string
		skip             bool
	}{
		{"sync head", "sync/v5-to-v6-2026-10-22", "chore: carry v5", true},
		{"topup head", "topup/v5-into-v6-20261022", "🌱 top-up: merge v5 into v6 (68 commits)", true},
		{"top-up title only", "someone/branch", "🌱 top-up: merge v5 into v6 (68 commits)", false},
		{"sync title only", "someone/branch", "sync: carry v5", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := 330 + i
			api := &stageAPI{labels: map[string]string{}, commitDetails: map[int][]stageCommitFixture{
				n: {{
					message:     "Add OWNERS file\n\nSigned-off-by: Andy Anderson <280983584+hivecommons-hive[bot]@users.noreply.github.com>",
					authorLogin: testHiveAppBotLogin,
					authorName:  "Andy Anderson",
					authorEmail: "280983584+hivecommons-hive[bot]@users.noreply.github.com",
				}, {
					message:     "fix: unsigned",
					authorLogin: "clubanderson",
					authorName:  "Andy",
					authorEmail: "andy@example.com",
				}},
			}}
			srv := newStageAPI(t, api)
			defer srv.Close()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)}
			e, _, store := newStageEngine(t, srv.URL, clock)
			e.stageLabelPolicy = func() StageLabelPolicy {
				return StageLabelPolicy{Enabled: true, ContributorNudge: true, DCORequired: func(string) (bool, bool) { return true, true }}
			}

			pr := stagePR(n, "role/trusted")
			pr.Head.Ref = gh.Ptr(tc.ref)
			pr.Head.SHA = gh.Ptr("sha")
			pr.Title = gh.Ptr(tc.title)
			pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
			e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting, false, nil)
			e.noteStageRole("acme", "widget", n, stagelabels.RoleTrusted)

			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec, ok := store.Get(stageKey("acme", "widget", n))
			if !ok {
				t.Fatalf("no record")
			}
			if tc.skip {
				if rec.Reason == stagelabels.LaneReasonHiveSignoffAttribution || rec.Reason == stagelabels.LaneReasonDCOMissing {
					t.Fatalf("dco reason on a forward-merge head: %+v", rec)
				}
				if rec.DCO("sha") != stagelabels.DCOUnknown || rec.DCOTotal != 0 || rec.DCOSquashUnsafeKnown {
					t.Fatalf("skip cached a dco record: %+v", rec)
				}
				for _, c := range api.comments {
					if strings.Contains(c, SignoffAttributionMarkerPrefix) {
						t.Fatalf("attribution nudge posted: %q", c)
					}
				}
				return
			}
			if rec.Reason != stagelabels.LaneReasonDCOMissing || rec.DCOTotal != 2 || rec.DCOUnsigned != 1 {
				t.Fatalf("title-only match relaxed the commit check: %+v", rec)
			}
		})
	}
}

func TestApplyStageLabelsHiveBotSignoffOnHumanHeadNeedsAuthorWithInstructions(t *testing.T) {
	// hivecommons/pluk#315 (2026-10-20): a human's OWNERS commit, made in a
	// hive pod, carried the App's noreply address and sign-off. GitHub
	// linked the commit to the App, so the squash gate would misattribute
	// it; the stage pass sent it to 070-fixing, where no agent could act.
	api := &stageAPI{labels: map[string]string{}, commitDetails: map[int][]stageCommitFixture{
		315: {{
			message:     "Add OWNERS file\n\nSigned-off-by: Andy Anderson <280983584+hivecommons-hive[bot]@users.noreply.github.com>",
			authorLogin: testHiveAppBotLogin,
			authorName:  "Andy Anderson",
			authorEmail: "280983584+hivecommons-hive[bot]@users.noreply.github.com",
		}},
	}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, ContributorNudge: true} }

	pr := stagePR(315, "role/trusted")
	pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting, false, nil)
	e.noteStageRole("acme", "widget", 315, stagelabels.RoleTrusted)

	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 315))
	if !ok || rec.Stage != stagelabels.NeedsAuthor || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("record %+v ok=%v", rec, ok)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", stagelabels.DCOLabel, "stage/071-needs-author"}}) {
		t.Fatalf("added %v", api.added)
	}
	if len(api.comments) != 1 {
		t.Fatalf("comments %q", api.comments)
	}
	for _, want := range []string{SignoffAttributionMarkerPrefix + " head=sha -->", "@clubanderson", "`stage/071-needs-author`", "git commit --amend --reset-author -s && git push --force-with-lease", "git config user.email"} {
		if !strings.Contains(api.comments[0], want) {
			t.Fatalf("comment lacks %q:\n%s", want, api.comments[0])
		}
	}
	if rec.NudgeHead != "sha" {
		t.Fatalf("nudge head not recorded: %+v", rec)
	}

	// Same head, next sweep: the labels stand and the author is not told twice.
	clock.at = clock.at.Add(10 * time.Minute)
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(315, "role/trusted", "author/human", "dco", "stage/071-needs-author"), autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting, false, nil)
	e.noteStageRole("acme", "widget", 315, stagelabels.RoleTrusted)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.comments) != 1 {
		t.Fatalf("second pass re-commented: %q", api.comments)
	}
}

func TestSignoffAttributionAsk(t *testing.T) {
	one := signoffAttributionAsk(stageDetail{DCOTotal: 1, DCOSquashUnsafeHive: 1})
	if !strings.Contains(one, "1 commit is signed off as the hive App") || !strings.Contains(one, "`git commit --amend --reset-author -s && git push --force-with-lease`") {
		t.Fatalf("one: %s", one)
	}
	many := signoffAttributionAsk(stageDetail{DCOTotal: 3, DCOSquashUnsafeHive: 2})
	if !strings.Contains(many, "2 commits are signed off as the hive App") || !strings.Contains(many, "git rebase --exec 'git commit --amend --no-edit --reset-author -s' HEAD~3 && git push --force-with-lease") {
		t.Fatalf("many: %s", many)
	}
	if zero := signoffAttributionAsk(stageDetail{}); !strings.Contains(zero, "1 commit is") {
		t.Fatalf("zero: %s", zero)
	}
	_, asks, _ := nudgeDiagnosis(stagelabels.LaneReasonHiveSignoffAttribution, stageDetail{DCOTotal: 1, DCOSquashUnsafeHive: 1})
	if len(asks) != 1 || asks[0] != one {
		t.Fatalf("diagnosis asks %q", asks)
	}
}

func TestApplyStageLabelsResolvedSelfAuthoredDecisionDropsNeedsDecision(t *testing.T) {
	api := &stageAPI{}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 21, 30, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()

	const number = 12808
	pr := stagePR(number, "author/hive", "role/trusted", "stage/085-needs-decision")
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "required-check-pending:build", false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
	e.noteStageVerdict("acme", "widget", number, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})

	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/060-reviewed"}}) || !reflect.DeepEqual(api.removed, []string{"stage/085-needs-decision"}) || len(api.comments) != 0 {
		t.Fatalf("resolved self-authored decision should leave 085: added=%v removed=%v comments=%v", api.added, api.removed, api.comments)
	}
}

// A repo without a DCO gate (no `dco`/`DCO` context required or reported)
// still gets the `no-dco` label but keeps the lane's stage and is not asked
// to rebase; `governor.labels.stage.dco.required` overrides detection both
// ways (hivecommons/hive#12544 review: "the 072 override hits every repo").
func TestApplyStageLabelsDCOGatePerRepo(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, commits: map[int][]string{
		41: {"fix: unsigned"},
		42: {"fix: unsigned"},
		43: {"fix: unsigned"},
	}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	required := map[string]bool{}
	e.stageLabelPolicy = func() StageLabelPolicy {
		return StageLabelPolicy{Enabled: true, ContributorNudge: true, DCORequired: func(display string) (bool, bool) {
			v, ok := required[display]
			return v, ok
		}}
	}
	ctx := context.Background()

	// Not detected, not configured: label only, stage from the lane.
	pr := stagePR(41)
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 41, stagelabels.RoleContributor)
	e.noteStageDCOGate("acme", "widget", 41, "sha", map[string]bool{"build": true}, map[string]bool{"build": true})
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(stageKey("acme", "widget", 41))
	if rec.Stage != stagelabels.ReviewPending || rec.Reason == stagelabels.LaneReasonDCOMissing {
		t.Fatalf("ungated repo routed: %+v", rec)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/human", stagelabels.NoDCOLabel, "role/contributor", "stage/050-review-pending"}}) || len(api.comments) != 0 {
		t.Fatalf("ungated: added=%v comments=%v", api.added, api.comments)
	}
	// The informational `no-dco` head is cached too: one commits read per
	// head, not one per cycle (hivecommons/hive#12544 review).
	if t0, u0, cached := rec.DCOFor("sha"); !cached || t0 != 1 || u0 != 1 || api.commitReads != 1 {
		t.Fatalf("ungated cache: cached=%v total=%d unsigned=%d reads=%d", cached, t0, u0, api.commitReads)
	}
	api.reset()
	pr = stagePR(41, "author/human", stagelabels.NoDCOLabel, "role/contributor", "stage/050-review-pending")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 41, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil || api.writes() != 0 || api.commitReads != 1 {
		t.Fatalf("ungated cached pass: err=%v writes=%d reads=%d", err, api.writes(), api.commitReads)
	}

	// Configured required=true beats "not detected".
	api.reset()
	required["acme/widget"] = true
	pr = stagePR(42)
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	pr.Body = gh.Ptr(clankerBody)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 42, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.Get(stageKey("acme", "widget", 42))
	if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != stagelabels.LaneReasonDCOMissing || len(api.comments) != 1 {
		t.Fatalf("configured required: %+v comments=%d", rec, len(api.comments))
	}

	// Configured required=false beats a detected gate.
	api.reset()
	required["acme/widget"] = false
	pr = stagePR(43)
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 43, stagelabels.RoleContributor)
	e.noteStageDCOGate("acme", "widget", 43, "sha", nil, map[string]bool{"DCO": true})
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.Get(stageKey("acme", "widget", 43))
	if rec.Stage != stagelabels.ReviewPending || len(api.comments) != 0 {
		t.Fatalf("configured not required: %+v comments=%d", rec, len(api.comments))
	}
	if api.labels[stagelabels.NoDCOLabel] != stagelabels.StalledColor {
		t.Fatalf("no-dco still provisioned: %v", api.labels)
	}
}

func TestDCOCommitStatsSkipsMerges(t *testing.T) {
	commits := []*gh.RepositoryCommit{
		{Commit: &gh.Commit{Message: gh.Ptr("a\n\nSigned-off-by: A <a@x>")}, Parents: []*gh.Commit{{}}},
		{Commit: &gh.Commit{Message: gh.Ptr("Merge branch 'v5'")}, Parents: []*gh.Commit{{}, {}}},
		{Commit: &gh.Commit{Message: gh.Ptr("b")}, Parents: []*gh.Commit{{}}},
	}
	if total, unsigned := dcoCommitStats(commits); total != 2 || unsigned != 1 {
		t.Fatalf("total=%d unsigned=%d", total, unsigned)
	}
	if dcoState(0, 0) != stagelabels.DCOUnknown || dcoState(2, 0) != stagelabels.DCOYes || dcoState(2, 1) != stagelabels.DCONo {
		t.Fatal("dcoState")
	}
	if got := dcoAsk(stageDetail{DCOTotal: 1, DCOUnsigned: 1}); !strings.Contains(got, "1 commit is missing") || !strings.Contains(got, "HEAD~1") {
		t.Fatalf("ask %q", got)
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
		e.noteStageRole("acme", "widget", 12423, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	queued := []string{"stage/100-merge-queue", "role/trusted", "author/human"}
	held := []string{"stage/050-review-pending", "role/trusted", "author/human"}

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

// hivecommons/hive#12581: the trust axis is role/<tier>; author/<kind> says
// who wrote the PR. A relay-trailered PR under a trusted login carries both
// role/trusted and author/clanker; the App's own PR is author/hive; a
// hand-pushed PR is author/human. The pre-rename author/<tier> spelling is
// migrated in the same write, and a mis-stamped agent/<lane> on a human PR
// (a stray, #12545) never reads as author/hive.
func TestApplyStageLabelsAuthorKind(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy {
		return StageLabelPolicy{Enabled: true, HiveID: "oke-11", RelayKnown: func(login string) bool { return login == "registered" }}
	}
	ctx := context.Background()
	run := func(number int, login, body string, labels ...string) {
		t.Helper()
		pr := stagePR(number, labels...)
		pr.User = &gh.User{Login: gh.Ptr(login)}
		pr.Body = gh.Ptr(body)
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
		e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	added := func() []string {
		var out []string
		for _, a := range api.added {
			out = append(out, a...)
		}
		sort.Strings(out)
		return out
	}
	removed := func() []string { out := append([]string(nil), api.removed...); sort.Strings(out); return out }

	run(31, "mendezr", "Adds x.\n\n— hive: backend=pi model=gpt-6\n", "author/trusted")
	if !reflect.DeepEqual(added(), []string{"author/clanker", "role/trusted", "stage/050-review-pending"}) || !reflect.DeepEqual(removed(), []string{"author/trusted"}) {
		t.Fatalf("clanker: added=%v removed=%v", api.added, api.removed)
	}
	run(32, testHiveAppBotLogin, "Adds y.\n\n— hive: agent=scanner backend=copilot\n")
	if !reflect.DeepEqual(added(), []string{"author/hive", "role/trusted", "stage/050-review-pending"}) {
		t.Fatalf("hive: added=%v", api.added)
	}
	run(33, "clubanderson", "Adds z by hand.\n", "agent/scanner", "hive/oke-11")
	if !reflect.DeepEqual(added(), []string{"author/human", "role/trusted", "stage/050-review-pending"}) || !reflect.DeepEqual(removed(), []string{"agent/scanner", "hive/oke-11"}) {
		t.Fatalf("human with stray stamps: added=%v removed=%v", api.added, api.removed)
	}
	run(34, "registered", "No marker at all.\n")
	if !reflect.DeepEqual(added(), []string{"author/clanker", "role/trusted", "stage/050-review-pending"}) {
		t.Fatalf("registry fallback: added=%v", api.added)
	}
	run(35, "mendezr", "<!-- hive:origin hive=oke-11 -->\n")
	if !reflect.DeepEqual(added(), []string{"author/hive", "role/trusted", "stage/050-review-pending"}) {
		t.Fatalf("local origin marker: added=%v", api.added)
	}
	// Steady state under the labels just written: nothing to do.
	run(31, "mendezr", "Adds x.\n\n— hive: backend=pi model=gpt-6\n", "author/clanker", "role/trusted", "stage/050-review-pending")
	if api.writes() != 0 {
		t.Fatalf("steady state wrote added=%v removed=%v", api.added, api.removed)
	}
}

// Configured author/<kind> names are written and provisioned instead of
// the defaults, and a default-named label left from before the rename is
// swapped out.
func TestApplyStageLabelsConfiguredKindNames(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	names := authorkind.Names{Clanker: "author/relay"}
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, AuthorKindLabels: names} }
	pr := stagePR(41, "author/clanker")
	pr.User = &gh.User{Login: gh.Ptr("mendezr")}
	pr.Body = gh.Ptr("— hive: backend=pi\n")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageRole("acme", "widget", 41, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"author/relay", "role/contributor", "stage/050-review-pending"}}) || !reflect.DeepEqual(api.removed, []string{"author/clanker"}) {
		t.Fatalf("added=%v removed=%v", api.added, api.removed)
	}
	if _, ok := api.labels["author/relay"]; !ok {
		t.Fatalf("configured name not provisioned: %v", api.labels)
	}
	if _, ok := api.labels["author/clanker"]; ok {
		t.Fatal("default clanker name provisioned despite override")
	}
}

// A transport without the process's StageWriter cannot stage anything: the
// pass fails closed and drains the tracker rather than writing from a
// second writer (ADR-0025 admission).
func TestApplyStageLabelsNeedsStageWriterTransport(t *testing.T) {
	tracker := NewStageTracker()
	e := New(stubTransport{}, Options{
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
		StageTracker:     tracker,
		StageStore:       stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), nil),
	})
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(1), "pending", false, nil)
	_, err := e.ApplyStageLabels(context.Background())
	if !errors.Is(err, ErrNoStageWriter) {
		t.Fatalf("err = %v, want ErrNoStageWriter", err)
	}
	if got := tracker.drain(); len(got) != 0 {
		t.Fatalf("tracker not drained: %d observations left", len(got))
	}
}

// Options.StageEngine reaches the reconciler: an enforce-mode engine with
// its own PR deriver decides the stage instead of the sweep's derivation.
func TestApplyStageLabelsHonoursStageEngineOptions(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	tracker := NewStageTracker()
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	engine := stageengine.DeriverFunc(stagelabels.PRItem, func(stageengine.Facts) stageengine.Decision {
		return stageengine.Decision{Stage: stagelabels.Reviewed, Role: stagelabels.RoleTrusted, Reason: "engine says reviewed"}
	})
	e := New(client, Options{
		Now:              clock.now,
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
		StageTracker:     tracker,
		StageStore:       store,
		StageEngine:      &stageengine.Options{Mode: stageengine.ModeEnforce, PR: engine},
		CheckSummary:     greenChecks,
	})
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(1, "stage/100-merge-queue", "role/trusted"), "head-changed-since-eval", false, nil)
	e.noteStageRole("acme", "widget", 1, stagelabels.RoleTrusted)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	var added []string
	for _, a := range api.added {
		added = append(added, a...)
	}
	if !slices.Contains(added, stagelabels.Reviewed.Label()) || !slices.Contains(api.removed, "stage/100-merge-queue") {
		t.Fatalf("engine decision not written: added=%v removed=%v", api.added, api.removed)
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 1)); !ok || rec.Stage != stagelabels.Reviewed || rec.Reason != "engine says reviewed" {
		t.Fatalf("store %+v", rec)
	}
}

// hivecommons/hive#15499: a decision question that names the PR already
// carrying the change is the superseded question itself and warrants the
// closer; any other decision question stays a person's.
func TestSupersededCheckWarrantedForRedundantOfDecision(t *testing.T) {
	human := stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Reason: stageReasonVerdictRequiresHuman, Human: true}
	redundant := stageObservation{Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "close as a no-op?", RedundantOf: 15427}}}
	other := stageObservation{Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "rename the flag?"}}}
	if !supersededCheckWarranted(human, redundant) {
		t.Fatal("redundant-of decision must run the superseded/empty-diff closer")
	}
	if supersededCheckWarranted(human, other) {
		t.Fatal("an ordinary decision question is not a supersession check")
	}
}
