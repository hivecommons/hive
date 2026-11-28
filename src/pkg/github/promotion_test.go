package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// Branch-promotion pull requests (auto_merge.promotions, promotion.go).
// On v6 tip PromotionRuleFor, VerifyPromotion and
// SentinelSweepOptions.PromotionsForRepo do not exist: a promotion pull
// request carrying workflow files is labelled sensitive_path like any other.

// promotionGitHub answers the reads VerifyPromotion and VerifyPromotionMerge
// make: the source branch (tip SHA, tree, protection), the rules for the
// source branch, the compare from the source tip to a head, the target
// branch and a merge commit.
type promotionGitHub struct {
	mu         sync.Mutex
	tip, tree  string
	branchCode int
	// unprotected makes the source branch report "protected": false; rules
	// are then what GET rules/branches/main returns (rulesCode overrides).
	unprotected bool
	rules       []map[string]any
	rulesCode   int
	// protection is GET branches/main/protection (nil: pull request reviews
	// required, force pushes off); protectionCode overrides the status.
	protection     map[string]any
	protectionCode int
	// compares maps "<base>...<head>" to the compare payload.
	compares    map[string]map[string]any
	branchHits  int
	rulesHits   int
	compareHits int
	// targetTip is GET branches/stable; commits maps a SHA to GET
	// git/commits/<sha>.
	targetTip string
	commits   map[string]map[string]any
	// movedHead, when set, is the head GET pulls/12 reports: a push that
	// landed after the sentinel listed the pull request's files.
	movedHead string
}

func (p *promotionGitHub) handle(w http.ResponseWriter, r *http.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/main":
		p.branchHits++
		if p.branchCode != 0 {
			w.WriteHeader(p.branchCode)
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":      "main",
			"protected": !p.unprotected,
			"commit":    map[string]any{"sha": p.tip, "commit": map[string]any{"tree": map[string]any{"sha": p.tree}}},
		})
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/main/protection":
		if p.protectionCode != 0 {
			w.WriteHeader(p.protectionCode)
			return true
		}
		body := p.protection
		if body == nil {
			body = map[string]any{"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0}, "allow_force_pushes": map[string]any{"enabled": false}}
		}
		_ = json.NewEncoder(w).Encode(body)
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rules/branches/main":
		p.rulesHits++
		if p.rulesCode != 0 {
			w.WriteHeader(p.rulesCode)
			return true
		}
		rules := p.rules
		if rules == nil {
			rules = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(rules)
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/12" && p.movedHead != "":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 12, "state": "open", "user": map[string]string{"login": "github-actions[bot]"},
			"head": map[string]any{"sha": p.movedHead, "ref": "auto/promote-main-to-stable", "repo": map[string]any{"full_name": "o/r"}},
			"base": map[string]any{"ref": "stable", "repo": map[string]any{"full_name": "o/r"}},
		})
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/stable":
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "stable", "commit": map[string]any{"sha": p.targetTip}})
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/commits/"):
		if body, ok := p.commits[strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/commits/")]; ok {
			_ = json.NewEncoder(w).Encode(body)
			return true
		}
		w.WriteHeader(http.StatusNotFound)
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/"):
		p.compareHits++
		key := strings.TrimPrefix(r.URL.Path, "/repos/o/r/compare/")
		if body, ok := p.compares[key]; ok {
			_ = json.NewEncoder(w).Encode(body)
			return true
		}
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	return false
}

func promotionCompare(status string, ahead int, mergeBase, mergeBaseTree string) map[string]any {
	return map[string]any{
		"status": status, "ahead_by": ahead,
		"merge_base_commit": map[string]any{"sha": mergeBase, "commit": map[string]any{"tree": map[string]any{"sha": mergeBaseTree}}},
	}
}

func promotionRules() []config.PromotionRule {
	return []config.PromotionRule{{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}}
}

// promotionPR is the pull request a promote-main-to-stable workflow opens.
func promotionPR(head string) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(12),
		User:   &gh.User{Login: gh.Ptr("github-actions[bot]"), Type: gh.Ptr("Bot")},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr(head), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("o/r")}},
		Base:   &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("o/r")}},
	}
}

func promotionClient(t *testing.T, p *promotionGitHub) *gh.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.handle(w, r) {
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	return c
}

func TestPromotionRuleForMatchesOnlyTheNamedPullRequest(t *testing.T) {
	rules := promotionRules()
	if _, ok := PromotionRuleFor(rules, "o", "r", promotionPR("h1")); !ok {
		t.Fatal("the named promotion pull request does not match its rule")
	}
	for name, mutate := range map[string]func(*gh.PullRequest){
		"author not listed":     func(pr *gh.PullRequest) { pr.User.Login = gh.Ptr("mallory") },
		"author is a prefix":    func(pr *gh.PullRequest) { pr.User.Login = gh.Ptr("github-actions") },
		"head ref prefix":       func(pr *gh.PullRequest) { pr.Head.Ref = gh.Ptr("feature/promote-main-to-stable") },
		"base is not target":    func(pr *gh.PullRequest) { pr.Base.Ref = gh.Ptr("main") },
		"head from a fork":      func(pr *gh.PullRequest) { pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/r")} },
		"head repo not known":   func(pr *gh.PullRequest) { pr.Head.Repo = nil },
		"no user on the record": func(pr *gh.PullRequest) { pr.User = nil },
	} {
		t.Run(name, func(t *testing.T) {
			pr := promotionPR("h1")
			mutate(pr)
			if _, ok := PromotionRuleFor(rules, "o", "r", pr); ok {
				t.Fatal("matched")
			}
		})
	}
	if _, ok := PromotionRuleFor(nil, "o", "r", promotionPR("h1")); ok {
		t.Fatal("matched with no rules")
	}
}

// promotionCommit is a git/commits payload: a tree and its parents.
func promotionCommit(tree string, parents ...string) map[string]any {
	ps := []map[string]any{}
	for _, p := range parents {
		ps = append(ps, map[string]any{"sha": p})
	}
	return map[string]any{"tree": map[string]any{"sha": tree}, "parents": ps}
}

func TestVerifyPromotion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		head        string
		gh          *promotionGitHub
		matched     bool
		shape       string
		reason      string
		wantErr     bool
		compareHits int
	}{
		{
			// The workflow pushed the source tip. One content read.
			name: "head is the source tip", head: "tip1",
			gh:      &promotionGitHub{tip: "tip1", tree: "tree1"},
			matched: true, shape: PromotionShapeSourceTip,
		},
		{
			name: "head is an ancestor with the same tree", head: "old1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base1",
				commits:  map[string]map[string]any{"old1": promotionCommit("tree1", "older")},
				compares: map[string]map[string]any{"tip1...old1": promotionCompare("behind", 0, "old1", "tree1")}},
			matched: true, shape: PromotionShapeSourceTip, compareHits: 1,
		},
		{
			// `git commit-tree <main tree> -p origin/stable`: one commit on
			// the live target tip with the source tree. The compare would
			// say diverged; it is not asked.
			name: "commit-tree onto the live target tip", head: "ct1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base1",
				commits: map[string]map[string]any{"ct1": promotionCommit("tree1", "base1")}},
			matched: true, shape: PromotionShapeTargetChild,
		},
		{
			name: "commit-tree onto an old target tip", head: "ct1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base2",
				commits:  map[string]map[string]any{"ct1": promotionCommit("tree1", "base1")},
				compares: map[string]map[string]any{"tip1...ct1": promotionCompare("diverged", 1, "fork", "treeF")}},
			reason: PromotionReasonHeadShape, compareHits: 1,
		},
		{
			// Two commits on the target, the last with the source tree.
			name: "two-commit chain on the target with the source tree", head: "c2",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base1",
				commits:  map[string]map[string]any{"c2": promotionCommit("tree1", "c1"), "c1": promotionCommit("treeX", "base1")},
				compares: map[string]map[string]any{"tip1...c2": promotionCompare("diverged", 2, "fork", "treeF")}},
			reason: PromotionReasonHeadShape, compareHits: 1,
		},
		{
			// The source moved one file on since the head was pushed.
			name: "head tree differs from the source tip by one file", head: "old1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1",
				commits: map[string]map[string]any{"old1": promotionCommit("tree0", "older")}},
			reason: PromotionReasonTreeMismatch,
		},
		{
			// A merge commit carrying the source tree and a commit the source
			// lacks: two parents, so not the target-child shape either.
			name: "head brings a commit the source lacks", head: "extra1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1",
				commits:  map[string]map[string]any{"extra1": promotionCommit("tree1", "tip1", "side")},
				compares: map[string]map[string]any{"tip1...extra1": promotionCompare("ahead", 1, "tip1", "tree1")}},
			reason: PromotionReasonNotOnSource, compareHits: 1,
		},
		{
			name: "source branch unreadable", head: "tip1",
			gh:     &promotionGitHub{branchCode: http.StatusInternalServerError},
			reason: PromotionReasonReadFailed, wantErr: true,
		},
		{
			name: "head commit unreadable", head: "old1",
			gh:     &promotionGitHub{tip: "tip1", tree: "tree1"},
			reason: PromotionReasonReadFailed, wantErr: true,
		},
		{
			name: "compare unreadable", head: "old1",
			gh: &promotionGitHub{tip: "tip1", tree: "tree1",
				commits: map[string]map[string]any{"old1": promotionCommit("tree1", "a", "b")}},
			reason: PromotionReasonReadFailed, wantErr: true, compareHits: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.gh
			client := promotionClient(t, p)
			check, err := VerifyPromotion(context.Background(), client, "o", "r", promotionPR(tc.head), promotionRules()[0])
			if check.Matched != tc.matched || check.Reason != tc.reason || (err != nil) != tc.wantErr || check.Shape != tc.shape {
				t.Fatalf("check = %+v, err = %v; want matched=%v shape=%q reason=%q err=%v", check, err, tc.matched, tc.shape, tc.reason, tc.wantErr)
			}
			if check.Matched && err != nil {
				t.Fatal("a check with an error matched")
			}
			if p.branchHits != 1 || p.compareHits != tc.compareHits {
				t.Fatalf("API calls: source branch %d compare %d, want 1 and %d", p.branchHits, p.compareHits, tc.compareHits)
			}
			if tc.matched && (check.SourceSHA != "tip1" || check.TreeSHA != "tree1" || !strings.Contains(check.Summary(), "tree == main at tip1")) {
				t.Fatalf("matched check does not carry its evidence: %+v %q", check, check.Summary())
			}
		})
	}
	// No head commit is never a match and costs nothing.
	p := &promotionGitHub{tip: "tip1", tree: "tree1"}
	pr := promotionPR("")
	if check, _ := VerifyPromotion(context.Background(), promotionClient(t, p), "o", "r", pr, promotionRules()[0]); check.Matched || p.branchHits != 0 {
		t.Fatalf("no head: %+v, branch reads %d", check, p.branchHits)
	}
}

// The source must take changes only through a pull request: the target
// inherits exactly its review guarantee. Classic protection counts when its
// settings require pull request reviews (zero approvals included) and do
// not allow force pushes; otherwise, or when those settings cannot be read
// (403 without administration read), only an active ruleset pull_request
// rule does. An update rule, status checks alone, or any read error of the
// rules is no match.
func TestVerifyPromotionRequiresPullRequestOnlySource(t *testing.T) {
	prRule := map[string]any{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "o/r", "ruleset_id": 7, "parameters": map[string]any{"required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false, "require_code_owner_review": false, "require_last_push_approval": false, "required_review_thread_resolution": false}}
	for _, tc := range []struct {
		name      string
		gh        *promotionGitHub
		matched   bool
		rulesHits int
		wantErr   bool
	}{
		{name: "classic protection requiring pull request reviews", gh: &promotionGitHub{tip: "tip1", tree: "tree1"}, matched: true},
		{name: "classic protection without pull request reviews", gh: &promotionGitHub{tip: "tip1", tree: "tree1", protection: map[string]any{"enabled": true}}, rulesHits: 1},
		{name: "classic protection allowing force pushes", gh: &promotionGitHub{tip: "tip1", tree: "tree1", protection: map[string]any{"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0}, "allow_force_pushes": map[string]any{"enabled": true}}}, rulesHits: 1},
		{name: "classic settings unreadable, ruleset requires a pull request", gh: &promotionGitHub{tip: "tip1", tree: "tree1", protectionCode: http.StatusForbidden, rules: []map[string]any{prRule}}, matched: true, rulesHits: 1},
		{name: "classic settings unreadable, no ruleset", gh: &promotionGitHub{tip: "tip1", tree: "tree1", protectionCode: http.StatusForbidden}, rulesHits: 1},
		{name: "unprotected, no rules", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true}, rulesHits: 1},
		{name: "unprotected, only a status-check rule", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true, rules: []map[string]any{
			{"type": "required_status_checks", "ruleset_source_type": "Repository", "ruleset_source": "o/r", "ruleset_id": 3, "parameters": map[string]any{"required_status_checks": []any{}, "strict_required_status_checks_policy": false}},
		}}, rulesHits: 1},
		{name: "ruleset requiring a pull request, zero approvals", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true, rules: []map[string]any{prRule}}, matched: true, rulesHits: 1},
		{name: "ruleset update rule alone", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true, rules: []map[string]any{
			{"type": "update", "ruleset_source_type": "Repository", "ruleset_source": "o/r", "ruleset_id": 8},
		}}, rulesHits: 1},
		{name: "rules unreadable", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true, rulesCode: http.StatusForbidden}, rulesHits: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check, err := VerifyPromotion(context.Background(), promotionClient(t, tc.gh), "o", "r", promotionPR("tip1"), promotionRules()[0])
			if check.Matched != tc.matched || (err != nil) != tc.wantErr || tc.gh.rulesHits != tc.rulesHits {
				t.Fatalf("check = %+v, err = %v, rules reads %d; want matched=%v err=%v rules reads %d", check, err, tc.gh.rulesHits, tc.matched, tc.wantErr, tc.rulesHits)
			}
			if !tc.matched && check.Reason != PromotionReasonSourceUnprotected {
				t.Fatalf("reason = %q, want %s", check.Reason, PromotionReasonSourceUnprotected)
			}
		})
	}
}

func promotionMergeCommit(tree string, parents ...string) map[string]any {
	ps := []map[string]any{}
	for _, p := range parents {
		ps = append(ps, map[string]any{"sha": p})
	}
	return map[string]any{"sha": "m1", "tree": map[string]any{"sha": tree}, "parents": ps}
}

// The merge result must equal the source too. On a target that diverged
// from the source (every squash promote), GitHub's test merge is a
// three-way merge: a change the source reverted can survive on the target,
// so a head equal to the source tip is not enough.
func TestVerifyPromotionMerge(t *testing.T) {
	matched := PromotionCheck{Matched: true, Rule: promotionRules()[0], HeadSHA: "tip1", SourceSHA: "tip1", TreeSHA: "tree1"}
	withMerge := func(mergeable *bool, sha string) *gh.PullRequest {
		pr := promotionPR("tip1")
		pr.Mergeable = mergeable
		if sha != "" {
			pr.MergeCommitSHA = gh.Ptr(sha)
		}
		return pr
	}
	for _, tc := range []struct {
		name    string
		pr      *gh.PullRequest
		commits map[string]map[string]any
		matched bool
		reason  string
		wantErr bool
	}{
		{name: "test merge equals the source tip", pr: withMerge(gh.Ptr(true), "m1"), commits: map[string]map[string]any{"m1": promotionMergeCommit("tree1", "base1", "tip1")}, matched: true},
		// main reverted a file stable still carries: the head equals main,
		// the merge keeps stable's copy.
		{name: "revert on the source does not carry over", pr: withMerge(gh.Ptr(true), "m1"), commits: map[string]map[string]any{"m1": promotionMergeCommit("tree-with-reverted-file", "base1", "tip1")}, reason: PromotionReasonMergeTreeMismatch},
		{name: "test merge of another head", pr: withMerge(gh.Ptr(true), "m1"), commits: map[string]map[string]any{"m1": promotionMergeCommit("tree1", "base1", "other")}, reason: PromotionReasonMergeTreeMismatch},
		{name: "test merge built on an older target tip", pr: withMerge(gh.Ptr(true), "m1"), commits: map[string]map[string]any{"m1": promotionMergeCommit("tree1", "base0", "tip1")}, reason: PromotionReasonMergeUnknown},
		{name: "mergeable unknown", pr: withMerge(nil, "m1"), reason: PromotionReasonMergeUnknown},
		{name: "not mergeable", pr: withMerge(gh.Ptr(false), "m1"), reason: PromotionReasonMergeTreeMismatch},
		{name: "no test merge commit", pr: withMerge(gh.Ptr(true), ""), reason: PromotionReasonMergeUnknown},
		{name: "test merge unreadable", pr: withMerge(gh.Ptr(true), "m1"), reason: PromotionReasonMergeUnknown, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base1", commits: tc.commits}
			check, err := VerifyPromotionMerge(context.Background(), promotionClient(t, p), "o", "r", tc.pr, matched)
			if check.Matched != tc.matched || check.Reason != tc.reason || (err != nil) != tc.wantErr {
				t.Fatalf("check = %+v, err = %v; want matched=%v reason=%q err=%v", check, err, tc.matched, tc.reason, tc.wantErr)
			}
			if tc.matched && (check.MergeSHA != "m1" || check.MergeTreeSHA != "tree1" || check.TargetSHA != "base1") {
				t.Fatalf("matched merge check does not carry its evidence: %+v", check)
			}
		})
	}
	// A check that did not match is never turned into a match.
	if check, _ := VerifyPromotionMerge(context.Background(), promotionClient(t, &promotionGitHub{}), "o", "r", withMerge(gh.Ptr(true), "m1"), PromotionCheck{}); check.Matched {
		t.Fatal("an unmatched check matched")
	}
}

// promotionSentinelFixture is a sentinel fixture whose PR 12 is the
// promote-main-to-stable pull request, carrying a workflow file main already
// has (sensitive_path), behind a proxy that also answers the branch and
// compare reads.
func promotionSentinelFixture(t *testing.T, head string, files []map[string]any, p *promotionGitHub) (*Client, *sentinelFixture) {
	t.Helper()
	pr := map[string]any{
		"number": 12, "title": "Promote main to stable", "state": "open",
		"user":   map[string]string{"login": "github-actions[bot]", "type": "Bot"},
		"head":   map[string]any{"sha": head, "ref": "auto/promote-main-to-stable", "repo": map[string]any{"full_name": "o/r"}},
		"base":   map[string]any{"ref": "stable", "repo": map[string]any{"full_name": "o/r"}},
		"labels": []map[string]string{{"name": "release/ready"}}, "changed_files": len(files),
	}
	f := &sentinelFixture{prs: []map[string]any{pr}, files: map[int][]map[string]any{12: files}}
	inner := newSentinelServer(t, f)
	t.Cleanup(inner.Close)
	target, _ := url.Parse(inner.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.handle(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(outer.Close)
	return newTestClient(t, outer, "o", []string{"o/r"}), f
}

func promotionWorkflowFile() map[string]any {
	return map[string]any{
		"filename": ".github/workflows/build.yml", "status": "modified", "additions": 1, "deletions": 1,
		"patch": "@@ -1 +1 @@\n-  go-version: 1.22\n+  go-version: 1.23\n",
	}
}

func promotionSentinelOpts(rules []config.PromotionRule) SentinelSweepOptions {
	opts := defaultSentinelOpts(nil)
	if rules != nil {
		opts.PromotionsForRepo = func(repo string) []config.PromotionRule {
			if repo == "o/r" {
				return rules
			}
			return nil
		}
	}
	return opts
}

// A tree-equal promote from the listed author and ref carries only content
// main already has: no sentinel hold, and the decision is audited with the
// head, source tip and tree.
func TestSentinelPromotionTreeEqualIsNotHeld(t *testing.T) {
	p := &promotionGitHub{tip: "tip1", tree: "tree1"}
	c, f := promotionSentinelFixture(t, "tip1", []map[string]any{promotionWorkflowFile()}, p)
	var audits []AuditRecord
	c.SetAttributionHooks(AttributionHooks{AuditRecord: func(r AuditRecord) {
		if r.Action == AuditActionPromotionDecision {
			audits = append(audits, r)
		}
	}})
	res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 0 || len(res.Notified) != 0 || len(f.labels[12]) != 0 || len(f.comments[12]) != 0 {
		t.Fatalf("tree-equal promotion was held: res=%+v labels=%v comments=%v", res, f.labels[12], f.comments[12])
	}
	if p.branchHits != 1 {
		t.Fatalf("branch reads = %d, want one live read", p.branchHits)
	}
	if len(audits) != 1 || audits[0].Repo != "o/r" || audits[0].Target != 12 {
		t.Fatalf("audits = %+v, want one promotion_decision for o/r#12", audits)
	}
	for _, want := range []string{"decision=sentinel-exempt", "head=tip1", "source=main", "source_sha=tip1", "tree=tree1"} {
		if !strings.Contains(audits[0].Detail, want) {
			t.Fatalf("audit detail %q missing %q", audits[0].Detail, want)
		}
	}
}

// Every way the content check can fail leaves the sentinel exactly as it is
// today: the workflow file is flagged and labelled.
func TestSentinelPromotionMismatchTakesTodaysPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  string
		gh    *promotionGitHub
		rules []config.PromotionRule
	}{
		{name: "feature off", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1"}},
		{name: "repository not listed", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1"}, rules: []config.PromotionRule{}},
		{name: "head tree differs from the source tip by one file", head: "old1", gh: &promotionGitHub{tip: "tip1", tree: "tree1",
			commits: map[string]map[string]any{"old1": promotionCommit("tree0", "older")}}, rules: promotionRules()},
		{name: "a commit not on the source branch", head: "extra1", gh: &promotionGitHub{tip: "tip1", tree: "tree1",
			commits:  map[string]map[string]any{"extra1": promotionCommit("tree1", "tip1", "side")},
			compares: map[string]map[string]any{"tip1...extra1": promotionCompare("ahead", 1, "tip1", "tree1")}}, rules: promotionRules()},
		{name: "author not in the rule", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1"}, rules: []config.PromotionRule{{Source: "main", Target: "stable", Authors: []string{"release-bot[bot]"}, HeadRefPrefix: "auto/promote-"}}},
		{name: "head ref prefix mismatch", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1"}, rules: []config.PromotionRule{{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "release/"}}},
		{name: "source branch unreadable", head: "tip1", gh: &promotionGitHub{branchCode: http.StatusBadGateway}, rules: promotionRules()},
		{name: "source branch unprotected", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1", unprotected: true}, rules: promotionRules()},
		// The files were listed for tip1, then a push moved the head.
		{name: "head moved after the files were listed", head: "tip1", gh: &promotionGitHub{tip: "tip1", tree: "tree1", movedHead: "tip9"}, rules: promotionRules()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.gh
			c, f := promotionSentinelFixture(t, tc.head, []map[string]any{promotionWorkflowFile()}, p)
			res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(tc.rules))
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(res.Flagged) != 1 || len(f.labels[12]) != 1 || f.labels[12][0] != "sentinel-alert" {
				t.Fatalf("want today's sentinel hold: res=%+v labels=%v", res, f.labels[12])
			}
			if rules := res.Flagged[0].Rules(); len(rules) != 1 || rules[0] != sentinel.RuleSensitivePath {
				t.Fatalf("rules = %v, want sensitive_path", rules)
			}
		})
	}
}

// owner_self_nomination reads this pull request's own author, so a
// tree-equal promotion that adds its author to OWNERS is still flagged, for
// that rule alone.
func TestSentinelPromotionKeepsOwnerSelfNomination(t *testing.T) {
	p := &promotionGitHub{tip: "tip1", tree: "tree1"}
	owners := map[string]any{
		"filename": "OWNERS", "status": "modified", "additions": 1, "deletions": 0,
		"patch": "@@ -1,2 +1,3 @@\n approvers:\n   - maintainer\n+  - github-actions[bot]\n",
	}
	c, f := promotionSentinelFixture(t, "tip1", []map[string]any{owners, promotionWorkflowFile()}, p)
	res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || len(f.labels[12]) != 1 {
		t.Fatalf("self-nomination not held: res=%+v labels=%v", res, f.labels[12])
	}
	if rules := res.Flagged[0].Rules(); len(rules) != 1 || rules[0] != sentinel.RuleOwnerSelfNomination {
		t.Fatalf("rules = %v, want only owner_self_nomination", rules)
	}
}

// Only sensitive_path is set aside: a rule that says what the content does
// still flags a tree-equal promotion, because the source branch may carry it
// without the sentinel ever having judged it.
func TestSentinelPromotionStillFlagsPermissionEscalation(t *testing.T) {
	p := &promotionGitHub{tip: "tip1", tree: "tree1"}
	escalation := map[string]any{
		"filename": ".github/workflows/release.yml", "status": "modified", "additions": 1, "deletions": 0,
		"patch": "@@ -1,2 +1,3 @@\n permissions:\n+  contents: write\n",
	}
	c, f := promotionSentinelFixture(t, "tip1", []map[string]any{escalation}, p)
	res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || len(f.labels[12]) != 1 {
		t.Fatalf("permission escalation on a promotion was not held: res=%+v labels=%v", res, f.labels[12])
	}
	if rules := res.Flagged[0].Rules(); len(rules) != 1 || rules[0] != sentinel.RulePermissionEscalation {
		t.Fatalf("rules = %v, want only permission_escalation (sensitive_path set aside)", rules)
	}
}

// An exempted head is checked again on every pass. When the source branch
// has only moved on (the tip the head matched is still on it), the head is
// an earlier reviewed state of the source and stays exempt, so a race never
// leaves a flag only a person can clear. When the source was rewritten (that
// tip is gone from it), the head is flagged.
func TestSentinelPromotionRechecksExemptedHead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmp     map[string]any
		flagged bool
	}{
		{name: "source moved on", cmp: map[string]any{"status": "ahead", "ahead_by": 3, "behind_by": 0}},
		{name: "source rewritten", cmp: map[string]any{"status": "diverged", "ahead_by": 3, "behind_by": 1}, flagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &promotionGitHub{tip: "tip1", tree: "tree1"}
			c, f := promotionSentinelFixture(t, "tip1", []map[string]any{promotionWorkflowFile()}, p)
			if res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules())); err != nil || len(res.Flagged) != 0 {
				t.Fatalf("first pass: res=%+v err=%v, want the tree-equal promotion exempt", res, err)
			}
			p.mu.Lock()
			p.tip, p.tree = "tip2", "tree2"
			p.commits = map[string]map[string]any{"tip1": promotionCommit("tree1", "tip0")}
			p.compares = map[string]map[string]any{"tip1...tip2": tc.cmp}
			p.mu.Unlock()
			res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if got := len(res.Flagged) == 1 && len(f.labels[12]) == 1; got != tc.flagged {
				t.Fatalf("flagged = %v, want %v: res=%+v labels=%v", got, tc.flagged, res, f.labels[12])
			}
		})
	}
}

// The sentinel applies the same two head shapes: a `git commit-tree`
// promote (one commit on the live target tip carrying the source tree) is
// exempt from sensitive_path; the same commit on an older target tip is not.
func TestSentinelPromotionCommitTreeShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		targetTip string
		flagged   bool
	}{
		{name: "on the live target tip", targetTip: "base1"},
		{name: "on an older target tip", targetTip: "base2", flagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: tc.targetTip,
				commits:  map[string]map[string]any{"ct1": promotionCommit("tree1", "base1")},
				compares: map[string]map[string]any{"tip1...ct1": promotionCompare("diverged", 1, "fork", "treeF")}}
			c, f := promotionSentinelFixture(t, "ct1", []map[string]any{promotionWorkflowFile()}, p)
			res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if got := len(res.Flagged) == 1 && len(f.labels[12]) == 1; got != tc.flagged {
				t.Fatalf("flagged = %v, want %v: res=%+v labels=%v", got, tc.flagged, res, f.labels[12])
			}
		})
	}
}

// A hotfix lands on the target while a commit-tree promotion is open: the
// head is no longer on the target tip, but it still carries exactly the
// unmoved, protected source tree, so it stays exempt. A head whose tree is
// not the source's is still flagged.
func TestSentinelPromotionTargetMovedUnderCommitTreeHead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headTree string
		flagged  bool
	}{
		{name: "head still carries the source tree", headTree: "tree1"},
		{name: "head carries another tree", headTree: "treeX", flagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &promotionGitHub{tip: "tip1", tree: "tree1", targetTip: "base1",
				commits:  map[string]map[string]any{"ct1": promotionCommit("tree1", "base1")},
				compares: map[string]map[string]any{"tip1...ct1": promotionCompare("diverged", 1, "fork", "treeF")}}
			c, f := promotionSentinelFixture(t, "ct1", []map[string]any{promotionWorkflowFile()}, p)
			if res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules())); err != nil || len(res.Flagged) != 0 {
				t.Fatalf("first pass: res=%+v err=%v, want the commit-tree promotion exempt", res, err)
			}
			p.mu.Lock()
			p.targetTip = "hotfix1"
			p.commits = map[string]map[string]any{"ct1": promotionCommit(tc.headTree, "base1")}
			p.mu.Unlock()
			res, err := c.SweepSentinel(context.Background(), promotionSentinelOpts(promotionRules()))
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if got := len(res.Flagged) == 1 && len(f.labels[12]) == 1; got != tc.flagged {
				t.Fatalf("flagged = %v, want %v: res=%+v labels=%v", got, tc.flagged, res, f.labels[12])
			}
		})
	}
}
