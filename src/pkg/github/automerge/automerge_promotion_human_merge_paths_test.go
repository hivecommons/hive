package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// auto_merge.promotions[].inherit_human_merge_paths, the stage pass's half:
// a needs-owner-merge label the hive itself put on a promotion for the
// human-merge-path refusal comes off once the promotion lane owns the head
// under a rule with the option on, checked against GitHub in that pass. A
// label a person set, a rule without the option, and a head or a test merge
// that does not carry the source tree keep it. On v6 tip the label always
// stays.

var promotionInheritHead = strings.Repeat("a", 40)

// promotionInheritFixture is what GitHub reports for the promote-main-to-
// stable pull request (#21) of promotionInheritStageAPI.
type promotionInheritFixture struct {
	// events is the label history (nil: the App's single application).
	events []map[string]any
	// sourceTip is what GET /branches/main reports; when it is not the
	// head, the head commit carries headTree and the source tip "tree-tip".
	sourceTip string
	headTree  string
	// mergeTree is the tree of GitHub's test merge commit "m21" (parents
	// the stable tip and the head); empty means the source tree.
	mergeTree string
	// prGets counts GET /pulls/21, and mergeCommitGets the reads of the
	// test merge commit, which only the release's live check makes.
	prGets          atomic.Int32
	mergeCommitGets atomic.Int32
}

// promotionInheritStageAPI is the human-merge-path lift fixture for a
// promote-main-to-stable pull request (#21) that changes a listed workflow
// and wears the label, with the App's head-stamped notice and label event
// as the gate writes them.
func promotionInheritStageAPI(t *testing.T, fx *promotionInheritFixture) *stageAPI {
	t.Helper()
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	notice := map[string]any{
		"id":         11,
		"body":       hgithub.HumanMergePathComment([]string{".github/workflows/release.yml"}) + "\n" + hgithub.HumanMergeHeadMarker(promotionInheritHead),
		"user":       app,
		"created_at": "2026-11-10T16:00:03Z",
		"updated_at": "2026-11-10T16:00:03Z",
	}
	events := fx.events
	if events == nil {
		events = []map[string]any{{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt}}
	}
	if fx.sourceTip == "" {
		fx.sourceTip = promotionInheritHead
	}
	mergeTree := fx.mergeTree
	if mergeTree == "" {
		mergeTree = "tree-tip"
	}
	files := append([]string{".github/workflows/release.yml"}, humanMergePathLiftCleanFiles...)
	api := newHumanMergePathLiftAPI(t, 21, files, []map[string]any{notice}, events)
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		switch r.URL.Path {
		case "/repos/acme/widget/pulls/21":
			fx.prGets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 21, "state": "open", "title": "Promote main to stable", "changed_files": len(files),
				"mergeable": true, "merge_commit_sha": "m21",
				"user":   map[string]string{"login": "github-actions[bot]", "type": "Bot"},
				"head":   map[string]any{"sha": promotionInheritHead, "ref": "auto/promote-main-to-stable", "repo": map[string]any{"full_name": "acme/widget"}},
				"base":   map[string]any{"ref": "stable", "repo": map[string]any{"full_name": "acme/widget"}},
				"labels": []map[string]string{{"name": hgithub.HumanMergePathLabel}},
			})
		case "/repos/acme/widget/pulls/21/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/widget/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": true, "commit": map[string]any{"sha": fx.sourceTip, "commit": map[string]any{"tree": map[string]any{"sha": "tree-tip"}}}})
		case "/repos/acme/widget/branches/main/protection":
			_ = json.NewEncoder(w).Encode(map[string]any{"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0}, "allow_force_pushes": map[string]any{"enabled": false}})
		case "/repos/acme/widget/branches/stable":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "stable", "commit": map[string]string{"sha": "basesha"}})
		case "/repos/acme/widget/git/commits/" + promotionInheritHead:
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": promotionInheritHead, "tree": map[string]any{"sha": fx.headTree}, "parents": []map[string]any{{"sha": "older"}}})
		case "/repos/acme/widget/git/commits/m21":
			fx.mergeCommitGets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "m21", "tree": map[string]any{"sha": mergeTree}, "parents": []map[string]any{{"sha": "basesha"}, {"sha": promotionInheritHead}}})
		default:
			return false
		}
		return true
	}
	return api
}

// promotionLabelledPR is the promotion wearing the human-merge-path label
// as a lane lists it: base stable, the promotion branch in the repository
// itself, opened by the workflow bot.
func promotionLabelledPR(extra ...string) *gh.PullRequest {
	labels := append([]string{hgithub.HumanMergePathLabel, "author/human", "role/contributor", "stage/050-review-pending"}, extra...)
	pr := stagePR(21, labels...)
	pr.Head.SHA = gh.Ptr(promotionInheritHead)
	pr.Head.Ref = gh.Ptr("auto/promote-main-to-stable")
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("acme/widget")}
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("stable")}
	pr.User = &gh.User{Login: gh.Ptr("github-actions[bot]")}
	return pr
}

// recordPromotionPR records pr as a lane sees it: every lane stops on the
// label (auto_merge.human_merge_labels) before its gate.
func recordPromotionPR(e *Engine, pr *gh.PullRequest) {
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonHumanMergeRequired, false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
}

func recordPromotionLabelledPR(e *Engine) { recordPromotionPR(e, promotionLabelledPR()) }

func promotionInheritStageEngine(t *testing.T, api *stageAPI, rules []config.PromotionRule) *Engine {
	t.Helper()
	e, _ := promotionInheritStageEngineWithClock(t, api, rules)
	return e
}

// promotionInheritStageEngineWithClock is promotionInheritStageEngine with
// the clock the engine and its stage store read, for a test that lets the
// release's recheck interval pass (the engine newHumanMergePathLiftEngine
// builds, on a clock the test keeps).
func promotionInheritStageEngineWithClock(t *testing.T, api *stageAPI, rules []config.PromotionRule) (*Engine, *contributorLaneClock) {
	t.Helper()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, api.url, clock)
	e.SetHumanMergePaths(func(string) []string { return []string{".github/workflows/**"} })
	e.promotions = func(repo string) []config.PromotionRule {
		if strings.EqualFold(repo, "acme/widget") {
			return rules
		}
		return nil
	}
	// The self-authored sweep (and so the promotion lane) runs.
	t.Cleanup(e.markSelfAuthoredLaneRunning())
	return e, clock
}

func TestApplyStageLabels_PromotionInheritReleasesTheHivesOwnLabel(t *testing.T) {
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	appLabelAt := func(at string) map[string]any {
		return map[string]any{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": at}
	}
	appLabel := appLabelAt(humanMergePathLiftHoldAt)
	otherTip := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name      string
		rules     []config.PromotionRule
		events    []map[string]any
		sourceTip string
		headTree  string
		mergeTree string
		stopped   bool
		// bare records the pull request without its refs and author, as a
		// lane that kept no payload would: the release then reads it.
		bare     bool
		released bool
	}{
		{name: "the hive's own label, option on, head is the source tip", rules: promotionInheritRules(), released: true},
		{name: "an observation without refs is read and judged the same", rules: promotionInheritRules(), bare: true, released: true},
		{name: "option off", rules: promotionLaneRules()},
		{name: "no promotion rule", rules: nil},
		{name: "a person re-added the label", rules: promotionInheritRules(), events: []map[string]any{
			appLabel,
			pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
			pathLabelEvent("labeled", "maintainer", "2026-11-10T17:00:05Z"),
		}},
		{name: "a person applied the label", rules: promotionInheritRules(), events: []map[string]any{
			pathLabelEvent("labeled", "maintainer", "2026-11-10T15:00:00Z"),
		}},
		// The release's provenance rule (the owner-approval one) asks who
		// applied the label, not who removed it: a person's removal that
		// the hive's gate answered by labelling the head again leaves the
		// hive's own label, and it is removed. A person who wants the
		// promotion kept adds hold.
		{name: "a person removed the label and the hive put it back", rules: promotionInheritRules(), events: []map[string]any{
			appLabel,
			pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
			appLabelAt("2026-11-10T17:00:05Z"),
		}, released: true},
		{name: "head tree differs from the source tip", rules: promotionInheritRules(), sourceTip: otherTip, headTree: "tree-other"},
		{name: "the test merge does not carry the source tree", rules: promotionInheritRules(), mergeTree: "tree-merged-other"},
		{name: "the promotion lane is not running", rules: promotionInheritRules(), stopped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := promotionInheritStageAPI(t, &promotionInheritFixture{events: tc.events, sourceTip: tc.sourceTip, headTree: tc.headTree, mergeTree: tc.mergeTree})
			var e *Engine
			if tc.stopped {
				e, _ = newHumanMergePathLiftEngine(t, api)
				rules := tc.rules
				e.promotions = func(string) []config.PromotionRule { return rules }
			} else {
				e = promotionInheritStageEngine(t, api, tc.rules)
			}
			record := func() {
				pr := promotionLabelledPR()
				if tc.bare {
					pr.Head.Ref, pr.Head.Repo, pr.Base = nil, nil, nil
				}
				recordPromotionPR(e, pr)
			}
			record()
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			removed := removedPathLabel(api)
			notices := humanMergePathLiftNotices(api)
			if h := removedHold(api); h != 0 {
				t.Fatalf("hold removed %d times: the release never touches hold", h)
			}
			if !tc.released {
				if removed != 0 || len(notices) != 0 {
					t.Fatalf("label removed %d times, notices %q; want the label kept", removed, notices)
				}
				return
			}
			if removed != 1 {
				t.Fatalf("label removed %d times (removed=%v), want once", removed, api.removed)
			}
			if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergePathLiftMarker+promotionInheritHead+" -->") {
				t.Fatalf("notices = %q, want one for the head", notices)
			}
			for _, want := range []string{
				"`" + hgithub.HumanMergePathLabel + "` removed", "promotes `main` to `stable`", "`inherit_human_merge_paths`",
				"GitHub's test merge of it carry exactly the current tree of `main`",
				"The hive checked the trees only. It did not check how or when those files reached `main`.",
				"A label a person sets, and any `hold`, is never removed this way.",
				"To keep this promotion for a person, add `hold`.",
			} {
				if !strings.Contains(notices[0], want) {
					t.Fatalf("notice lacks %q:\n%s", want, notices[0])
				}
			}
			// The notice claims only what was checked: nothing about how
			// the files reached the source, or when the label was set.
			for _, not := range []string{"already merged", "same list applied", "inherits what", "before the rule allowed"} {
				if strings.Contains(notices[0], not) {
					t.Fatalf("notice says %q, which the hive never checked:\n%s", not, notices[0])
				}
			}
			if strings.ContainsAny(notices[0], "@\u2013\u2014") {
				t.Fatalf("notice carries a mention or a dash:\n%s", notices[0])
			}

			// The same head labelled again (the merge-request relay or a
			// merger's queue label put it back): it stays, so two writers
			// never flap the label.
			api.reset()
			api.mu.Lock()
			api.issueEvents[21] = append(api.issueEvents[21],
				map[string]any{"event": "unlabeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": "2026-11-10T18:00:00Z"},
				map[string]any{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": "2026-11-10T18:05:00Z"},
			)
			api.mu.Unlock()
			record()
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := removedPathLabel(api); n != 0 {
				t.Fatalf("label removed again on the same head: %v", api.removed)
			}
			if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
				t.Fatalf("notice repeated on the same head: %q", notices)
			}
		})
	}
}

// A person's hold beside the hive's label is another source: the label
// stays, the hold is never touched, and the pass says so once for the head,
// not on every pass while the person holds it, nor when it judges the head
// again after the recheck interval.
func TestApplyStageLabels_PromotionInheritKeepsTheLabelBesideAPersonsHold(t *testing.T) {
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	events := []map[string]any{
		{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt},
		holdEvent("labeled", "maintainer", "2026-11-10T17:00:00Z"),
	}
	api := promotionInheritStageAPI(t, &promotionInheritFixture{events: events})
	e, clock := promotionInheritStageEngineWithClock(t, api, promotionInheritRules())
	var logs bytes.Buffer
	e.logger = slog.New(slog.NewTextHandler(&logs, nil))
	for pass := 1; pass <= 3; pass++ {
		if pass == 3 {
			// Past the recheck interval the pass judges the head again,
			// and still says it at info only the first time.
			clock.advance(promotionInheritReleaseRecheck + time.Minute)
		}
		api.reset()
		recordPromotionPR(e, promotionLabelledPR("hold"))
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n, h := removedPathLabel(api), removedHold(api); n != 0 || h != 0 {
			t.Fatalf("pass %d: removed=%v; neither the label nor a person's hold may come off while the person holds the PR", pass, api.removed)
		}
		if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
			t.Fatalf("pass %d: notice posted while a person holds the PR: %q", pass, notices)
		}
	}
	if n := strings.Count(logs.String(), "another hold source keeps the human-merge-path label"); n != 1 {
		t.Fatalf("hold beside the label logged %d times at info over three passes, want once; log:\n%s", n, logs.String())
	}
}

// The release is an unsolicited write on a pull request a workflow bot
// opened: the write-back gate's human_merge_path_release policy decides it.
// Refused (the pull request carries no opt-in label and the repository is
// not in full mode), the label stays, no comment is posted, and the refusal
// is logged once for the head, not on every pass.
func TestApplyStageLabels_PromotionInheritHonoursTheWritebackGate(t *testing.T) {
	fx := &promotionInheritFixture{}
	api := promotionInheritStageAPI(t, fx)
	e, clock := promotionInheritStageEngineWithClock(t, api, promotionInheritRules())
	var logs bytes.Buffer
	e.logger = slog.New(slog.NewTextHandler(&logs, nil))
	client, ok := e.transport.(*hgithub.Client)
	if !ok {
		t.Fatalf("transport is %T, want *github.Client", e.transport)
	}
	client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, ""))
	for pass := 1; pass <= 3; pass++ {
		if pass == 3 {
			// Past the recheck interval the release is tried, and refused,
			// again: still logged at info only the first time.
			clock.advance(promotionInheritReleaseRecheck + time.Minute)
		}
		api.reset()
		recordPromotionLabelledPR(e)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := removedPathLabel(api); n != 0 {
			t.Fatalf("pass %d: label removed %d times past the write-back gate (removed=%v); log:\n%s", pass, n, api.removed, logs.String())
		}
		if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
			t.Fatalf("pass %d: lift notice posted past the write-back gate: %q", pass, notices)
		}
	}
	if n := strings.Count(logs.String(), "label release refused by the write-back gate"); n != 1 {
		t.Fatalf("write-back refusal logged %d times at info over three passes, want once; log:\n%s", n, logs.String())
	}
	// The live check ran in the first pass and in the one after the recheck
	// interval, not in the pass between them.
	if n := fx.mergeCommitGets.Load(); n != 2 {
		t.Fatalf("test merge read %d times over three passes, want 2 (the pass inside the recheck interval reads nothing)", n)
	}
}

// A release attempt that ends without removing the label is not repeated
// for the same head on every pass: the reads are made once, a pass inside
// the recheck interval makes none, and a pass after it makes them again.
// Here the live check keeps failing (GitHub's test merge does not carry the
// source tree). A new head is judged at once.
func TestApplyStageLabels_PromotionInheritRechecksAKeptLabelAfterTheInterval(t *testing.T) {
	fx := &promotionInheritFixture{mergeTree: "tree-merged-other"}
	api := promotionInheritStageAPI(t, fx)
	e, clock := promotionInheritStageEngineWithClock(t, api, promotionInheritRules())
	pass := func(what string, wantReads int32) {
		t.Helper()
		api.reset()
		recordPromotionLabelledPR(e)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := removedPathLabel(api); n != 0 || len(humanMergePathLiftNotices(api)) != 0 {
			t.Fatalf("%s: label removed %d times although the test merge differs from the source tree", what, n)
		}
		if n := fx.mergeCommitGets.Load(); n != wantReads {
			t.Fatalf("%s: test merge read %d times in all, want %d", what, n, wantReads)
		}
	}
	pass("first pass", 1)
	clock.advance(promotionInheritReleaseRecheck - time.Minute)
	pass("inside the recheck interval", 1)
	clock.advance(2 * time.Minute)
	pass("after the recheck interval", 2)
	pass("right after that attempt", 2)

	// The mark is the head's: an attempt for another head is not held back.
	if !e.promotionInheritReleaseTried("acme", "widget", 21, promotionInheritHead) {
		t.Fatal("the kept label is not remembered for its head")
	}
	if e.promotionInheritReleaseTried("acme", "widget", 21, strings.Repeat("c", 40)) {
		t.Fatal("a new head is held back by the previous head's attempt")
	}
	if e.promotionInheritReleaseTried("acme", "widget", 21, promotionInheritHead) {
		t.Fatal("the mark outlived the head it was made for")
	}
}

// The pass's own observation is matched against the rules before anything
// is read: a labelled pull request no rule with the option names costs the
// release no call, although the repository has such a rule. The pull
// request GitHub would report is a promotion the release would accept, so
// only the observation (a head branch outside the rule's prefix) stops it;
// the pass reads the pull request exactly as often as with the option off.
func TestApplyStageLabels_PromotionInheritMatchesTheObservationBeforeReading(t *testing.T) {
	gets := map[string]int32{}
	for name, rules := range map[string][]config.PromotionRule{"option off": promotionLaneRules(), "option on": promotionInheritRules()} {
		fx := &promotionInheritFixture{}
		api := promotionInheritStageAPI(t, fx)
		e := promotionInheritStageEngine(t, api, rules)
		pr := promotionLabelledPR()
		pr.Head.Ref = gh.Ptr("feature/not-a-promotion")
		recordPromotionPR(e, pr)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := removedPathLabel(api); n != 0 || len(humanMergePathLiftNotices(api)) != 0 {
			t.Fatalf("%s: label removed %d times for a pull request no rule names; want it kept", name, n)
		}
		gets[name] = fx.prGets.Load()
	}
	if gets["option off"] == 0 {
		t.Fatal("the pass never read the pull request: the fixture does not count reads")
	}
	if gets["option on"] != gets["option off"] {
		t.Fatalf("pull request read %d times with the option on and %d with it off; the release must not read a pull request its observation rules out", gets["option on"], gets["option off"])
	}
}

func TestPromotionObservedRuleInherits(t *testing.T) {
	obs := func(edit func(*stageObservation)) stageObservation {
		o := stageObservation{Owner: "acme", Repo: "widget", Number: 21, BaseRef: "stable", HeadRef: "auto/promote-main-to-stable", HeadRepo: "acme/widget", Author: "github-actions[bot]"}
		if edit != nil {
			edit(&o)
		}
		return o
	}
	for _, tc := range []struct {
		name            string
		rules           []config.PromotionRule
		obs             stageObservation
		inherits, known bool
	}{
		{name: "the rule with the option names it", rules: promotionInheritRules(), obs: obs(nil), inherits: true, known: true},
		{name: "the rule that names it has the option off", rules: promotionLaneRules(), obs: obs(nil), known: true},
		{name: "another base", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.BaseRef = "main" }), known: true},
		{name: "another author", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.Author = "someone" }), known: true},
		{name: "another head ref", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.HeadRef = "feature/x" }), known: true},
		{name: "a fork's head", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.HeadRepo = "fork/widget" }), known: true},
		{name: "no head repository recorded", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.HeadRepo = "" }), inherits: true, known: true},
		{name: "no base recorded", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.BaseRef = "" })},
		{name: "no head ref recorded", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.HeadRef = "" })},
		{name: "no author recorded", rules: promotionInheritRules(), obs: obs(func(o *stageObservation) { o.Author = "" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if inherits, known := promotionObservedRuleInherits(tc.rules, tc.obs); inherits != tc.inherits || known != tc.known {
				t.Fatalf("promotionObservedRuleInherits = %v, %v; want %v, %v", inherits, known, tc.inherits, tc.known)
			}
		})
	}
}
