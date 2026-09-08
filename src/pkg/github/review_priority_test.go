package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassifyReviewClass(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		labels []string
		want   ReviewClass
	}{
		{"bug emoji", "🐛 fix: leaked goroutine in proxy stall timeout", nil, ReviewClassFix},
		{"lock emoji", "🔒 restrict claude local-mode write roots", nil, ReviewClassFix},
		{"fix scope", "fix(proxy): retry transient GHCR publish", nil, ReviewClassFix},
		{"fix colon", "fix: retry transient GHCR publish", nil, ReviewClassFix},
		{"scanner prefix", "[scanner] atomic stall-timeout in the proxy", nil, ReviewClassFix},
		{"ci-maintainer prefix", "[ci-maintainer] pin setup-go", nil, ReviewClassFix},
		{"fixtures is not fix", "fixtures: reshuffle golden files", nil, ReviewClassUnknown},
		{"architect prefix", "[architect] refactor: drop dead snapshot dir", nil, ReviewClassRefactorDocs},
		{"refactor word", "refactor: collapse duplicate status builders", nil, ReviewClassRefactorDocs},
		{"docs emoji", "📖 docs: explain exit 77 netfilter", nil, ReviewClassRefactorDocs},
		{"docs word", "docs: explain exit 77 netfilter", nil, ReviewClassRefactorDocs},
		{"docker is not docs", "docker: bump base image", nil, ReviewClassUnknown},
		{"strategist prefix", "[strategist] planning: propose triage policy", nil, ReviewClassRefactorDocs},
		{"quality prefix", "[quality] test: cover repos_rescan error path", nil, ReviewClassTests},
		{"test word", "test: cover repos_rescan error path", nil, ReviewClassTests},
		{"tests word", "tests: cover repos_rescan error path", nil, ReviewClassTests},
		{"test emoji", "🧪 cover repos_rescan error path", nil, ReviewClassTests},
		{"sprout then fix", "🌱 fix: something behind a sprout", nil, ReviewClassFix},
		{"feature unknown", "✨ feature: new dashboard card", nil, ReviewClassUnknown},
		{"quality lane label", "cover repos_rescan error path", []string{"hold", "agent/quality"}, ReviewClassTests},
		{"scanner lane label", "handle nil forge in rescan", []string{"agent/scanner", "hold"}, ReviewClassFix},
		{"guide lane label", "explain exit 77 netfilter", []string{"agent/guide"}, ReviewClassRefactorDocs},
		{"title beats label", "fix: real bug from the quality lane", []string{"agent/quality"}, ReviewClassFix},
		{"kind/bug label", "handle nil forge in rescan", []string{"kind/bug"}, ReviewClassFix},
		{"nothing", "", nil, ReviewClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyReviewClass(tc.title, tc.labels); got != tc.want {
				t.Errorf("ClassifyReviewClass(%q, %v) = %q, want %q", tc.title, tc.labels, got, tc.want)
			}
		})
	}
}

// TestSortPullRequestsForReview_NewerFixBeatsOlderTest is the fix under test
// for #6183: a fix opened an hour ago must sort ahead of a coverage PR that
// has been queued for two days, and a docs PR sits between them.
func TestSortPullRequestsForReview_NewerFixBeatsOlderTest(t *testing.T) {
	now := time.Now()
	prs := []PullRequest{
		{Number: 1, Title: "[quality] test: cover foo", CreatedAt: now.Add(-48 * time.Hour)},
		{Number: 2, Title: "📖 docs: explain foo", CreatedAt: now.Add(-24 * time.Hour)},
		{Number: 3, Title: "🐛 fix: foo leaks a goroutine", CreatedAt: now.Add(-1 * time.Hour)},
	}
	SortPullRequestsForReview(prs)

	if got := []int{prs[0].Number, prs[1].Number, prs[2].Number}; got[0] != 3 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("order = %v, want [3 2 1] (fix > docs > test regardless of age)", got)
	}
	if prs[0].ReviewClass != ReviewClassFix || prs[1].ReviewClass != ReviewClassRefactorDocs || prs[2].ReviewClass != ReviewClassTests {
		t.Errorf("review classes not stamped: %q %q %q", prs[0].ReviewClass, prs[1].ReviewClass, prs[2].ReviewClass)
	}
}

// TestSortPullRequestsForReview_SameClassKeepsAgeOrder: within one class the
// pre-existing oldest-first order is the secondary key, so nothing regresses
// for a queue made of a single class.
func TestSortPullRequestsForReview_SameClassKeepsAgeOrder(t *testing.T) {
	now := time.Now()
	prs := []PullRequest{
		{Number: 10, Title: "test: newer", CreatedAt: now.Add(-1 * time.Hour)},
		{Number: 11, Title: "test: oldest", CreatedAt: now.Add(-72 * time.Hour)},
		{Number: 12, Title: "test: middle", CreatedAt: now.Add(-24 * time.Hour)},
		{Number: 20, Title: "fix: newer", CreatedAt: now.Add(-2 * time.Hour)},
		{Number: 21, Title: "fix: older", CreatedAt: now.Add(-30 * time.Hour)},
	}
	SortPullRequestsForReview(prs)

	want := []int{21, 20, 11, 12, 10}
	for i, n := range want {
		if prs[i].Number != n {
			got := make([]int, len(prs))
			for j := range prs {
				got[j] = prs[j].Number
			}
			t.Fatalf("order = %v, want %v (oldest first within each class)", got, want)
		}
	}
}

// TestSortPullRequestsForReview_UnknownRanksWithRefactors: a prefix-less
// adopter feature PR must not be pushed behind coverage PRs merely for
// lacking a conventional title.
func TestSortPullRequestsForReview_UnknownRanksWithRefactors(t *testing.T) {
	now := time.Now()
	prs := []PullRequest{
		{Number: 1, Title: "test: cover foo", CreatedAt: now.Add(-48 * time.Hour)},
		{Number: 2, Title: "Add the frobnicator", CreatedAt: now.Add(-1 * time.Hour)},
		{Number: 3, Title: "refactor: foo", CreatedAt: now.Add(-24 * time.Hour)},
	}
	SortPullRequestsForReview(prs)
	if prs[0].Number != 3 || prs[1].Number != 2 || prs[2].Number != 1 {
		t.Fatalf("order = [%d %d %d], want [3 2 1]", prs[0].Number, prs[1].Number, prs[2].Number)
	}
}

func TestSortHoldItemsForReview_ClassThenAge(t *testing.T) {
	now := time.Now()
	items := []HoldItem{
		{Number: 1, Type: "pr", ReviewClass: ReviewClassTests, CreatedAt: now.Add(-48 * time.Hour)},
		{Number: 2, Type: "issue", CreatedAt: now.Add(-10 * time.Hour)},
		{Number: 3, Type: "pr", ReviewClass: ReviewClassFix, CreatedAt: now.Add(-1 * time.Hour)},
		{Number: 4, Type: "pr", ReviewClass: ReviewClassFix, CreatedAt: now.Add(-5 * time.Hour)},
	}
	SortHoldItemsForReview(items)
	want := []int{4, 3, 2, 1}
	for i, n := range want {
		if items[i].Number != n {
			t.Fatalf("items[%d] = #%d, want #%d (full order want %v)", i, items[i].Number, n, want)
		}
	}
}

// TestEnumerateActionable_ReviewOrderInSnapshot drives the real enumeration
// path: a held (hold-gated) fix opened after a held coverage PR must come out
// first in Hold.Items, and the same holds for un-held PRs in PRs.Items. The
// wire fixtures are served newest-first, as GitHub's list endpoint does.
func TestEnumerateActionable_ReviewOrderInSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{Number: 104, Title: "🐛 fix: leaked goroutine", User: wireUser{Login: "bot"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(1), HTMLURL: "u"},
		{Number: 103, Title: "test: cover foo", User: wireUser{Login: "bot"}, Labels: []wireLabel{{Name: "hold"}, {Name: "agent/quality"}}, CreatedAt: hoursAgo(40), HTMLURL: "u"},
		{Number: 102, Title: "test: cover bar", User: wireUser{Login: "bot"}, CreatedAt: hoursAgo(30), HTMLURL: "u"},
		{Number: 101, Title: "fix: nil forge in rescan", User: wireUser{Login: "bot"}, CreatedAt: hoursAgo(2), HTMLURL: "u"},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	res, err := c.EnumerateActionable(t.Context())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}

	if len(res.Hold.Items) != 2 || res.Hold.Items[0].Number != 104 || res.Hold.Items[1].Number != 103 {
		t.Fatalf("Hold.Items = %+v, want [#104 fix, #103 test]", res.Hold.Items)
	}
	if res.Hold.Items[0].ReviewClass != ReviewClassFix || res.Hold.Items[0].CreatedAt.IsZero() {
		t.Errorf("held fix = %+v, want review_class=fix and a created_at", res.Hold.Items[0])
	}
	if len(res.PRs.Items) != 2 || res.PRs.Items[0].Number != 101 || res.PRs.Items[1].Number != 102 {
		t.Fatalf("PRs.Items = %+v, want [#101 fix, #102 test]", res.PRs.Items)
	}
	if res.PRs.Items[0].ReviewClass != ReviewClassFix || res.PRs.Items[1].ReviewClass != ReviewClassTests {
		t.Errorf("review classes = %q, %q", res.PRs.Items[0].ReviewClass, res.PRs.Items[1].ReviewClass)
	}
}
