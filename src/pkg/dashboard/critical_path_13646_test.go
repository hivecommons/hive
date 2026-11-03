package dashboard

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/redmain"
)

// The your-turn critical path (hivecommons/hive#13646): waits_on edges,
// transitive unblocks, root causes, ordering, collapse and the named action.

var cpNow13646 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func cpPR13646(n int, mutate ...func(*github.PullRequest)) github.PullRequest {
	pr := github.PullRequest{Repo: "acme/widget", Number: n, Title: "pr", CreatedAt: cpNow13646.Add(-time.Duration(100-n) * time.Hour), Mergeable: github.MergeableYes, CIStatus: "success"}
	for _, m := range mutate {
		m(&pr)
	}
	return pr
}

func cpDepends13646(refs ...github.PRLinkedIssue) func(*github.PullRequest) {
	return func(pr *github.PullRequest) { pr.DependsOn = refs }
}

func cpIssue13646(n int) github.Issue {
	return github.Issue{Repo: "acme/widget", Number: n, Title: "issue", CreatedAt: cpNow13646.Add(-time.Duration(100-n) * time.Hour)}
}

// cpRepo13646 builds a repository whose listed items are all your turn
// (REVIEW for PRs, DECIDE for issues) unless overridden in verbs.
func cpRepo13646(prs []github.PullRequest, issues []github.Issue, verbs map[string]string) FrontendRepo {
	repo := FrontendRepo{Name: "widget", Full: "acme/widget", Ownership: map[string]Ownership{}}
	for _, pr := range prs {
		repo.OpenPrs = append(repo.OpenPrs, pr)
		repo.Ownership[ownershipKey(ownershipKindPR, pr.Number)] = Ownership{YourTurn: true, Verb: OwnershipVerbReview}
	}
	for _, issue := range issues {
		repo.ActionableIssues = append(repo.ActionableIssues, issue)
		repo.Ownership[ownershipKey(ownershipKindIssue, issue.Number)] = Ownership{YourTurn: true, Verb: OwnershipVerbDecide}
	}
	for key, verb := range verbs {
		if verb == "" {
			delete(repo.Ownership, key)
			continue
		}
		o := repo.Ownership[key]
		o.YourTurn, o.Verb = true, verb
		repo.Ownership[key] = o
	}
	return repo
}

func cpPath13646(t *testing.T, repo FrontendRepo, key string) CriticalPath {
	t.Helper()
	cp, ok := repo.CriticalPath[key]
	if !ok {
		t.Fatalf("no critical path for %s; have %v", key, repo.CriticalPath)
	}
	return cp
}

func TestCriticalPathEdges13646(t *testing.T) {
	prs := []github.PullRequest{
		cpPR13646(10, cpDepends13646(github.PRLinkedIssue{Number: 11, Repo: "acme/widget"}), func(pr *github.PullRequest) {
			pr.LinkedIssues = []github.PRLinkedIssue{{Number: 20}}
		}),
		cpPR13646(11),
		cpPR13646(12, cpDepends13646(github.PRLinkedIssue{Number: 40, Repo: "other/lib"}), func(pr *github.PullRequest) {
			pr.ReferencedIssues = []github.PRLinkedIssue{{Number: 21, Via: github.PRLinkViaFixes}, {Number: 22, Via: github.PRLinkViaRefs}}
		}),
		cpPR13646(13, cpDepends13646(github.PRLinkedIssue{Number: 99, Repo: "widget"})),
		cpPR13646(14, func(pr *github.PullRequest) {
			pr.Protection = &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}
		}),
		cpPR13646(15, func(pr *github.PullRequest) {
			pr.BaseRedChecks = []string{"Python Unit Tests"}
			pr.CIStatus = "failure"
		}),
		cpPR13646(16, func(pr *github.PullRequest) { pr.Mergeable = github.MergeableNo }),
	}
	issues := []github.Issue{cpIssue13646(20), cpIssue13646(21), cpIssue13646(22)}
	repo := cpRepo13646(prs, issues, nil)
	repo.HeldPrs = []any{cpPR13646(17)}
	repo.Ownership[ownershipKey(ownershipKindPR, 17)] = Ownership{YourTurn: true, Verb: OwnershipVerbReview}
	repo.RedMain = &redmain.Fact{Repo: "acme/widget", Check: "Python Unit Tests", Since: cpNow13646.Add(-48 * time.Hour), FixPRs: []redmain.FixPR{{Number: 11}}}
	repos := []FrontendRepo{repo}
	total := computeCriticalPath(repos)
	repo = repos[0]

	cases := []struct {
		key       string
		waits     []WaitsOn
		action    string
		actionRef string
	}{
		{"pr#10", []WaitsOn{{Kind: "pr", Ref: "#11"}}, "rebase", "#11"},
		{"pr#11", []WaitsOn{{Kind: "merge"}}, "merge", ""},
		{"pr#12", []WaitsOn{{Kind: "external", Ref: "other/lib#40"}}, "merge", ""},
		// A tracked-repo dependency that is no longer open is done.
		{"pr#13", []WaitsOn{{Kind: "merge"}}, "merge", ""},
		{"pr#14", []WaitsOn{{Kind: "approval"}}, "approve", ""},
		{"pr#15", []WaitsOn{{Kind: "base-check", Ref: "Python Unit Tests"}, {Kind: "pr", Ref: "#11"}}, "rerun", "#11"},
		{"pr#16", []WaitsOn{{Kind: "merge"}}, "rebase", ""},
		{"pr#17", []WaitsOn{{Kind: "merge"}}, "release-hold", ""},
		{"issue#20", []WaitsOn{{Kind: "pr", Ref: "#10"}}, "merge", "#10"},
		{"issue#21", []WaitsOn{{Kind: "pr", Ref: "#12"}}, "merge", "#12"},
		// A plain reference is not a closing link.
		{"issue#22", []WaitsOn{}, "approve", ""},
	}
	for _, tc := range cases {
		cp := cpPath13646(t, repo, tc.key)
		if !reflect.DeepEqual(cp.WaitsOn, tc.waits) {
			t.Errorf("%s waits_on = %+v, want %+v", tc.key, cp.WaitsOn, tc.waits)
		}
		if cp.Action != tc.action || cp.ActionRef != tc.actionRef {
			t.Errorf("%s action = %q %q, want %q %q", tc.key, cp.Action, cp.ActionRef, tc.action, tc.actionRef)
		}
	}
	// #11 unblocks #10, #15 directly and #20 through #10.
	if got := cpPath13646(t, repo, "pr#11").Unblocks; got != 3 {
		t.Errorf("pr#11 unblocks = %d, want 3", got)
	}
	if got := cpPath13646(t, repo, "pr#10").Unblocks; got != 1 {
		t.Errorf("pr#10 unblocks = %d, want 1", got)
	}
	if cp := cpPath13646(t, repo, "issue#20"); cp.RootCause || cp.Blocker != "#11" {
		t.Errorf("issue#20 root=%v blocker=%q, want blocker #11", cp.RootCause, cp.Blocker)
	}
	// Roots: #11, #12 (waits outside the your-turn set), #13, #14, #16, #17, #22.
	if total.RootPRs != 6 || total.RootIssues != 1 || total.RootCauseCount != 7 || total.PRs != 8 || total.Issues != 3 {
		t.Errorf("totals = %+v", *total)
	}
	if mr := repo.YourTurn.MainRed; mr == nil || mr.Check != "Python Unit Tests" || mr.Fix != 11 || !mr.FixYourTurn || !mr.Since.Equal(cpNow13646.Add(-48*time.Hour)) {
		t.Errorf("main_red = %+v", mr)
	}
}

func TestCriticalPathCrossRepoDependsOn13646(t *testing.T) {
	widget := cpRepo13646([]github.PullRequest{cpPR13646(5, cpDepends13646(github.PRLinkedIssue{Number: 7, Repo: "acme/lib"}))}, nil, nil)
	lib := FrontendRepo{Name: "lib", Full: "acme/lib", Ownership: map[string]Ownership{"pr#7": {YourTurn: true, Verb: OwnershipVerbReview}},
		OpenPrs: []any{github.PullRequest{Repo: "acme/lib", Number: 7, CreatedAt: cpNow13646}}}
	repos := []FrontendRepo{widget, lib}
	total := computeCriticalPath(repos)
	cp := cpPath13646(t, repos[0], "pr#5")
	if !reflect.DeepEqual(cp.WaitsOn, []WaitsOn{{Kind: "pr", Ref: "acme/lib#7"}}) || cp.Blocker != "acme/lib#7" || cp.Action != "rebase" {
		t.Errorf("cross-repo edge = %+v", cp)
	}
	if got := cpPath13646(t, repos[1], "pr#7").Unblocks; got != 1 {
		t.Errorf("acme/lib#7 unblocks = %d, want 1", got)
	}
	if total.RootCauseCount != 1 {
		t.Errorf("root causes = %d, want 1", total.RootCauseCount)
	}
}

func TestCriticalPathCycleGuard13646(t *testing.T) {
	dep := func(n int) func(*github.PullRequest) {
		return cpDepends13646(github.PRLinkedIssue{Number: n, Repo: "acme/widget"})
	}
	repos := []FrontendRepo{cpRepo13646([]github.PullRequest{cpPR13646(1, dep(2)), cpPR13646(2, dep(1)), cpPR13646(3, dep(1))}, nil, nil)}
	total := computeCriticalPath(repos)
	repo := repos[0]
	if got := cpPath13646(t, repo, "pr#1").Unblocks; got != 2 {
		t.Errorf("pr#1 unblocks = %d, want 2 (cycle counted once)", got)
	}
	if got := cpPath13646(t, repo, "pr#3").Unblocks; got != 0 {
		t.Errorf("pr#3 unblocks = %d, want 0", got)
	}
	if !cpPath13646(t, repo, "pr#1").RootCause || cpPath13646(t, repo, "pr#2").RootCause {
		t.Errorf("a closed cycle must surface exactly one root (its smallest item)")
	}
	if b := cpPath13646(t, repo, "pr#3").Blocker; b != "#1" {
		t.Errorf("pr#3 blocker = %q, want #1", b)
	}
	if total.RootCauseCount != 1 {
		t.Errorf("root causes = %d, want 1", total.RootCauseCount)
	}
}

func TestCriticalPathOrderByUnblocksThenAge13646(t *testing.T) {
	dep := func(n int) func(*github.PullRequest) {
		return cpDepends13646(github.PRLinkedIssue{Number: n, Repo: "acme/widget"})
	}
	// Lower numbers are older. #30 unblocks two, #31 one, the rest none.
	prs := []github.PullRequest{cpPR13646(20), cpPR13646(30), cpPR13646(31, dep(30)), cpPR13646(32, dep(31)), cpPR13646(25)}
	repos := []FrontendRepo{cpRepo13646(prs, nil, nil)}
	computeCriticalPath(repos)
	var got []int
	for _, ref := range repos[0].YourTurn.Order {
		got = append(got, ref.Number)
	}
	if want := []int{30, 31, 20, 25, 32}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestCriticalPathCollapseAndRootCauses13646(t *testing.T) {
	prs := []github.PullRequest{cpPR13646(255)}
	for n := 260; n < 268; n++ {
		prs = append(prs, cpPR13646(n, func(pr *github.PullRequest) { pr.BaseRedChecks = []string{"Python Unit Tests"} }))
	}
	issues := []github.Issue{cpIssue13646(1), cpIssue13646(2)}
	repo := cpRepo13646(prs, issues, map[string]string{"issue#2": OwnershipVerbClose})
	repo.Name, repo.Full = "dakota-iso", "acme/dakota-iso"
	repo.RedMain = &redmain.Fact{Check: "Python Unit Tests", Since: cpNow13646, FixPRs: []redmain.FixPR{{Number: 255}}}
	repos := []FrontendRepo{repo}
	total := computeCriticalPath(repos)
	yt := repos[0].YourTurn
	if len(yt.Groups) != 1 || yt.Groups[0].Blocker.Number != 255 || len(yt.Groups[0].Dependents) != 8 {
		t.Fatalf("groups = %+v, want 8 PRs collapsed under #255", yt.Groups)
	}
	if cp := cpPath13646(t, repos[0], "pr#255"); cp.Unblocks != 8 || !cp.RootCause || cp.Action != "merge" {
		t.Errorf("pr#255 = %+v", cp)
	}
	// The CLOSE issue is your turn but, like the inbox's "N your turn",
	// is not counted.
	if yt.PRs != 9 || yt.Issues != 1 || yt.RootPRs != 1 || yt.RootIssues != 1 || yt.RootCauseCount != 2 {
		t.Errorf("repo your turn = %+v", *yt)
	}
	if *total != (FrontendYourTurn{Issues: 1, PRs: 9, RootIssues: 1, RootPRs: 1, RootCauseCount: 2}) {
		t.Errorf("total = %+v", *total)
	}
	if yt.Order[0].Number != 255 {
		t.Errorf("the blocker must sort first, got %+v", yt.Order[0])
	}
}

func TestCriticalPathMayConflictIsSoft13646(t *testing.T) {
	closes := func(n int) func(*github.PullRequest) {
		return func(pr *github.PullRequest) { pr.LinkedIssues = []github.PRLinkedIssue{{Number: n}} }
	}
	paths := func(p ...string) func(*github.PullRequest) {
		return func(pr *github.PullRequest) { pr.ChangedPaths = p }
	}
	prs := []github.PullRequest{
		cpPR13646(30, closes(50)),
		cpPR13646(31, closes(50)),
		cpPR13646(32, paths("a.go", "b.go")),
		cpPR13646(33, paths("b.go")),
		cpPR13646(34, paths("a.go"), cpDepends13646(github.PRLinkedIssue{Number: 32, Repo: "acme/widget"})),
		cpPR13646(35, paths("c.go")),
	}
	repos := []FrontendRepo{cpRepo13646(prs, nil, nil)}
	computeCriticalPath(repos)
	want := map[string][]int{"pr#30": {31}, "pr#31": {30}, "pr#33": {32}, "pr#34": {}, "pr#35": {}}
	for key, w := range want {
		cp := cpPath13646(t, repos[0], key)
		if !reflect.DeepEqual(cp.MayConflictWith, w) {
			t.Errorf("%s may_conflict_with = %v, want %v", key, cp.MayConflictWith, w)
		}
		if key != "pr#34" && !reflect.DeepEqual(cp.WaitsOn, []WaitsOn{{Kind: "merge"}}) {
			t.Errorf("%s: an overlap must never become a waits_on edge: %+v", key, cp.WaitsOn)
		}
	}
	if cp := cpPath13646(t, repos[0], "pr#30"); !cp.RootCause {
		t.Errorf("a soft conflict must not collapse a PR: %+v", cp)
	}
}

func TestCriticalPathOnlyOnYourTurnRows13646(t *testing.T) {
	repo := cpRepo13646([]github.PullRequest{cpPR13646(1), cpPR13646(2, cpDepends13646(github.PRLinkedIssue{Number: 1, Repo: "acme/widget"}))}, nil, map[string]string{"pr#1": ""})
	out := (&Server{}).statusWithOverviewBands(&StatusPayload{HiveID: "h", Repos: []FrontendRepo{repo}}, cpNow13646)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		YourTurn FrontendYourTurn `json:"yourTurn"`
		Repos    []struct {
			OpenPrs  []map[string]any `json:"openPrs"`
			YourTurn RepoYourTurn     `json:"yourTurn"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	rows := decoded.Repos[0].OpenPrs
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if _, ok := rows[0]["waits_on"]; ok {
		t.Errorf("a row that is not your turn must not carry waits_on: %v", rows[0])
	}
	if !strings.Contains(string(raw), `"waits_on":[{"kind":"pr","ref":"#1"}]`) || rows[1]["action"] != "rebase" {
		t.Errorf("your-turn row = %v", rows[1])
	}
	// #1 is not your turn, so #2 waits on nothing in the your-turn set.
	if decoded.YourTurn.RootPRs != 1 || decoded.Repos[0].YourTurn.RootCauseCount != 1 {
		t.Errorf("yourTurn = %+v / %+v", decoded.YourTurn, decoded.Repos[0].YourTurn)
	}
}

func TestTrendHistoryCarriesYourTurnRootCauses13646(t *testing.T) {
	s := &Server{}
	s.appendTrendHistoryAt(&StatusPayload{}, cpNow13646)
	s.appendTrendHistoryAt(&StatusPayload{YourTurn: &FrontendYourTurn{Issues: 5, PRs: 9, RootIssues: 2, RootPRs: 3, RootCauseCount: 5}}, cpNow13646.Add(time.Hour))
	got := s.yourTurnRootHistory()
	if len(got) != 1 || got[0].YourTurnRootIssues != 2 || got[0].YourTurnRootPrs != 3 || got[0].Timestamp != cpNow13646.Add(time.Hour).UnixMilli() {
		t.Errorf("root history = %+v", got)
	}
}

// GitHub's "blocked" and "draft" map to MergeableNo but are not fixed by a
// rebase: only "dirty" and "behind" are (hivecommons/hive#14175).
func TestCriticalActionMergeableState14175(t *testing.T) {
	state := func(s string) func(*github.PullRequest) {
		return func(pr *github.PullRequest) { pr.Mergeable, pr.MergeableState = github.MergeableNo, s }
	}
	red := func(pr *github.PullRequest) { pr.CIStatus = "failure" }
	cases := []struct {
		name   string
		pr     github.PullRequest
		action string
	}{
		{"blocked green CI, empty review decision", cpPR13646(1, state("blocked"), func(pr *github.PullRequest) {
			pr.Protection = &github.ProtectionFacts{}
		}), "approve"},
		{"blocked red CI", cpPR13646(2, state("blocked"), red), "rerun"},
		{"dirty", cpPR13646(3, state("dirty")), "rebase"},
		{"behind", cpPR13646(4, state("behind")), "rebase"},
		{"draft", cpPR13646(5, state("draft")), "merge"},
		{"draft review required", cpPR13646(6, state("draft"), func(pr *github.PullRequest) {
			pr.Protection = &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}
		}), "approve"},
		{"raw-bool fallback, no state", cpPR13646(7, state("")), "rebase"},
		{"unknown state", cpPR13646(8, state("unknown")), "rebase"},
	}
	for _, tc := range cases {
		pr := tc.pr
		action, ref := criticalAction(&cpNode{pr: &pr})
		if action != tc.action || ref != "" {
			t.Errorf("%s: action = %q %q, want %q", tc.name, action, ref, tc.action)
		}
	}
}
