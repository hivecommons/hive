package github

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/review"
)

// fakeBaseRefreshRepos serves one PR's commit graph for the base-refresh
// check: the commits by SHA and the three-dot compare of base...head.
type fakeBaseRefreshRepos struct {
	mu       sync.Mutex
	commits  map[string]*gh.RepositoryCommit
	compares map[string]*gh.CommitsComparison
	calls    int
}

func (f *fakeBaseRefreshRepos) GetCommit(_ context.Context, _, _, sha string, _ *gh.ListOptions) (*gh.RepositoryCommit, *gh.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if c, ok := f.commits[sha]; ok {
		return c, nil, nil
	}
	return nil, nil, errors.New("not found")
}

func (f *fakeBaseRefreshRepos) CompareCommits(_ context.Context, _, _, base, head string, _ *gh.ListOptions) (*gh.CommitsComparison, *gh.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if c, ok := f.compares[base+"..."+head]; ok {
		return c, nil, nil
	}
	return nil, nil, errors.New("not found")
}

func refreshCommit(sha, login string, parents ...string) *gh.RepositoryCommit {
	c := &gh.RepositoryCommit{SHA: gh.Ptr(sha), Author: &gh.User{Login: gh.Ptr(login)}, Committer: &gh.User{Login: gh.Ptr("web-flow")},
		Commit: &gh.Commit{Verification: &gh.SignatureVerification{Verified: gh.Ptr(true)}}}
	for _, p := range parents {
		c.Parents = append(c.Parents, &gh.Commit{SHA: gh.Ptr(p)})
	}
	return c
}

func refreshCompare(mergeBase string, files ...*gh.CommitFile) *gh.CommitsComparison {
	return &gh.CommitsComparison{MergeBaseCommit: &gh.RepositoryCommit{SHA: gh.Ptr(mergeBase)}, Files: files}
}

func refreshFile(name, blob, patch string) *gh.CommitFile {
	return &gh.CommitFile{Filename: gh.Ptr(name), Status: gh.Ptr("modified"), SHA: gh.Ptr(blob), Additions: gh.Ptr(3), Deletions: gh.Ptr(1), Changes: gh.Ptr(4), Patch: gh.Ptr(patch)}
}

type captureHandler struct {
	slog.Handler
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// hivecommons/hive#15649: the operator approved #15649 on head A; the
// require_current_base gate then merged v5 into the branch (a merge commit
// by the App that left the PR's diff untouched) and the reviewer re-asked
// the same question on head B. The decision follows the PR onto B — once —
// but any other head change still supersedes it.
func TestDecisionCarrierCarriesOverPureBaseRefresh(t *testing.T) {
	const oldHead, newHead, baseTip, app = "aaaa", "72246f98", "b45e", "kubestellar-hive[bot]"
	same := []*gh.CommitFile{refreshFile("src/pkg/proxy/private.go", "blob1", "@@ -1 +1 @@"), refreshFile("src/docs/hive-merge.md", "blob2", "@@ -2 +2 @@")}
	api := &fakeBaseRefreshRepos{
		commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, baseTip)},
		compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same[1], same[0])},
	}
	store := review.NewHumanChangeRequests("")
	if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 15649, HeadSHA: oldHead, Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
		t.Fatal(err)
	}
	logs := &captureHandler{}
	carrier := &DecisionCarrier{Store: store, API: api, AppBotLogin: app, Logger: slog.New(logs), Checker: &BaseRefreshChecker{}}

	got, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 15649, "v5", newHead)
	if !ok || !got.IsApproval() || got.HeadSHA != newHead || got.CarriedFromHead != oldHead || got.Actor != "clubanderson" || got.SourceID != 1 {
		t.Fatalf("carried = %+v ok=%v", got, ok)
	}
	if again, ok := store.For("hivecommons/hive", 15649, newHead); !ok || again.CarriedFromHead != oldHead {
		t.Fatalf("store does not hold the carried decision: %+v ok=%v", again, ok)
	}
	if _, ok := store.For("hivecommons/hive", 15649, oldHead); ok {
		t.Fatal("the old head's record must be superseded by the carried one")
	}
	calls := api.calls
	if _, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 15649, "v5", newHead); !ok {
		t.Fatal("second lookup must find the carried decision")
	}
	if api.calls != calls {
		t.Fatalf("second lookup hit GitHub: %d calls, want %d", api.calls, calls)
	}
	if n := logs.count(DecisionCarriedOverBaseRefreshMessage); n != 1 {
		t.Fatalf("carry logged %d times, want once", n)
	}
}

func TestDecisionCarrierRefusesOtherHeadChanges(t *testing.T) {
	const oldHead, newHead, baseTip, content, app = "aaaa", "bbbb", "b45e", "c0de", "kubestellar-hive[bot]"
	same := []*gh.CommitFile{refreshFile("src/pkg/proxy/private.go", "blob1", "@@ -1 +1 @@")}
	changed := []*gh.CommitFile{refreshFile("src/pkg/proxy/private.go", "blob9", "@@ -1 +1,2 @@")}
	for _, tc := range []struct {
		name     string
		source   string
		commits  map[string]*gh.RepositoryCommit
		compares map[string]*gh.CommitsComparison
	}{
		{
			name:     "content commit pushed, then base merged",
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, content, baseTip)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, changed...)},
		},
		{
			name:     "base merged but the diff changed with it",
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, baseTip)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, changed...)},
		},
		{
			name:     "merge commit not by the App",
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, "mendezr", oldHead, baseTip)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same...)},
		},
		{
			name:     "other parent is not on the base branch",
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, content)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same...)},
		},
		{
			name:     "single-parent push",
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare("0ld", same...)},
		},
		{
			name:     "GitHub review approvals never travel",
			source:   review.HumanChangeRequestSourceReview,
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, baseTip)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same...)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeBaseRefreshRepos{commits: tc.commits, compares: tc.compares}
			store := review.NewHumanChangeRequests("")
			source := tc.source
			if source == "" {
				source = review.HumanChangeRequestSourceDecision
			}
			if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 15649, HeadSHA: oldHead, Source: source, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
				t.Fatal(err)
			}
			logs := &captureHandler{}
			carrier := &DecisionCarrier{Store: store, API: api, AppBotLogin: app, Logger: slog.New(logs), Checker: &BaseRefreshChecker{}}
			if got, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 15649, "v5", newHead); ok {
				t.Fatalf("decision carried onto a head that is not a pure base refresh: %+v", got)
			}
			if _, ok := store.For("hivecommons/hive", 15649, oldHead); !ok {
				t.Fatal("the old head's record must stand untouched")
			}
			if n := logs.count(DecisionCarriedOverBaseRefreshMessage); n != 0 {
				t.Fatalf("carry logged %d times, want none", n)
			}
			calls := api.calls
			carrier.Carry(context.Background(), "hivecommons", "hive", 15649, "v5", newHead)
			if api.calls != calls {
				t.Fatalf("negative answer not memoised: %d calls, want %d", api.calls, calls)
			}
		})
	}
}

// Without a base branch from the caller the carrier asks the PR for it,
// and only once a carry candidate exists.
func TestDecisionCarrierResolvesBaseOnDemand(t *testing.T) {
	const oldHead, newHead, baseTip, app = "aaaa", "bbbb", "b45e", "kubestellar-hive[bot]"
	same := []*gh.CommitFile{refreshFile("a.go", "blob1", "@@")}
	api := &fakeBaseRefreshRepos{
		commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, baseTip)},
		compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same...)},
	}
	store := review.NewHumanChangeRequests("")
	resolved := 0
	carrier := &DecisionCarrier{Store: store, API: api, AppBotLogin: app, Checker: &BaseRefreshChecker{}, ResolveBase: func(context.Context, string, string, int) (string, error) {
		resolved++
		return "v5", nil
	}}
	if _, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 1, "", newHead); ok || resolved != 0 || api.calls != 0 {
		t.Fatalf("no recorded decision must mean no lookups: ok=%v resolved=%d calls=%d", ok, resolved, api.calls)
	}
	if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 1, HeadSHA: oldHead, Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
		t.Fatal(err)
	}
	got, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 1, "", newHead)
	if !ok || got.HeadSHA != newHead || resolved != 1 {
		t.Fatalf("carry = %+v ok=%v resolved=%d", got, ok, resolved)
	}
}

// refreshChain builds n consecutive App base refreshes on top of "aaaa":
// h1 refreshes aaaa, h2 refreshes h1, and so on. broken, when > 0, makes
// that hop's author someone other than the App.
func refreshChain(n, broken int, app string) (*fakeBaseRefreshRepos, string) {
	same := []*gh.CommitFile{refreshFile("a.go", "blob1", "@@")}
	api := &fakeBaseRefreshRepos{commits: map[string]*gh.RepositoryCommit{}, compares: map[string]*gh.CommitsComparison{"v5...aaaa": refreshCompare("0ld", same...)}}
	prev := "aaaa"
	for i := 1; i <= n; i++ {
		sha, tip, login := "h"+strconv.Itoa(i), "tip"+strconv.Itoa(i), app
		if i == broken {
			login = "mendezr"
		}
		api.commits[sha] = refreshCommit(sha, login, prev, tip)
		api.compares["v5..."+sha] = refreshCompare(tip, same...)
		prev = sha
	}
	return api, prev
}

// hivecommons/hive#15766: two (or more) App refreshes landed before any
// lane read the PR, so the decided head is not a direct parent.
func TestDecisionCarrierCarriesAcrossConsecutiveBaseRefreshes(t *testing.T) {
	const app = "kubestellar-hive[bot]"
	for _, tc := range []struct {
		name   string
		hops   int
		broken int
		want   bool
	}{
		{name: "two hops", hops: 2, want: true},
		{name: "depth bound reached", hops: baseRefreshMaxHops, want: true},
		{name: "beyond the depth bound", hops: baseRefreshMaxHops + 1, want: false},
		{name: "newest hop not by the App", hops: 3, broken: 3, want: false},
		{name: "middle hop not by the App", hops: 3, broken: 2, want: false},
		{name: "oldest hop not by the App", hops: 3, broken: 1, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, head := refreshChain(tc.hops, tc.broken, app)
			store := review.NewHumanChangeRequests("")
			if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 15766, HeadSHA: "aaaa", Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
				t.Fatal(err)
			}
			carrier := &DecisionCarrier{Store: store, API: api, AppBotLogin: app, Checker: &BaseRefreshChecker{}}
			got, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 15766, "v5", head)
			if ok != tc.want {
				t.Fatalf("carried = %v, want %v (%+v)", ok, tc.want, got)
			}
			if tc.want && (got.HeadSHA != head || got.CarriedFromHead != "aaaa" || !got.IsApproval()) {
				t.Fatalf("carried record = %+v", got)
			}
		})
	}
}

// A content commit between two refreshes breaks the chain.
func TestDecisionCarrierRefusesChainBrokenByContentCommit(t *testing.T) {
	const app = "kubestellar-hive[bot]"
	same := []*gh.CommitFile{refreshFile("a.go", "blob1", "@@")}
	changed := []*gh.CommitFile{refreshFile("a.go", "blob9", "@@ +")}
	api := &fakeBaseRefreshRepos{
		commits: map[string]*gh.RepositoryCommit{
			"h2":   refreshCommit("h2", app, "h1", "tip2"),
			"h1":   refreshCommit("h1", app, "c0de", "tip1"),
			"c0de": refreshCommit("c0de", "someone", "aaaa"),
		},
		compares: map[string]*gh.CommitsComparison{
			"v5...aaaa": refreshCompare("0ld", same...),
			"v5...h1":   refreshCompare("tip1", changed...),
			"v5...h2":   refreshCompare("tip2", changed...),
		},
	}
	store := review.NewHumanChangeRequests("")
	if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 15766, HeadSHA: "aaaa", Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
		t.Fatal(err)
	}
	carrier := &DecisionCarrier{Store: store, API: api, AppBotLogin: app, Checker: &BaseRefreshChecker{}}
	if got, ok := carrier.Carry(context.Background(), "hivecommons", "hive", 15766, "v5", "h2"); ok {
		t.Fatalf("decision carried across a content commit: %+v", got)
	}
}

func TestSameChangedFilesIgnoresOrderButNotContent(t *testing.T) {
	a := []*gh.CommitFile{refreshFile("a.go", "1", "p1"), refreshFile("b.go", "2", "p2")}
	b := []*gh.CommitFile{refreshFile("b.go", "2", "p2"), refreshFile("a.go", "1", "p1")}
	if !sameChangedFiles(a, b) {
		t.Fatal("same files in another order must compare equal")
	}
	renamed := []*gh.CommitFile{refreshFile("a.go", "1", "p1"), refreshFile("c.go", "2", "p2")}
	if sameChangedFiles(a, renamed) {
		t.Fatal("a different file set must not compare equal")
	}
	noPatch := []*gh.CommitFile{refreshFile("a.go", "1", "p1"), {Filename: gh.Ptr("b.go"), Status: gh.Ptr("modified"), SHA: gh.Ptr("2"), Additions: gh.Ptr(3), Deletions: gh.Ptr(1), Changes: gh.Ptr(4)}}
	if !sameChangedFiles(a, noPatch) {
		t.Fatal("a patch GitHub omitted on one side must not fail the comparison when blob and counts agree")
	}
	if !CommitByGitHubLogin(refreshCommit("x", "Kubestellar-Hive"), "kubestellar-hive[bot]") || CommitByGitHubLogin(refreshCommit("x", "mendezr"), "kubestellar-hive[bot]") {
		t.Fatal("App attribution must match the login case-insensitively, [bot] suffix aside, and nobody else")
	}
}

// BaseMergeParent accepts a diff-preserving base merge whoever made it
// (projectbluefin/utah#1078's 9d8ceab was a web merge by the owner), and
// refuses one that changes the PR's diff or is not a base merge.
func TestBaseMergeParentAnyAuthor(t *testing.T) {
	files := []*gh.CommitFile{refreshFile("ci.yml", "blobA", "@@")}
	api := &fakeBaseRefreshRepos{
		commits: map[string]*gh.RepositoryCommit{
			"merge":   refreshCommit("merge", "Danathar", "head", "base2"),
			"changed": refreshCommit("changed", "Danathar", "head", "base2"),
			"single":  refreshCommit("single", "Danathar", "head"),
		},
		compares: map[string]*gh.CommitsComparison{
			"main...head":    refreshCompare("base1", files...),
			"main...merge":   refreshCompare("base2", files...),
			"main...changed": refreshCompare("base2", refreshFile("ci.yml", "blobB", "@@ changed")),
			"main...single":  refreshCompare("base1", files...),
		},
	}
	ctx := context.Background()
	if prev, ok := BaseMergeParent(ctx, api, "o", "r", "main", api.commits["merge"], "merge"); !ok || prev != "head" {
		t.Fatalf("diff-preserving merge by a person: prev=%q ok=%v", prev, ok)
	}
	if _, ok := BaseMergeParent(ctx, api, "o", "r", "main", api.commits["changed"], "changed"); ok {
		t.Fatal("a merge that changes the PR's diff must not count")
	}
	if _, ok := BaseMergeParent(ctx, api, "o", "r", "main", api.commits["single"], "single"); ok {
		t.Fatal("a single-parent commit is not a base merge")
	}
}

// hivecommons/hive#16860: GitHub derives a commit's author login from the
// author email, so a merge authored as the App's noreply address resolves to
// the App. Only a commit GitHub created and signed itself counts.
func TestHeadIsBaseRefreshOfRequiresVerifiedGitHubCommit(t *testing.T) {
	const oldHead, newHead, baseTip, app = "aaaa", "bbbb", "b45e", "kubestellar-hive[bot]"
	same := []*gh.CommitFile{refreshFile("a.go", "blob1", "@@")}
	for _, tc := range []struct {
		name   string
		mutate func(*gh.RepositoryCommit)
		want   bool
	}{
		{name: "verified update-branch merge", mutate: func(*gh.RepositoryCommit) {}, want: true},
		{name: "unverified", mutate: func(c *gh.RepositoryCommit) { c.Commit.Verification.Verified = gh.Ptr(false) }},
		{name: "no verification", mutate: func(c *gh.RepositoryCommit) { c.Commit = nil }},
		{name: "verified but committed by someone else", mutate: func(c *gh.RepositoryCommit) { c.Committer = &gh.User{Login: gh.Ptr("mallory")} }},
		{name: "verified, committed by the App", mutate: func(c *gh.RepositoryCommit) { c.Committer = &gh.User{Login: gh.Ptr(app)} }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commit := refreshCommit(newHead, app, oldHead, baseTip)
			tc.mutate(commit)
			api := &fakeBaseRefreshRepos{
				commits:  map[string]*gh.RepositoryCommit{newHead: commit},
				compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", same...), "v5..." + newHead: refreshCompare(baseTip, same...)},
			}
			if got := (&BaseRefreshChecker{}).HeadIsBaseRefreshOf(context.Background(), api, "hivecommons", "hive", 1, "v5", oldHead, newHead, app); got != tc.want {
				t.Fatalf("HeadIsBaseRefreshOf = %v, want %v", got, tc.want)
			}
		})
	}
}

// hivecommons/hive#16860: GitHub lists at most 300 files for a compare, so
// two diffs at the cap may differ in files it left out; that fails closed.
func TestHeadIsBaseRefreshOfFailsClosedAtCompareFileCap(t *testing.T) {
	const oldHead, newHead, baseTip, app = "aaaa", "bbbb", "b45e", "kubestellar-hive[bot]"
	for _, n := range []int{baseRefreshCompareFileCap - 1, baseRefreshCompareFileCap} {
		files := make([]*gh.CommitFile, 0, n)
		for i := 0; i < n; i++ {
			files = append(files, refreshFile("f"+strconv.Itoa(i)+".go", "blob"+strconv.Itoa(i), "@@"))
		}
		api := &fakeBaseRefreshRepos{
			commits:  map[string]*gh.RepositoryCommit{newHead: refreshCommit(newHead, app, oldHead, baseTip)},
			compares: map[string]*gh.CommitsComparison{"v5..." + oldHead: refreshCompare("0ld", files...), "v5..." + newHead: refreshCompare(baseTip, files...)},
		}
		want := n < baseRefreshCompareFileCap
		if got := (&BaseRefreshChecker{}).HeadIsBaseRefreshOf(context.Background(), api, "hivecommons", "hive", 1, "v5", oldHead, newHead, app); got != want {
			t.Fatalf("%d files: HeadIsBaseRefreshOf = %v, want %v", n, got, want)
		}
		if _, _, err := compareAllFiles(context.Background(), api, "hivecommons", "hive", "v5", newHead); want != (err == nil) || (!want && !errors.Is(err, ErrCompareFileCap)) {
			t.Fatalf("%d files: compareAllFiles err = %v", n, err)
		}
	}
}

// A base merge that also edited a file the PR modifies, away from the PR's
// hunks, leaves every hunk byte-for-byte the same at other line numbers: the
// blob differs but the change is still the one approved
// (projectbluefin/utah-packages#924). Anything more — a changed context
// line or hunk length, a missing patch, a rename, any `.github/` file — is
// still a different diff.
func TestSameChangedFilesToleratesShiftedHunksOutsideDotGitHub(t *testing.T) {
	const approved = "@@ -145,32 +145,82 @@ Verify a fix with `rebuild-rpms.yml`\n ctx\n-old\n+new\n ctx\n@@ -207,7 +257,7 @@ way as libcdio.\n ctx\n-a\n+b"
	const shifted = "@@ -160,32 +160,82 @@ Verify a fix with `rebuild-rpms.yml`\n ctx\n-old\n+new\n ctx\n@@ -239,7 +289,7 @@ way as libcdio.\n ctx\n-a\n+b"
	file := func(name, status, blob, patch string) []*gh.CommitFile {
		f := refreshFile(name, blob, patch)
		f.Status = gh.Ptr(status)
		return []*gh.CommitFile{f}
	}
	const doc = "docs/skills/upstream-version-bumps.md"
	if !sameChangedFiles(file(doc, "modified", "3604514", approved), file(doc, "modified", "4da6d8a", shifted)) {
		t.Fatal("the same hunks at shifted line numbers must compare equal outside .github/")
	}
	noPatch := file(doc, "modified", "4da6d8a", "")
	noPatch[0].Patch = nil
	for name, tc := range map[string][2][]*gh.CommitFile{
		"a context line changed":  {file(doc, "modified", "3604514", approved), file(doc, "modified", "4da6d8a", strings.Replace(shifted, " ctx\n@@", " main's line\n@@", 1))},
		"a hunk length changed":   {file(doc, "modified", "3604514", approved), file(doc, "modified", "4da6d8a", strings.Replace(shifted, "+160,82", "+160,83", 1))},
		"a section heading moved": {file(doc, "modified", "3604514", approved), file(doc, "modified", "4da6d8a", strings.Replace(shifted, "way as libcdio.", "way as libcue.", 1))},
		"the patch was omitted":   {file(doc, "modified", "3604514", approved), noPatch},
		"the file was renamed":    {file(doc, "renamed", "3604514", approved), file(doc, "renamed", "4da6d8a", shifted)},
		"a workflow file":         {file(".github/workflows/bump.yml", "modified", "a0bbe66", approved), file(".github/workflows/bump.yml", "modified", "39b9f1e", shifted)},
		"a nested action file":    {file("tools/.github/actions/x/action.yml", "modified", "a0bbe66", approved), file("tools/.github/actions/x/action.yml", "modified", "39b9f1e", shifted)},
		"identical patch text":    {file(".github/workflows/bump.yml", "modified", "a0bbe66", approved), file(".github/workflows/bump.yml", "modified", "39b9f1e", approved)},
	} {
		if sameChangedFiles(tc[0], tc[1]) {
			t.Errorf("%s: a different blob must not compare equal here", name)
		}
	}
}
