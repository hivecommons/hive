package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ── prReferencesIssue ───────────────────────────────────────────────────────

func TestPrReferencesIssue_ExactMatch(t *testing.T) {
	iss := &gh.Issue{
		Title: strPtr("Fix login page"),
		Body:  strPtr("Fixes #42 by adding retry logic"),
	}
	if !prReferencesIssue(iss, 42) {
		t.Error("expected #42 to match in body")
	}
}

func TestPrReferencesIssue_TitleMatch(t *testing.T) {
	iss := &gh.Issue{
		Title: strPtr("Resolves #99"),
		Body:  strPtr("Some unrelated text"),
	}
	if !prReferencesIssue(iss, 99) {
		t.Error("expected #99 to match in title")
	}
}

func TestPrReferencesIssue_TrailingPunctuation(t *testing.T) {
	tests := []struct {
		body string
		num  int
	}{
		{"Closes #10.", 10},
		{"Fixes #20,", 20},
		{"See #30)", 30},
		{"refs #50:", 50},
	}
	for _, tt := range tests {
		iss := &gh.Issue{Title: strPtr(""), Body: strPtr(tt.body)}
		if !prReferencesIssue(iss, tt.num) {
			t.Errorf("expected #%d to match in %q", tt.num, tt.body)
		}
	}
}

func TestPrReferencesIssue_NoMatch(t *testing.T) {
	iss := &gh.Issue{
		Title: strPtr("Update docs"),
		Body:  strPtr("This improves #123 handling"),
	}
	// Looking for #42, not #123
	if prReferencesIssue(iss, 42) {
		t.Error("expected no match for #42")
	}
}

func TestPrReferencesIssue_NumberCollision(t *testing.T) {
	// #1 should not match #10, #100, etc.
	iss := &gh.Issue{
		Title: strPtr("Fix #100"),
		Body:  strPtr("Related to issue #1000"),
	}
	if prReferencesIssue(iss, 1) {
		t.Error("expected no match for #1 when only #100 and #1000 present")
	}
}

func TestPrReferencesIssue_EmptyBodyAndTitle(t *testing.T) {
	// When both are empty, return true (benefit of the doubt per design)
	iss := &gh.Issue{Title: strPtr(""), Body: strPtr("")}
	if !prReferencesIssue(iss, 5) {
		t.Error("expected true when body and title are both empty")
	}
}

func TestPrReferencesIssue_NilFields(t *testing.T) {
	// nil GetBody/GetTitle return ""
	iss := &gh.Issue{}
	if !prReferencesIssue(iss, 5) {
		t.Error("expected true when body and title are nil (both return empty)")
	}
}

// ── prLinkResolver caching ──────────────────────────────────────────────────

func TestPRLinkResolver_CacheHitWithinTTL(t *testing.T) {
	callCount := 0
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return time.Unix(1700000000, 0) },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			callCount++
			return &prLinkStatus{Number: 10, State: prLinkStateOpen}, nil
		},
	}

	ctx := context.Background()
	// First call populates cache
	r.resolve(ctx, "org/repo", 5)
	// Second call should hit cache
	r.resolve(ctx, "org/repo", 5)

	if callCount != 1 {
		t.Errorf("lookup called %d times, want 1 (cache hit)", callCount)
	}
}

func TestPRLinkResolver_CacheExpireAfterTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	callCount := 0
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return now },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			callCount++
			return &prLinkStatus{Number: 10, State: prLinkStateOpen}, nil
		},
	}

	ctx := context.Background()
	r.resolve(ctx, "org/repo", 5)

	// Advance past TTL (90s)
	now = now.Add(91 * time.Second)
	r.resolve(ctx, "org/repo", 5)

	if callCount != 2 {
		t.Errorf("lookup called %d times, want 2 (cache expired)", callCount)
	}
}

func TestPRLinkResolver_NilOnLookupError(t *testing.T) {
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return time.Unix(1700000000, 0) },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			return nil, fmt.Errorf("network error")
		},
	}

	got := r.resolve(context.Background(), "org/repo", 5)
	if got != nil {
		t.Errorf("expected nil on error, got %+v", got)
	}
}

func TestPRLinkResolver_NilCachesError(t *testing.T) {
	callCount := 0
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return time.Unix(1700000000, 0) },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			callCount++
			return nil, fmt.Errorf("fail")
		},
	}

	ctx := context.Background()
	r.resolve(ctx, "org/repo", 5)
	r.resolve(ctx, "org/repo", 5)

	// Error result is cached; second call should NOT re-lookup
	if callCount != 1 {
		t.Errorf("lookup called %d times, want 1 (nil cached)", callCount)
	}
}

func TestPRLinkResolver_NilReceiver(t *testing.T) {
	var r *prLinkResolver
	got := r.resolve(context.Background(), "org/repo", 5)
	if got != nil {
		t.Errorf("expected nil from nil receiver, got %+v", got)
	}
}

func TestPRLinkResolver_InvalidInputs(t *testing.T) {
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return time.Unix(1700000000, 0) },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			t.Fatal("lookup should not be called for invalid inputs")
			return nil, nil
		},
	}
	if r.resolve(context.Background(), "", 5) != nil {
		t.Error("expected nil for empty repo")
	}
	if r.resolve(context.Background(), "org/repo", 0) != nil {
		t.Error("expected nil for zero number")
	}
	if r.resolve(context.Background(), "org/repo", -1) != nil {
		t.Error("expected nil for negative number")
	}
}

func TestPRLinkResolver_ConcurrentAccess(t *testing.T) {
	callCount := 0
	var mu sync.Mutex
	r := &prLinkResolver{
		cache: make(map[string]prLinkCacheEntry),
		now:   func() time.Time { return time.Unix(1700000000, 0) },
		lookup: func(ctx context.Context, repo string, number int) (*prLinkStatus, error) {
			mu.Lock()
			callCount++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return &prLinkStatus{Number: number, State: prLinkStateOpen}, nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.resolve(context.Background(), "org/repo", 5)
		}()
	}
	wg.Wait()

	// Should not panic; exact call count depends on race but should be small
	if callCount > 10 {
		t.Errorf("too many calls (%d), possible cache bypass", callCount)
	}
}

// ── prLinkKey ───────────────────────────────────────────────────────────────

func TestPrLinkKey(t *testing.T) {
	got := prLinkKey("org/repo", 42)
	if got != "org/repo#42" {
		t.Errorf("prLinkKey = %q, want %q", got, "org/repo#42")
	}
}

// ── opportunisticHeat ───────────────────────────────────────────────────────

func TestOpportunisticHeat_BrandNew(t *testing.T) {
	heat, reason := opportunisticHeat(0, true, nil)
	// Age 0 → full recencyHeatMax (10.0)
	if heat != 10.0 {
		t.Errorf("heat = %f, want 10.0", heat)
	}
	if !strings.Contains(reason, "just now") {
		t.Errorf("reason = %q, want to contain 'just now'", reason)
	}
}

func TestOpportunisticHeat_HalfAge(t *testing.T) {
	halfFloor := 7 * 24 * 60 // 7 days = half of 14-day floor
	heat, _ := opportunisticHeat(halfFloor, true, nil)
	// frac = 1 - 0.5 = 0.5 → heat = 5.0
	if heat != 5.0 {
		t.Errorf("heat = %f, want 5.0", heat)
	}
}

func TestOpportunisticHeat_AtFloor(t *testing.T) {
	floor := 14 * 24 * 60 // exactly at floor
	heat, reason := opportunisticHeat(floor, true, nil)
	// At floor → no recency heat (condition is < floor)
	if heat != 0 {
		t.Errorf("heat = %f, want 0 (at floor)", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason = %q, want 'actionable'", reason)
	}
}

func TestOpportunisticHeat_NoAge(t *testing.T) {
	heat, reason := opportunisticHeat(100, false, nil)
	if heat != 0 {
		t.Errorf("heat = %f, want 0 (no age)", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason = %q, want 'actionable'", reason)
	}
}

func TestOpportunisticHeat_LabelBumps(t *testing.T) {
	heat, reason := opportunisticHeat(0, false, []string{"good first issue", "bug"})
	// No recency (haveAge=false), but labels: 3 + 1.5 = 4.5
	if heat != 4.5 {
		t.Errorf("heat = %f, want 4.5", heat)
	}
	if reason != "good first issue" {
		t.Errorf("reason = %q, want 'good first issue'", reason)
	}
}

func TestOpportunisticHeat_LabelCaseInsensitive(t *testing.T) {
	heat, _ := opportunisticHeat(0, false, []string{"Help Wanted"})
	if heat != 2.0 {
		t.Errorf("heat = %f, want 2.0", heat)
	}
}

func TestOpportunisticHeat_RecencyPlusLabels(t *testing.T) {
	// 1 day old: frac = 1 - (1440/20160) ≈ 0.9286; heat ≈ 9.286 + 2 (help wanted) = 11.286
	heat, reason := opportunisticHeat(24*60, true, []string{"help wanted"})
	expectedRecency := (1 - float64(24*60)/float64(14*24*60)) * 10.0
	expected := expectedRecency + 2.0
	if diff := heat - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("heat = %f, want ~%f", heat, expected)
	}
	// Reason comes from recency since haveAge is true and age < floor
	if !strings.Contains(reason, "ago") {
		t.Errorf("reason = %q, want to contain 'ago'", reason)
	}
}

func TestOpportunisticHeat_MultipleMatchingLabelsStack(t *testing.T) {
	heat, reason := opportunisticHeat(recencyHeatFloorMinutes, true, []string{"help wanted", "enhancement"})
	if heat != 3.0 {
		t.Errorf("heat = %f, want 3.0 for stacked help wanted + enhancement bumps", heat)
	}
	if reason != "help wanted" {
		t.Errorf("reason = %q, want first matching label reason", reason)
	}
}

func TestOpportunisticHeat_NegativeAge(t *testing.T) {
	heat, reason := opportunisticHeat(-5, true, nil)
	if heat != 0 {
		t.Errorf("heat = %f, want 0 for negative age", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason = %q, want actionable", reason)
	}
}

func TestOpportunisticHeat_UnknownLabelIgnored(t *testing.T) {
	heat, _ := opportunisticHeat(0, false, []string{"wontfix", "duplicate"})
	if heat != 0 {
		t.Errorf("heat = %f, want 0 (unknown labels)", heat)
	}
}

// ── humanizeAge ─────────────────────────────────────────────────────────────

func TestHumanizeAge(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{0, "just now"},
		{1, "just now"},
		{2, "2m ago"},
		{59, "59m ago"},
		{60, "1h ago"},
		{119, "1h ago"},
		{120, "2h ago"},
		{23 * 60, "23h ago"},
		{24 * 60, "1d ago"},
		{48 * 60, "2d ago"},
		{7 * 24 * 60, "7d ago"},
	}
	for _, tt := range tests {
		got := humanizeAge(tt.minutes)
		if got != tt.want {
			t.Errorf("humanizeAge(%d) = %q, want %q", tt.minutes, got, tt.want)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

// ── searchLinkedPR: commit / comment / timeline references (#14859) ────────

// TestSearchLinkedPR_CountsCommitCommentAndTimelineReferences pins
// hivecommons/hive#14859: a PR that names the issue only in a commit message
// or a PR comment must still count as linked. GitHub records those mentions
// as cross-referenced events on the issue's timeline; before the fix only the
// PR title/body was consulted, so such a PR read as "no linked PR".
func TestSearchLinkedPR_CountsCommitCommentAndTimelineReferences(t *testing.T) {
	const issue = 13135
	// A search hit whose title/body does not name the issue: GitHub search
	// matched it on a comment or commit mention.
	searchHit := `{"number":14656,"state":"open","title":"keep the header chips from overlapping the title","body":"Hive Agent","html_url":"https://github.com/o/r/pull/14656","pull_request":{"url":"x"}}`
	crossRef := func(src string) string {
		return `{"event":"cross-referenced","created_at":"2026-10-27T00:00:00Z","source":{"type":"issue","issue":` + src + `}}`
	}
	openPR := `{"number":14656,"state":"open","html_url":"https://github.com/o/r/pull/14656","repository":{"full_name":"o/r"},"pull_request":{"url":"x"}}`
	mergedPR := `{"number":14656,"state":"closed","html_url":"https://github.com/o/r/pull/14656","repository":{"full_name":"o/r"},"pull_request":{"url":"x","merged_at":"2026-10-28T00:00:00Z"}}`
	otherRepoPR := `{"number":77,"state":"open","html_url":"https://github.com/x/y/pull/77","repository":{"full_name":"x/y"},"pull_request":{"url":"x"}}`
	plainIssue := `{"number":14859,"state":"open","html_url":"https://github.com/o/r/issues/14859","repository":{"full_name":"o/r"}}`

	cases := []struct {
		name       string
		search     string
		timeline   string
		wantNumber int
		wantState  string
	}{
		{
			name:       "comment reference only (cross-referenced event, search miss)",
			search:     `[]`,
			timeline:   `[` + crossRef(openPR) + `]`,
			wantNumber: 14656,
			wantState:  prLinkStateOpen,
		},
		{
			name:       "PR comment reference only (search hit rejected by title/body)",
			search:     `[` + searchHit + `]`,
			timeline:   `[` + crossRef(openPR) + `]`,
			wantNumber: 14656,
			wantState:  prLinkStateOpen,
		},
		{
			name:       "timeline cross-reference from a merged PR",
			search:     `[]`,
			timeline:   `[{"event":"labeled","created_at":"2026-10-26T00:00:00Z"},` + crossRef(mergedPR) + `]`,
			wantNumber: 14656,
			wantState:  prLinkStateMerged,
		},
		{
			name:     "no reference anywhere",
			search:   `[` + searchHit + `]`,
			timeline: `[{"event":"labeled","created_at":"2026-10-26T00:00:00Z"},` + crossRef(otherRepoPR) + `,` + crossRef(plainIssue) + `]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/search/issues", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"total_count":1,"items":%s}`, tc.search)
			})
			mux.HandleFunc(fmt.Sprintf("/repos/o/r/issues/%d/timeline", issue), func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.timeline)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			gc := gh.NewClient(nil)
			base, err := url.Parse(srv.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			gc.BaseURL = base

			got, err := searchLinkedPR(context.Background(), gc, "o/r", issue)
			if err != nil {
				t.Fatalf("searchLinkedPR: %v", err)
			}
			if tc.wantNumber == 0 {
				if got != nil {
					t.Fatalf("got linked PR %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got no linked PR, want #%d (%s)", tc.wantNumber, tc.wantState)
			}
			if got.Number != tc.wantNumber || got.State != tc.wantState {
				t.Fatalf("got #%d (%s), want #%d (%s)", got.Number, got.State, tc.wantNumber, tc.wantState)
			}
		})
	}
}

// TestCrossReferencingPRs_PageCap pins the prLinkTimelineMaxPages bound: a
// timeline that keeps advertising a next page is read at most that many
// times, and the lookup keeps what it has read instead of walking to
// exhaustion — the design note on prLinkSearchPerPage applies to the timeline
// read too.
func TestCrossReferencingPRs_PageCap(t *testing.T) {
	const issue = 13135
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/o/r/issues/%d/timeline", issue), func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s?page=%d>; rel="next"`, "http://x"+r.URL.Path, calls+1))
		fmt.Fprintf(w, `[{"event":"cross-referenced","created_at":"2026-10-27T00:00:00Z","source":{"type":"issue","issue":{"number":%s00,"state":"open","html_url":"https://github.com/o/r/pull/%s00","repository":{"full_name":"o/r"},"pull_request":{"url":"x"}}}}]`, page, page)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gh.NewClient(nil)
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	gc.BaseURL = base

	got := crossReferencingPRs(context.Background(), gc, "o/r", issue)
	if calls != prLinkTimelineMaxPages {
		t.Fatalf("timeline requests = %d, want exactly %d (page cap)", calls, prLinkTimelineMaxPages)
	}
	if len(got) != prLinkTimelineMaxPages {
		t.Fatalf("got %d PRs from %d pages, want one per page", len(got), prLinkTimelineMaxPages)
	}
	if got[0].GetNumber() != 100 || got[len(got)-1].GetNumber() != prLinkTimelineMaxPages*100 {
		t.Fatalf("got PRs %d..%d, want 100..%d in page order", got[0].GetNumber(), got[len(got)-1].GetNumber(), prLinkTimelineMaxPages*100)
	}
}
