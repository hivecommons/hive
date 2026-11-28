package automerge

// The promotion lane (automerge_promotion_lane.go) merges a branch-promotion
// pull request whose head tree is, read live, the source branch's tree. On
// v6 tip Options.Promotions does not exist and the self-authored sweep skips
// every such pull request as not-app-authored, so a person merges it.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// promotionSweepFixture is one promote-main-to-stable pull request on
// acme/widget and the source branch main as GitHub reports it.
type promotionSweepFixture struct {
	mu     sync.Mutex
	number int
	author string
	ref    string
	head   string
	labels []string
	files  []map[string]any
	// tips are the successive tips GET /branches/main reports; the last one
	// repeats. trees maps a tip to its tree.
	tips  []string
	trees map[string]string
	// compares maps "<tip>...<head>" to the compare payload.
	compares   map[string]map[string]any
	branchHits int
	merged     []int
	// mergeableState is the PR's mergeable_state ("clean" when empty);
	// mergeableUnknown reports mergeable as null. mergeTree is the tree of
	// GitHub's test merge commit ("m<number>", parents basesha and the
	// head); empty means the source tip's tree at the first read.
	mergeableState   string
	mergeableUnknown bool
	mergeTree        string
	updates          int
	// headTree and headParents are GET git/commits/<head> (defaults: the
	// tree trees names for the head, else the first tip's, and one parent
	// "older"). mergedTree is the tree of the commit the merge lands
	// ("merge<number>"; default the test merge tree).
	headTree    string
	headParents []string
	mergedTree  string
	// labelPosts and commentPosts count the label and comment writes on the
	// pull request (the human-merge-path gate makes one of each).
	labelPosts   int
	commentPosts int
	// checkState is the state of the required ci/build status on the head
	// ("success" when empty).
	checkState string
	// onPRRead, when set, runs each time the pull request itself is read
	// (GET /pulls/N).
	onPRRead func()
}

func (fx *promotionSweepFixture) tip() string {
	i := fx.branchHits
	if i >= len(fx.tips) {
		i = len(fx.tips) - 1
	}
	fx.branchHits++
	return fx.tips[i]
}

func newPromotionSweepAPI(t *testing.T, fx *promotionSweepFixture) *httptest.Server {
	t.Helper()
	if fx.author == "" {
		fx.author = "github-actions[bot]"
	}
	if fx.ref == "" {
		fx.ref = "auto/promote-main-to-stable"
	}
	prPath := "/repos/acme/widget/pulls/" + strconv.Itoa(fx.number)
	issuePath := "/repos/acme/widget/issues/" + strconv.Itoa(fx.number)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if servePRCommitsSigned(w, r) {
			return
		}
		p := r.URL.Path
		pr := func() map[string]any {
			state := fx.mergeableState
			if state == "" {
				state = "clean"
			}
			var mergeable any = true
			if fx.mergeableUnknown {
				mergeable = nil
			}
			return map[string]any{
				"number": fx.number, "state": "open", "title": "Promote main to stable",
				"mergeable_state": state, "mergeable": mergeable, "changed_files": len(fx.files),
				"merge_commit_sha": "m" + strconv.Itoa(fx.number),
				"user":             map[string]any{"login": fx.author, "type": "Bot"},
				"head":             map[string]any{"sha": fx.head, "ref": fx.ref, "repo": map[string]any{"full_name": "acme/widget"}},
				"base":             map[string]any{"ref": "stable", "sha": "basesha", "repo": map[string]any{"full_name": "acme/widget"}},
				"labels":           issueLabels("", fx.labels),
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{pr()})
		case r.Method == http.MethodGet && p == prPath+"/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && p == prPath+"/files":
			files := fx.files
			if files == nil {
				files = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(files)
		case r.Method == http.MethodGet && p == prPath:
			if fx.onPRRead != nil {
				fx.onPRRead()
			}
			_ = json.NewEncoder(w).Encode(pr())
		case r.Method == http.MethodPut && p == prPath+"/merge":
			fx.merged = append(fx.merged, fx.number)
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge" + strconv.Itoa(fx.number)})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/branches/main":
			tip := fx.tip()
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": true, "commit": map[string]any{"sha": tip, "commit": map[string]any{"tree": map[string]any{"sha": fx.trees[tip]}}}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/branches/main/protection":
			_ = json.NewEncoder(w).Encode(map[string]any{"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0}, "allow_force_pushes": map[string]any{"enabled": false}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/git/commits/"+fx.head:
			tree := fx.headTree
			if tree == "" {
				if t, ok := fx.trees[fx.head]; ok {
					tree = t
				} else {
					tree = fx.trees[fx.tips[0]]
				}
			}
			parents := fx.headParents
			if parents == nil {
				parents = []string{"older"}
			}
			ps := []map[string]any{}
			for _, sha := range parents {
				ps = append(ps, map[string]any{"sha": sha})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": fx.head, "tree": map[string]any{"sha": tree}, "parents": ps})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/git/commits/merge"+strconv.Itoa(fx.number):
			tree := fx.mergedTree
			if tree == "" {
				tree = fx.mergeTree
			}
			if tree == "" {
				tree = fx.trees[fx.tips[0]]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "merge" + strconv.Itoa(fx.number), "tree": map[string]any{"sha": tree}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/git/commits/m"+strconv.Itoa(fx.number):
			tree := fx.mergeTree
			if tree == "" {
				tree = fx.trees[fx.tips[0]]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "m" + strconv.Itoa(fx.number), "tree": map[string]any{"sha": tree}, "parents": []map[string]any{{"sha": "basesha"}, {"sha": fx.head}}})
		case r.Method == http.MethodPut && p == prPath+"/update-branch":
			fx.updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repos/acme/widget/compare/"):
			if body, ok := fx.compares[strings.TrimPrefix(p, "/repos/acme/widget/compare/")]; ok {
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && p == "/repos/acme/widget/branches/stable":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "stable", "commit": map[string]string{"sha": "basesha"}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/branches/stable/protection/required_status_checks":
			_ = json.NewEncoder(w).Encode(map[string]any{"strict": true, "contexts": []string{"ci/build"}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/rules/branches/stable":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/commits/basesha/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []map[string]string{}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/commits/"+fx.head+"/status":
			state := fx.checkState
			if state == "" {
				state = "success"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": state, "total_count": 1, "statuses": []map[string]string{{"context": "ci/build", "state": state}}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/commits/"+fx.head+"/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/commits/"+fx.head:
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": fx.head, "author": map[string]string{"login": fx.author}, "commit": map[string]any{"author": map[string]string{"name": fx.author}}})
		case r.Method == http.MethodGet && p == "/repos/acme/widget/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		case r.Method == http.MethodGet && (p == issuePath+"/comments" || p == issuePath+"/timeline"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && p == issuePath+"/comments":
			fx.commentPosts++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && p == issuePath+"/labels":
			fx.labelPosts++
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "needs-owner-merge"}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repos/acme/widget/labels/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"name": strings.TrimPrefix(p, "/repos/acme/widget/labels/")})
		default:
			t.Logf("promotion fixture: unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		}
	}))
}

func promotionLaneRules() []config.PromotionRule {
	return []config.PromotionRule{{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}}
}

// promotionInheritRules is promotionLaneRules with inherit_human_merge_paths
// on: the repository's human-merge list does not stop that rule's promotions.
func promotionInheritRules() []config.PromotionRule {
	rules := promotionLaneRules()
	rules[0].InheritHumanMergePaths = true
	return rules
}

// promotionWorkflowChange is the workflow file main already carries.
func promotionWorkflowChange() []map[string]any {
	return []map[string]any{{"filename": ".github/workflows/build.yml", "status": "modified", "additions": 1, "deletions": 1, "changes": 2}}
}

type promotionEngineOpts struct {
	rules      []config.PromotionRule
	humanPaths []string
	// currentBase turns auto_merge.require_current_base on for the repo;
	// integrationSelf admits the self-authored lane for integration probes.
	currentBase     bool
	integrationSelf bool
	// laneStopped: no self-authored sweep runs over the client.
	laneStopped bool
	// branchesOnly wires the rules as PromotionBranches only (the lane is
	// gated off, the write guards still see them).
	branchesOnly bool
}

func newPromotionEngine(t *testing.T, apiURL string, o promotionEngineOpts) (*Engine, *[]hgithub.AuditRecord) {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	var audits []hgithub.AuditRecord
	var mu sync.Mutex
	client.SetAttributionHooks(hgithub.AttributionHooks{AuditRecord: func(r hgithub.AuditRecord) {
		mu.Lock()
		defer mu.Unlock()
		if r.Action == hgithub.AuditActionPromotionDecision {
			audits = append(audits, r)
		}
	}})
	opts := Options{
		BasePreparation: NewBasePreparationStore(""),
		HeadFirstSeen:   NewHeadFirstSeenTracker(),
		MergerAuthorizer: func(login string) bool {
			return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
		},
		SentinelLabel:  func() string { return "sentinel-alert" },
		HumanMergeRule: func() prstage.HumanMergeRule { return prstage.HumanMergeRule{Labels: []string{"needs-owner-merge"}} },
	}
	if o.rules != nil {
		rules := o.rules
		byRepo := func(repo string) []config.PromotionRule {
			if strings.EqualFold(repo, "acme/widget") {
				return rules
			}
			return nil
		}
		if o.branchesOnly {
			opts.PromotionBranches = byRepo
		} else {
			opts.Promotions = byRepo
		}
	}
	if o.humanPaths != nil {
		paths := o.humanPaths
		opts.HumanMergePaths = func(string) []string { return paths }
	}
	if o.currentBase {
		opts.RequireCurrentBase = func(string) bool { return true }
	}
	if o.integrationSelf {
		opts.IntegrationSelfAllowed = func() bool { return true }
	}
	e := New(client, opts)
	if !o.laneStopped {
		// The self-authored sweep (and so the promotion lane) runs.
		t.Cleanup(e.markSelfAuthoredLaneRunning())
	}
	return e, &audits
}

func promotionCmp(status string, ahead int, mergeBase, tree string) map[string]any {
	return map[string]any{
		"status": status, "ahead_by": ahead,
		"merge_base_commit": map[string]any{"sha": mergeBase, "commit": map[string]any{"tree": map[string]any{"sha": tree}}},
	}
}

// A tree-equal promote from a listed author and ref merges on green checks,
// with no linked issue and no author tier, and the merge is audited with the
// head, source tip and tree. The live tree check runs again right before the
// merge.
func TestPromotionLaneMergesTreeEqualPromotion(t *testing.T) {
	fx := &promotionSweepFixture{number: 31, head: "tip31", tips: []string{"tip31"}, trees: map[string]string{"tip31": "tree31"}, files: promotionWorkflowChange(), labels: []string{"release/ready"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})

	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 31, true)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = %q, %v; want a merge", reason, err)
	}
	if len(fx.merged) != 1 || event.Tier != lanePromotion || event.HeadSHA != "tip31" {
		t.Fatalf("merged=%v event=%+v, want PR 31 merged on the promotion lane", fx.merged, event)
	}
	if fx.branchHits != 2 {
		t.Fatalf("source branch read %d times, want 2 (routing, then right before the merge)", fx.branchHits)
	}
	if len(*audits) != 1 {
		t.Fatalf("promotion audits = %+v, want one", *audits)
	}
	for _, want := range []string{"decision=merged", "head=tip31", "source=main", "source_sha=tip31", "tree=tree31"} {
		if !strings.Contains((*audits)[0].Detail, want) {
			t.Fatalf("audit detail %q missing %q", (*audits)[0].Detail, want)
		}
	}
}

// The source branch moves between routing and the merge: the re-check
// against the live tip fails, nothing merges this pass, and the refusal is
// audited.
func TestPromotionLaneSourceMovedBeforeMergeDoesNotMerge(t *testing.T) {
	fx := &promotionSweepFixture{
		number: 32, head: "tip32", files: promotionWorkflowChange(),
		tips:     []string{"tip32", "newtip"},
		trees:    map[string]string{"tip32": "tree32", "newtip": "tree-new"},
		compares: map[string]map[string]any{"newtip...tip32": promotionCmp("behind", 0, "tip32", "tree32")},
	}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 32, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR: %v", err)
	}
	if reason != autoMergeReasonPromotionSourceMoved || len(fx.merged) != 0 {
		t.Fatalf("reason=%q merged=%v, want %s and no merge", reason, fx.merged, autoMergeReasonPromotionSourceMoved)
	}
	if len(*audits) != 1 || !strings.Contains((*audits)[0].Detail, "decision=merge-refused:"+hgithub.PromotionReasonTreeMismatch) || !strings.Contains((*audits)[0].Detail, "source_sha=newtip") {
		t.Fatalf("audits = %+v, want one merge-refused tree mismatch against newtip", *audits)
	}
}

// Every way the match can fail leaves the pull request where it is today:
// the self-authored sweep does not own it.
func TestPromotionLaneNoMatchIsUnchanged(t *testing.T) {
	tipOnly := func(author, ref string) func() *promotionSweepFixture {
		return func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 33, head: "tip33", author: author, ref: ref, tips: []string{"tip33"}, trees: map[string]string{"tip33": "t"}}
		}
	}
	for _, tc := range []struct {
		name string
		fx   func() *promotionSweepFixture
		opts promotionEngineOpts
	}{
		{name: "feature empty", fx: tipOnly("", "")},
		{name: "repository not listed", fx: tipOnly("", ""), opts: promotionEngineOpts{rules: []config.PromotionRule{}}},
		{name: "author not in the list", fx: tipOnly("mallory", ""), opts: promotionEngineOpts{rules: promotionLaneRules()}},
		{name: "head ref prefix mismatch", fx: tipOnly("", "feature/promote"), opts: promotionEngineOpts{rules: promotionLaneRules()}},
		{name: "head tree differs from the source tip by one file", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 33, head: "old33", tips: []string{"tip33"}, trees: map[string]string{"tip33": "tree-tip"}, headTree: "tree-old"}
		}, opts: promotionEngineOpts{rules: promotionLaneRules()}},
		{name: "a commit not reachable from source", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{
				number: 33, head: "extra33", tips: []string{"tip33"}, trees: map[string]string{"tip33": "tree-tip"}, headParents: []string{"tip33", "side"},
				compares: map[string]map[string]any{"tip33...extra33": promotionCmp("ahead", 1, "tip33", "tree-tip")},
			}
		}, opts: promotionEngineOpts{rules: promotionLaneRules()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fx()
			api := newPromotionSweepAPI(t, fx)
			defer api.Close()
			c, _ := newPromotionEngine(t, api.URL, tc.opts)
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 33, true)
			if err != nil || reason != "not-app-authored" || len(fx.merged) != 0 {
				t.Fatalf("reason=%q err=%v merged=%v, want not-app-authored and no merge", reason, err, fx.merged)
			}
			pr := &gh.PullRequest{
				Number: gh.Ptr(33), State: gh.Ptr("open"),
				User: &gh.User{Login: gh.Ptr(fx.author)},
				Head: &gh.PullRequestBranch{SHA: gh.Ptr(fx.head), Ref: gh.Ptr(fx.ref), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
				Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
			}
			if got := c.prefilterSelfAuthoredPR(pr); got != "not-app-authored" {
				t.Fatalf("prefilterSelfAuthoredPR = %q, want not-app-authored", got)
			}
			if c.promotionLaneOwns(context.Background(), "acme", "widget", pr) {
				t.Fatal("other lanes would hand a non-matching pull request off")
			}
		})
	}
}

// A hold, needs-owner-merge, a human-merge path or a sentinel label still
// blocks a matching promotion: the promotion lane is the self-authored
// sweep's gates with a content check, not a way around them.
func TestPromotionLaneGatesStillBlock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		paths  []string
		want   string
	}{
		{name: "hold label", labels: []string{"hold"}, want: "label:hold"},
		{name: "needs-owner-merge", labels: []string{"needs-owner-merge"}, want: autoMergeReasonHumanMergeRequired},
		{name: "human-merge path", paths: []string{".github/workflows/**"}, want: autoMergeReasonHumanMergePath},
		// The App's own pull requests pass a sentinel label; a promotion's
		// author is not trusted, so it does not.
		{name: "sentinel label", labels: []string{"sentinel-alert"}, want: "sentinel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &promotionSweepFixture{number: 34, head: "tip34", tips: []string{"tip34"}, trees: map[string]string{"tip34": "tree34"}, files: promotionWorkflowChange(), labels: tc.labels}
			api := newPromotionSweepAPI(t, fx)
			defer api.Close()
			c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules(), humanPaths: tc.paths})
			_, reason, _ := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 34, true)
			if reason != tc.want || len(fx.merged) != 0 {
				t.Fatalf("reason=%q merged=%v, want %q and no merge", reason, fx.merged, tc.want)
			}
		})
	}
}

// The trusted-author and contributor lanes hand a matching promotion to the
// promotion lane instead of refusing it for a missing linked issue or tier,
// and the hand-off carries no stage opinion of its own.
func TestPromotionLaneHandOff(t *testing.T) {
	fx := &promotionSweepFixture{number: 35, head: "tip35", tips: []string{"tip35"}, trees: map[string]string{"tip35": "tree35"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	pr := &gh.PullRequest{
		Number: gh.Ptr(35), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip35"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	// The contributor lane hands off only in a pass where the self-authored
	// sweep (and so the promotion lane) runs.
	inPass := contextWithPromotionHandoff(context.Background())
	if got := c.prefilterContributorLanePRIn(inPass, "acme", "widget", pr, ContributorLanePolicy{Enabled: true}); got != promotionLaneSkipReason {
		t.Fatalf("contributor lane prefilter = %q, want %s", got, promotionLaneSkipReason)
	}
	if got := c.prefilterContributorLanePR(pr, ContributorLanePolicy{Enabled: true}); got == promotionLaneSkipReason {
		t.Fatal("the contributor lane handed a promotion to a lane that is not running")
	}
	if got := c.prefilterSelfAuthoredPR(pr); got != "" {
		t.Fatalf("self-authored prefilter = %q, want the promotion lane to take it", got)
	}
	if got := c.selfAuthoredSweepTier(pr); got != lanePromotion {
		t.Fatalf("tier = %q, want %s", got, lanePromotion)
	}
	// One live read serves the routing questions of one pass.
	if fx.branchHits != 1 {
		t.Fatalf("source branch read %d times for routing, want 1", fx.branchHits)
	}
	// A payload rebuilt from the ForgeState cache carries no base
	// repository; the lanes pass the repository they listed it from.
	cached := pullRequestFromItem(forgestate.Item{Number: 35, State: "open", Author: "github-actions[bot]", HeadSHA: "tip35", HeadRef: "auto/promote-main-to-stable", HeadRepo: "acme/widget", BaseRef: "stable"})
	if got := c.prefilterSelfAuthoredPRIn("acme", "widget", cached); got != "" {
		t.Fatalf("self-authored prefilter on a cached payload = %q, want the promotion lane to take it", got)
	}
	if !c.promotionLaneOwns(context.Background(), "acme", "widget", cached) {
		t.Fatal("a cached payload was not handed to the promotion lane")
	}
	if v := classifyStage(stageObservation{Lane: stageLaneContributor, Reason: promotionLaneSkipReason}); v.Rank != stageRankNone {
		t.Fatalf("hand-off classified as %+v, want no opinion", v)
	}
	// An author with a lane of its own keeps it: a trusted bot is never
	// re-routed by a promotion rule.
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{"github-actions[bot]": true} }
	if c.promotionLaneOwns(context.Background(), "acme", "widget", pr) {
		t.Fatal("a trusted-bot author was handed to the promotion lane")
	}
	if got := c.prefilterContributorLanePRIn(inPass, "acme", "widget", pr, ContributorLanePolicy{Enabled: true}); got != trustedBotLaneSkipReason {
		t.Fatalf("contributor lane prefilter = %q, want %s for a trusted bot", got, trustedBotLaneSkipReason)
	}
}

// The merge result must equal the source too: GitHub's test merge of the
// head onto the live target tip. On a target that diverged (every squash
// promote), a change the source reverted can survive the three-way merge.
func TestPromotionLaneMergeResultMustEqualSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fx      func() *promotionSweepFixture
		want    string
		decided string
	}{
		{name: "test merge keeps a file the source reverted", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 36, head: "tip36", tips: []string{"tip36"}, trees: map[string]string{"tip36": "tree36"}, mergeTree: "tree36-plus-reverted-file"}
		}, want: hgithub.PromotionReasonMergeTreeMismatch, decided: "decision=merge-refused:" + hgithub.PromotionReasonMergeTreeMismatch},
		{name: "mergeable unknown", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 36, head: "tip36", tips: []string{"tip36"}, trees: map[string]string{"tip36": "tree36"}, mergeableUnknown: true}
		}, want: hgithub.PromotionReasonMergeUnknown, decided: "decision=merge-refused:" + hgithub.PromotionReasonMergeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fx()
			api := newPromotionSweepAPI(t, fx)
			defer api.Close()
			c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 36, true)
			if err != nil || reason != tc.want || len(fx.merged) != 0 {
				t.Fatalf("reason=%q err=%v merged=%v, want %s and no merge", reason, err, fx.merged, tc.want)
			}
			found := false
			for _, a := range *audits {
				found = found || strings.Contains(a.Detail, tc.decided)
			}
			if !found {
				t.Fatalf("audits = %+v, want %s", *audits, tc.decided)
			}
		})
	}
}

// The hive never writes to a promotion branch. A promotion the target
// reports behind (it requires up-to-date branches) holds for a person
// instead of being updated, and require_current_base is replaced by the
// merge-result check rather than updating the branch to the target tip.
func TestPromotionLaneNeverUpdatesTheBranch(t *testing.T) {
	behind := &promotionSweepFixture{number: 37, head: "tip37", tips: []string{"tip37"}, trees: map[string]string{"tip37": "tree37"}, mergeableState: "behind"}
	api := newPromotionSweepAPI(t, behind)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	_, reason, _ := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 37, true)
	if reason != autoMergeReasonPromotionBehindTarget || behind.updates != 0 || len(behind.merged) != 0 {
		t.Fatalf("reason=%q updates=%d merged=%v, want %s with no branch update", reason, behind.updates, behind.merged, autoMergeReasonPromotionBehindTarget)
	}
	pr := &gh.PullRequest{
		Number: gh.Ptr(37), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip37"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), SHA: gh.Ptr("basesha"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if _, reason, _ := c.updateBranchAndSkip(context.Background(), "widget", "acme", "widget", 37, "tip37", "basesha", "behind", pr); reason != autoMergeReasonPromotionBehindTarget || behind.updates != 0 {
		t.Fatalf("updateBranchAndSkip = %q with %d updates, want %s and none", reason, behind.updates, autoMergeReasonPromotionBehindTarget)
	}
	if !c.leaveBaseSyncToDependencyBot("widget", "acme", "widget", pr, "merge-train-rebase") {
		t.Fatal("the merge train would rebase a promotion branch")
	}

	strict := &promotionSweepFixture{number: 38, head: "tip38", tips: []string{"tip38"}, trees: map[string]string{"tip38": "tree38"}}
	api2 := newPromotionSweepAPI(t, strict)
	defer api2.Close()
	c2, _ := newPromotionEngine(t, api2.URL, promotionEngineOpts{rules: promotionLaneRules(), currentBase: true})
	if _, reason, err := c2.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 38, true); err != nil || reason != "" || len(strict.merged) != 1 || strict.updates != 0 {
		t.Fatalf("require_current_base: reason=%q err=%v merged=%v updates=%d, want a merge proven by the test merge and no update", reason, err, strict.merged, strict.updates)
	}
}

// Integration execution routes a promotion to the promotion lane, under the
// self-authored admission, instead of the contributor lane that would only
// hand it back.
func TestPromotionLaneIntegrationEligibility(t *testing.T) {
	fx := &promotionSweepFixture{number: 39, head: "tip39", tips: []string{"tip39"}, trees: map[string]string{"tip39": "tree39"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules(), integrationSelf: true})
	pr, reason, err := c.IntegrationEligibility(context.Background(), "widget", "stable", 39, "tip39")
	if err != nil || reason != "" || pr == nil || pr.GetNumber() != 39 {
		t.Fatalf("IntegrationEligibility = %v, %q, %v; want the promotion captured by its lane", pr, reason, err)
	}
	if len(fx.merged) != 0 {
		t.Fatalf("the eligibility probe merged: %v", fx.merged)
	}
	off, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	if pr, reason, _ := off.IntegrationEligibility(context.Background(), "widget", "stable", 39, "tip39"); pr != nil || !strings.Contains(reason, "self-authored") {
		t.Fatalf("without self-authored admission: %v, %q; want a refusal naming it", pr, reason)
	}
}

// The merge relay names the promotion lane instead of a contributor-lane
// refusal that would read as a hand merge.
func TestPromotionLaneRelayRefusalNamesTheLane(t *testing.T) {
	fx := &promotionSweepFixture{number: 40, head: "tip40", tips: []string{"tip40"}, trees: map[string]string{"tip40": "tree40"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	refusal, err := c.RelayMergeRefusal(context.Background(), "widget", 40, "")
	if err != nil || !strings.HasPrefix(refusal, RelayMergeRefusalPrefix) || !strings.Contains(refusal, ": "+promotionLaneSkipReason+" (") {
		t.Fatalf("refusal = %q, %v; want the promotion-lane reason", refusal, err)
	}
}

// A promotion the lane queued that no longer matches leaves GitHub's merge
// queue; one that still matches stays. Only mismatches withdraw: a test
// merge GitHub is still recomputing does not.
func TestPromotionLaneWithdrawsStaleQueueEntry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fx       func() *promotionSweepFixture
		dequeued int
	}{
		{name: "still matches", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 41, head: "tip41", tips: []string{"tip41"}, trees: map[string]string{"tip41": "tree41"}}
		}},
		{name: "source moved on", fx: func() *promotionSweepFixture {
			return &promotionSweepFixture{number: 41, head: "tip41", tips: []string{"newtip"}, trees: map[string]string{"newtip": "tree-new", "tip41": "tree41"},
				compares: map[string]map[string]any{"newtip...tip41": promotionCmp("behind", 0, "tip41", "tree41")}}
		}, dequeued: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fx()
			api := newPromotionSweepAPI(t, fx)
			defer api.Close()
			c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
			enableNativeQueue(c)
			q := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_41", HeadRefOid: "tip41", IsMergeQueueEnabled: true, IsInMergeQueue: true, MergeQueueEntry: queueEntry(testHiveAppBotLogin)}}
			c.transport = q
			pr := &gh.PullRequest{
				Number: gh.Ptr(41), NodeID: gh.Ptr("PR_41"), State: gh.Ptr("open"),
				User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
				Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip41"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
				Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
			}
			c.promotionWithdrawIfStale(context.Background(), stageLaneSelf, "acme", "widget", pr)
			if q.dequeues != tc.dequeued {
				t.Fatalf("dequeues = %d, want %d", q.dequeues, tc.dequeued)
			}
		})
	}
	for _, reason := range []string{autoMergeReasonPromotionSourceMoved, autoMergeReasonPromotionNoLongerMatches, hgithub.PromotionReasonMergeTreeMismatch} {
		if !nativeQueueWithdrawReason(reason) {
			t.Errorf("%s does not withdraw a queue entry", reason)
		}
	}
	if nativeQueueWithdrawReason(hgithub.PromotionReasonMergeUnknown) {
		t.Error("a test merge GitHub is recomputing withdraws the queue entry")
	}
}

// A promotion refused for a reason no later pass cures on that head (the
// target requires up-to-date branches, a serialized repository, a test
// merge that differs from the source) is handed back: the lane stops
// claiming it, so the other lanes route it as any pull request and a person
// is told to merge it, instead of a silent hold. A new head is judged again.
func TestPromotionLaneHandsBackPermanentRefusals(t *testing.T) {
	fx := &promotionSweepFixture{number: 42, head: "tip42", tips: []string{"tip42"}, trees: map[string]string{"tip42": "tree42"}, mergeableState: "behind"}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	if _, reason, _ := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 42, true); reason != autoMergeReasonPromotionBehindTarget {
		t.Fatalf("reason = %q, want %s", reason, autoMergeReasonPromotionBehindTarget)
	}
	pr := &gh.PullRequest{
		Number: gh.Ptr(42), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip42"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if c.promotionLaneOwns(context.Background(), "acme", "widget", pr) {
		t.Fatal("the lane still claims a head it handed back")
	}
	if got := c.prefilterContributorLanePRIn(contextWithPromotionHandoff(context.Background()), "acme", "widget", pr, ContributorLanePolicy{Enabled: true}); got == promotionLaneSkipReason {
		t.Fatal("the contributor lane still hands a handed-back head to the promotion lane")
	}
	if refusal, _ := c.RelayMergeRefusal(context.Background(), "widget", 42, ""); strings.Contains(refusal, ": "+promotionLaneSkipReason+" (") {
		t.Fatalf("the relay still says the promotion lane merges it: %q", refusal)
	}
	handedBack := 0
	for _, a := range *audits {
		if strings.Contains(a.Detail, "decision=handed-back:"+autoMergeReasonPromotionBehindTarget) {
			handedBack++
		}
	}
	if handedBack != 1 {
		t.Fatalf("audits = %+v, want one hand-back", *audits)
	}
	// A new head is judged afresh.
	pr.Head.SHA = gh.Ptr("tip42b")
	if c.promotionHandedBack("acme", "widget", 42, "tip42b") {
		t.Fatal("a new head inherited the hand-back")
	}
}

// Nothing claims a promotion for the lane while no self-authored sweep runs
// over the client: the relay and the other lanes keep today's path.
func TestPromotionLaneOwnsOnlyWhileTheLaneRuns(t *testing.T) {
	fx := &promotionSweepFixture{number: 43, head: "tip43", tips: []string{"tip43"}, trees: map[string]string{"tip43": "tree43"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules(), laneStopped: true})
	pr := &gh.PullRequest{
		Number: gh.Ptr(43), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip43"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if c.promotionLaneOwns(context.Background(), "acme", "widget", pr) {
		t.Fatal("a promotion was claimed for a lane that is not running")
	}
	release := c.markSelfAuthoredLaneRunning()
	if !c.promotionLaneOwns(context.Background(), "acme", "widget", pr) {
		t.Fatal("a running lane does not claim its promotion")
	}
	release()
	release() // idempotent
	if c.selfAuthoredLaneRunning() {
		t.Fatal("the lane still counts as running after it stopped")
	}
}

// After a direct merge the landed commit's tree is compared with the test
// merge tree the lane verified; a difference is audited (no rollback).
func TestPromotionLaneAuditsAMergedTreeMismatch(t *testing.T) {
	fx := &promotionSweepFixture{number: 44, head: "tip44", tips: []string{"tip44"}, trees: map[string]string{"tip44": "tree44"}, mergedTree: "tree-other"}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 44, true); err != nil || reason != "" || len(fx.merged) != 1 {
		t.Fatalf("reason=%q err=%v merged=%v, want a merge", reason, err, fx.merged)
	}
	found := false
	for _, a := range *audits {
		if strings.Contains(a.Detail, "decision=merged-tree-mismatch") && strings.Contains(a.Detail, "merge_tree=tree-other") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audits = %+v, want merged-tree-mismatch", *audits)
	}
}

// The write guards see every configured rule, lane or no lane: the hive
// never updates a promotion branch even where the promotion lane is off.
func TestPromotionBranchGuardIgnoresTheLaneGate(t *testing.T) {
	c, _ := newPromotionEngine(t, "http://127.0.0.1:0", promotionEngineOpts{rules: promotionLaneRules(), branchesOnly: true, laneStopped: true})
	pr := &gh.PullRequest{
		Number: gh.Ptr(45), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip45"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if _, ok := c.promotionRule("acme", "widget", pr); ok {
		t.Fatal("the lane-gated rules were consulted")
	}
	if !c.leaveBaseSyncToDependencyBot("widget", "acme", "widget", pr, "update-branch") {
		t.Fatal("the hive would update a promotion branch while the lane is off")
	}
	if _, reason, _ := c.updateBranchAndSkip(context.Background(), "widget", "acme", "widget", 45, "tip45", "basesha", "behind", pr); reason != autoMergeReasonPromotionBehindTarget {
		t.Fatalf("updateBranchAndSkip = %q, want %s", reason, autoMergeReasonPromotionBehindTarget)
	}
}

// A `git commit-tree <main tree> -p origin/stable` promote, the shape real
// promote workflows build: one commit on the live target tip carrying the
// source tree, never on the source itself. It merges like a pushed source
// tip.
func TestPromotionLaneMergesCommitTreePromotion(t *testing.T) {
	fx := &promotionSweepFixture{number: 46, head: "ct46", tips: []string{"tip46"}, trees: map[string]string{"tip46": "tree46"}, headTree: "tree46", headParents: []string{"basesha"}}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules()})
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 46, true); err != nil || reason != "" || len(fx.merged) != 1 {
		t.Fatalf("reason=%q err=%v merged=%v, want the commit-tree promotion merged", reason, err, fx.merged)
	}
	found := false
	for _, a := range *audits {
		found = found || (strings.Contains(a.Detail, "decision=merged") && strings.Contains(a.Detail, "head=ct46"))
	}
	if !found {
		t.Fatalf("audits = %+v, want a merged decision for ct46", *audits)
	}
}

// inherit_human_merge_paths (automerge_promotion_human_merge_paths.go). On
// v6 tip PromotionRule has no such field, so none of the tests below
// compile; with the field alone and no lane change, the first one fails as
// human-merge-path.

// promotionHumanMergePaths is a repository whose workflows a person merges.
var promotionHumanMergePaths = []string{".github/workflows/**"}

// promotionInheritSetAsideLog starts the line the lane logs, once per head,
// when it sets the human-merge-path gate aside for a head that changes a
// listed file under a rule with inherit_human_merge_paths on
// (promotionInheritedHumanMergePaths).
const promotionInheritSetAsideLog = "promotion lane: the rule has inherit_human_merge_paths on"

// promotionLogProbe is a slog.Handler that hands each record's message to
// seen, so a test can tell which point of a pass the lane has reached.
type promotionLogProbe struct{ seen func(msg string) }

func (p promotionLogProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p promotionLogProbe) Handle(_ context.Context, r slog.Record) error {
	p.seen(r.Message)
	return nil
}
func (p promotionLogProbe) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p promotionLogProbe) WithGroup(string) slog.Handler      { return p }

// watchPromotionInheritSetAside makes c set setAside when the lane sets the
// human-merge-path gate aside under the option.
func watchPromotionInheritSetAside(c *Engine, setAside *atomic.Bool) {
	c.logger = slog.New(promotionLogProbe{seen: func(msg string) {
		if strings.HasPrefix(msg, promotionInheritSetAsideLog) {
			setAside.Store(true)
		}
	}})
}

func promotionAuditWith(audits []hgithub.AuditRecord, wants ...string) bool {
	for _, a := range audits {
		all := true
		for _, want := range wants {
			all = all && strings.Contains(a.Detail, want)
		}
		if all {
			return true
		}
	}
	return false
}

// With the rule's option on, a tree-identical promote that changes a file on
// the repository's human-merge list merges on green checks: nothing is
// labelled, no notice is posted, and the merge's audit entry says the list
// was set aside by the rule's option (human_merge_paths=promotion-rule-option),
// with the files and the tree. With the option off (the default) the same
// pull request is left for a person exactly as before.
func TestPromotionLaneInheritHumanMergePaths(t *testing.T) {
	newFx := func() *promotionSweepFixture {
		return &promotionSweepFixture{number: 47, head: "tip47", tips: []string{"tip47"}, trees: map[string]string{"tip47": "tree47"}, files: promotionWorkflowChange()}
	}
	t.Run("option on merges and audits", func(t *testing.T) {
		fx := newFx()
		api := newPromotionSweepAPI(t, fx)
		defer api.Close()
		c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionInheritRules(), humanPaths: promotionHumanMergePaths})
		event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 47, true)
		if err != nil || reason != "" {
			t.Fatalf("trySweepSelfAuthoredPR = %q, %v; want a merge", reason, err)
		}
		if len(fx.merged) != 1 || event.Tier != lanePromotion {
			t.Fatalf("merged=%v event=%+v, want PR 47 merged on the promotion lane", fx.merged, event)
		}
		// The gate labels before it posts its notice, so no label means the
		// pull request was never handed to a person.
		if fx.labelPosts != 0 {
			t.Fatalf("labels=%d, want the pull request not labelled needs-owner-merge", fx.labelPosts)
		}
		if fx.branchHits != 2 {
			t.Fatalf("source branch read %d times, want 2 (routing, then right before the merge)", fx.branchHits)
		}
		if !promotionAuditWith(*audits, "decision=merged", "tree=tree47", "source_sha=tip47", "human_merge_paths="+hgithub.PromotionHumanMergePathsRuleOption, "human_merge_path_files=.github/workflows/build.yml") {
			t.Fatalf("audits = %+v, want the merge recorded with the rule-option note, the listed files and the tree", *audits)
		}
	})
	t.Run("option off is a person's merge as before", func(t *testing.T) {
		fx := newFx()
		api := newPromotionSweepAPI(t, fx)
		defer api.Close()
		c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionLaneRules(), humanPaths: promotionHumanMergePaths})
		_, reason, _ := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 47, true)
		if reason != autoMergeReasonHumanMergePath || len(fx.merged) != 0 || fx.labelPosts != 1 {
			t.Fatalf("reason=%q merged=%v labels=%d, want %s, no merge and the needs-owner-merge label", reason, fx.merged, fx.labelPosts, autoMergeReasonHumanMergePath)
		}
		if promotionAuditWith(*audits, "human_merge_paths=") {
			t.Fatalf("audits = %+v, want nothing recorded as inherited", *audits)
		}
	})
	t.Run("option on with no listed file changed records nothing inherited", func(t *testing.T) {
		fx := newFx()
		fx.files = []map[string]any{{"filename": "README.md", "status": "modified", "additions": 1, "deletions": 0, "changes": 1}}
		api := newPromotionSweepAPI(t, fx)
		defer api.Close()
		c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionInheritRules(), humanPaths: promotionHumanMergePaths})
		if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 47, true); err != nil || reason != "" || len(fx.merged) != 1 {
			t.Fatalf("reason=%q err=%v merged=%v, want a merge", reason, err, fx.merged)
		}
		if !promotionAuditWith(*audits, "decision=merged") || promotionAuditWith(*audits, "human_merge_paths=") {
			t.Fatalf("audits = %+v, want a merge with no inherited note", *audits)
		}
	})
}

// The option lifts the hive's own human-merge-path stop and nothing else. A
// person's hold, a needs-owner-merge label on the pull request (the lane
// never decides whose it is; only the stage pass removes one, and only the
// hive's own), the sentinel label and a failing required check stop the
// promotion before the lane reaches the gate the option sets aside. The
// review verdict hold comes after it: the lane sets the human-merge list
// aside, labels nothing, and is still stopped by the verdict.
func TestPromotionLaneInheritHumanMergePathsOtherGatesStillBlock(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      []string
		checkState  string
		verdictHold bool
		want        string
		// pastGate: the lane got past the human-merge-path gate (set aside
		// under the option) before it was stopped.
		pastGate bool
	}{
		{name: "a person's hold", labels: []string{"hold"}, want: "label:hold"},
		{name: "a person's needs-owner-merge", labels: []string{"needs-owner-merge"}, want: autoMergeReasonHumanMergeRequired},
		{name: "sentinel label", labels: []string{"sentinel-alert"}, want: "sentinel"},
		{name: "failing required check", checkState: "failure", want: "required-check-failing:ci/build"},
		{name: "review verdict hold, after the gate the option sets aside", verdictHold: true, want: autoMergeReasonReviewRequiresHuman, pastGate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &promotionSweepFixture{number: 48, head: "tip48", tips: []string{"tip48"}, trees: map[string]string{"tip48": "tree48"}, files: promotionWorkflowChange(), labels: tc.labels, checkState: tc.checkState}
			api := newPromotionSweepAPI(t, fx)
			defer api.Close()
			c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionInheritRules(), humanPaths: promotionHumanMergePaths})
			if tc.verdictHold {
				// A hold with no recording time cannot have been released
				// since, so the gate answers without reading GitHub.
				c.reviewVerdictHold = func(string, int) ReviewVerdictHold {
					return ReviewVerdictHold{Held: true, Verdict: "requires_human", HeadSHA: "tip48"}
				}
			}
			var setAside atomic.Bool
			watchPromotionInheritSetAside(c, &setAside)
			_, reason, _ := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 48, true)
			if len(fx.merged) != 0 || reason != tc.want {
				t.Fatalf("reason=%q merged=%v, want %q and no merge", reason, fx.merged, tc.want)
			}
			if got := setAside.Load(); got != tc.pastGate {
				t.Fatalf("human-merge-path gate set aside = %v, want %v: the case must stop the lane %s that gate", got, tc.pastGate, map[bool]string{true: "after", false: "before"}[tc.pastGate])
			}
			if fx.labelPosts != 0 {
				t.Fatalf("labels=%d, want the pull request not labelled needs-owner-merge: the option lifts that stop, and no other gate here labels", fx.labelPosts)
			}
			if promotionAuditWith(*audits, "decision=merged") {
				t.Fatalf("audits = %+v, want no merge recorded", *audits)
			}
		})
	}
}

// A head whose tree differs from the source by one file is not the lane's,
// option or no option: the self-authored sweep does not own it, no other
// lane hands it off, and the human-merge-path gate those lanes run still
// refuses and labels it.
func TestPromotionLaneInheritHumanMergePathsNeedsTheTreeMatch(t *testing.T) {
	fx := &promotionSweepFixture{number: 49, head: "old49", tips: []string{"tip49"}, trees: map[string]string{"tip49": "tree-tip"}, headTree: "tree-old", files: promotionWorkflowChange()}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, _ := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionInheritRules(), humanPaths: promotionHumanMergePaths})
	ctx := context.Background()
	if _, reason, err := c.trySweepSelfAuthoredPR(ctx, "widget", "acme", "widget", 49, true); err != nil || reason != "not-app-authored" || len(fx.merged) != 0 {
		t.Fatalf("reason=%q err=%v merged=%v, want not-app-authored and no merge", reason, err, fx.merged)
	}
	pr := &gh.PullRequest{
		Number: gh.Ptr(49), State: gh.Ptr("open"), ChangedFiles: gh.Ptr(len(fx.files)),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("old49"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	if c.promotionLaneOwns(ctx, "acme", "widget", pr) {
		t.Fatal("the lane claims a head whose tree is not the source's")
	}
	// Today's path: the gate the trusted-author, contributor and queued
	// lanes run on a pull request they keep.
	if reason, err := c.humanMergePathGate(ctx, "widget", "acme", "widget", pr); err != nil || reason != autoMergeReasonHumanMergePath {
		t.Fatalf("humanMergePathGate = %q, %v; want %s", reason, err, autoMergeReasonHumanMergePath)
	}
	if fx.labelPosts != 1 || fx.commentPosts != 1 {
		t.Fatalf("labels=%d comments=%d, want the label and the notice", fx.labelPosts, fx.commentPosts)
	}
}

// The rule is read again right before the merge. If it lost the option
// after the lane set the gate aside, the human-merge list binds again in
// full: nothing merges and the pull request is labelled for a person.
//
// The option goes off at the first read of the pull request that follows
// the lane setting the gate aside (the lane says so in its log), which is
// at the latest the re-read right before the merge recheck. It does not
// hang on which read of the changed files is the gate's, so a files read
// added earlier in the pass cannot turn the option off before the gate and
// let this test pass through the plain gate.
func TestPromotionLaneInheritHumanMergePathsSwitchedOffMidPass(t *testing.T) {
	fx := &promotionSweepFixture{number: 50, head: "tip50", tips: []string{"tip50"}, trees: map[string]string{"tip50": "tree50"}, files: promotionWorkflowChange()}
	var setAside, off atomic.Bool
	fx.onPRRead = func() {
		if setAside.Load() {
			off.Store(true)
		}
	}
	api := newPromotionSweepAPI(t, fx)
	defer api.Close()
	c, audits := newPromotionEngine(t, api.URL, promotionEngineOpts{rules: promotionInheritRules(), humanPaths: promotionHumanMergePaths})
	watchPromotionInheritSetAside(c, &setAside)
	c.promotions = func(repo string) []config.PromotionRule {
		if !strings.EqualFold(repo, "acme/widget") {
			return nil
		}
		if off.Load() {
			return promotionLaneRules()
		}
		return promotionInheritRules()
	}
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 50, true)
	if !setAside.Load() || !off.Load() {
		t.Fatalf("gate set aside = %v, option switched off = %v (reason=%q); want the lane to set the gate aside under the option and the option to go off before the recheck", setAside.Load(), off.Load(), reason)
	}
	if err != nil || reason != autoMergeReasonHumanMergePath || len(fx.merged) != 0 {
		t.Fatalf("reason=%q err=%v merged=%v, want %s and no merge", reason, err, fx.merged, autoMergeReasonHumanMergePath)
	}
	if fx.labelPosts != 1 {
		t.Fatalf("labels=%d, want the needs-owner-merge label", fx.labelPosts)
	}
	if promotionAuditWith(*audits, "decision=merged") {
		t.Fatalf("audits = %+v, want no merge recorded", *audits)
	}
}
