package automerge

// Tests for the contributor-lane auto-merge tier: the eligible path, every
// distinct skip reason, soak timing through an injected clock, and the policy
// gates on the sweep entry point. No test sleeps; time advances by mutating
// the fixture clock.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	contributorLaneTestIssue  = 42
	contributorLaneTestHive   = "hive-prod"
	contributorLaneTestAuthor = "mrbobbytables"
	contributorLaneTestSoak   = 2 * time.Hour
)

func contributorClaimMarker(who, kind, hive string) string {
	return "<!-- hive:claim who=" + who + " kind=" + kind + " id=abc hive=" + hive + " until=2030-01-01T00:00:00Z -->\nClaimed."
}

type contributorLaneFixture struct {
	author          string
	body            string
	labels          []string
	draft           bool
	mergeableState  string
	headRepo        string
	listStatus      int
	getStatus       int
	recheckHeadSHA  string
	issueComments   []map[string]any // comments on the linked issue
	prComments      []map[string]any // comments on the PR
	issueCommentsSt int
	reviews         []map[string]any
	mergeStatus     int
	mergeNotApplied bool
	commentStatus   int
	merges          int
	comments        int
	updates         int
	lastComment     string
}

func newContributorLaneAPI(t *testing.T, fx *contributorLaneFixture) *httptest.Server {
	t.Helper()
	fetches := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := fx.mergeableState
		if state == "" {
			state = "clean"
		}
		author := fx.author
		if author == "" {
			author = contributorLaneTestAuthor
		}
		headRepo := fx.headRepo
		if headRepo == "" {
			headRepo = "acme/widget"
		}
		labels := make([]map[string]string, 0, len(fx.labels))
		for _, l := range fx.labels {
			labels = append(labels, map[string]string{"name": l})
		}
		pull := func(sha string) map[string]any {
			return map[string]any{
				"number":          7,
				"state":           "open",
				"draft":           fx.draft,
				"body":            fx.body,
				"mergeable_state": state,
				"mergeable":       state == "clean",
				"user":            map[string]string{"login": author},
				"head":            map[string]any{"sha": sha, "repo": map[string]string{"full_name": headRepo}},
				"base":            map[string]any{"repo": map[string]string{"full_name": "acme/widget"}},
				"labels":          labels,
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			if fx.listStatus != 0 {
				http.Error(w, "list error", fx.listStatus)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{pull("sha7")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			fetches++
			if fetches == 1 && fx.getStatus != 0 {
				http.Error(w, "get error", fx.getStatus)
				return
			}
			sha := "sha7"
			if fetches > 1 && fx.recheckHeadSHA != "" {
				sha = fx.recheckHeadSHA
			}
			json.NewEncoder(w).Encode(pull(sha))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/42/comments":
			if fx.issueCommentsSt != 0 {
				http.Error(w, "comments error", fx.issueCommentsSt)
				return
			}
			json.NewEncoder(w).Encode(fx.issueComments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			json.NewEncoder(w).Encode(fx.prComments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode(fx.reviews)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
			json.NewEncoder(w).Encode(map[string]any{
				"state":       "success",
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": "success"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
			json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			fx.merges++
			if fx.mergeStatus != 0 {
				w.WriteHeader(fx.mergeStatus)
				json.NewEncoder(w).Encode(map[string]string{"message": "merge error"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"merged": !fx.mergeNotApplied, "sha": "merge7"})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			fx.updates++
			json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			fx.comments++
			if fx.commentStatus != 0 {
				http.Error(w, "comment error", fx.commentStatus)
				return
			}
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			fx.lastComment = payload.Body
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

// contributorLaneClock is a mutable fake clock injected via Options.Now.
type contributorLaneClock struct{ at time.Time }

func (c *contributorLaneClock) now() time.Time { return c.at }

func (c *contributorLaneClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func defaultContributorLanePolicy() ContributorLanePolicy {
	return ContributorLanePolicy{
		Enabled:                true,
		HiveID:                 contributorLaneTestHive,
		Soak:                   contributorLaneTestSoak,
		RequireReviewerVerdict: true,
		ExcludeLabels:          map[string]bool{"hold": true, "do-not-merge": true, "needs-human": true, "blocked": true, "needs-rebase": true},
	}
}

func validContributorFixture() contributorLaneFixture {
	return contributorLaneFixture{
		body: "Fixes #42\n\nSome change.",
		issueComments: []map[string]any{{
			"id":   1,
			"user": map[string]string{"login": testHiveAppBotLogin},
			"body": contributorClaimMarker(contributorLaneTestAuthor, "contributor", contributorLaneTestHive),
		}},
	}
}

func approvedVerdict(string, int, string) ContributorLaneVerdict {
	return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
}

func newContributorLaneEngine(apiURL string, policy ContributorLanePolicy, clock *contributorLaneClock, opts Options) *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	opts.ContributorLanePolicy = func() ContributorLanePolicy { return policy }
	if opts.ContributorLaneVerdict == nil {
		opts.ContributorLaneVerdict = approvedVerdict
	}
	if opts.ContributorSoak == nil {
		opts.ContributorSoak = NewContributorSoakTracker()
	}
	if clock != nil {
		opts.Now = clock.now
	}
	if opts.SentinelLabel == nil {
		opts.SentinelLabel = func() string { return "hive-sentinel" }
	}
	return New(client, opts)
}

// soakedTracker returns a tracker that already recorded sha7 going green a
// full soak window before the clock, so the eligible path merges immediately.
func soakedTracker(clock *contributorLaneClock) *ContributorSoakTracker {
	tracker := NewContributorSoakTracker()
	tracker.observeGreen(contributorSoakKey("acme", "widget", 7, "sha7"), clock.at.Add(-contributorLaneTestSoak))
	return tracker
}

func TestTrySweepContributorLanePRSkipReasons(t *testing.T) {
	appClaim := func(who, hive string) []map[string]any {
		return []map[string]any{{
			"id":   1,
			"user": map[string]string{"login": testHiveAppBotLogin},
			"body": contributorClaimMarker(who, "contributor", hive),
		}}
	}
	for _, tc := range []struct {
		name       string
		mutate     func(fx *contributorLaneFixture)
		policy     func(p *ContributorLanePolicy)
		verdict    ContributorLaneVerdictFunc
		wantReason string
		wantErr    bool
		wantMerges int
		wantUpdate int
	}{
		{name: "eligible merges", wantMerges: 1},
		{name: "pr gone", mutate: func(fx *contributorLaneFixture) { fx.getStatus = http.StatusNotFound }, wantReason: "gone"},
		{name: "fetch error", mutate: func(fx *contributorLaneFixture) { fx.getStatus = http.StatusInternalServerError }, wantReason: "fetch-pr", wantErr: true},
		{name: "draft", mutate: func(fx *contributorLaneFixture) { fx.draft = true }, wantReason: "draft"},
		{name: "app-authored belongs to self lane", mutate: func(fx *contributorLaneFixture) { fx.author = testHiveAppBotLogin }, wantReason: contributorLaneReasonAppAuthored},
		{name: "sentinel label blocks", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"hive-sentinel"} }, wantReason: contributorLaneReasonSentinelBlocked},
		{name: "hold label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"hold"} }, wantReason: "label:hold"},
		{name: "do-not-merge slash label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"do-not-merge/wip"} }, wantReason: "label:do-not-merge/wip"},
		{name: "policy exclude label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"blocked"} }, wantReason: "excluded-label:blocked"},
		{name: "no linked issue", mutate: func(fx *contributorLaneFixture) { fx.body = "No issue here." }, wantReason: contributorLaneReasonNoLinkedIssue},
		{name: "refs keyword is not closing", mutate: func(fx *contributorLaneFixture) { fx.body = "Refs #42" }, wantReason: contributorLaneReasonNoLinkedIssue},
		{name: "no claim marker", mutate: func(fx *contributorLaneFixture) { fx.issueComments = nil }, wantReason: contributorLaneReasonNoClaim},
		{
			name:       "claim from non-app author is ignored",
			mutate:     func(fx *contributorLaneFixture) { fx.issueComments[0]["user"] = map[string]string{"login": "someone"} },
			wantReason: contributorLaneReasonNoClaim,
		},
		{
			name: "maintainer-kind claim is not a contributor claim",
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments[0]["body"] = contributorClaimMarker(contributorLaneTestAuthor, "maintainer", contributorLaneTestHive)
			},
			wantReason: contributorLaneReasonNoClaim,
		},
		{name: "claim author mismatch", mutate: func(fx *contributorLaneFixture) { fx.issueComments = appClaim("someoneelse", contributorLaneTestHive) }, wantReason: contributorLaneReasonClaimAuthor},
		{name: "claim for other hive", mutate: func(fx *contributorLaneFixture) {
			fx.issueComments = appClaim(contributorLaneTestAuthor, "hive-staging")
		}, wantReason: contributorLaneReasonClaimOtherHive},
		{
			name: "other-hive outranks author mismatch",
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments = append(appClaim("someoneelse", contributorLaneTestHive), appClaim(contributorLaneTestAuthor, "hive-staging")...)
			},
			wantReason: contributorLaneReasonClaimOtherHive,
		},
		{name: "claim comments fetch error", mutate: func(fx *contributorLaneFixture) { fx.issueCommentsSt = http.StatusInternalServerError }, wantReason: "claim-comments-fetch", wantErr: true},
		{
			name: "reporter-trust hold",
			mutate: func(fx *contributorLaneFixture) {
				fx.prComments = []map[string]any{{"id": 9, "user": map[string]string{"login": testHiveAppBotLogin}, "body": hgithub.ReporterTrustNoticeMarker + " held"}}
			},
			wantReason: contributorLaneReasonReporterTrustHold,
		},
		{
			name: "changes requested",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{{"id": 1, "state": "CHANGES_REQUESTED", "user": map[string]string{"login": "maintainer"}}}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
		{name: "behind updates branch", mutate: func(fx *contributorLaneFixture) { fx.mergeableState = "behind" }, wantReason: "check-pending", wantUpdate: 1},
		{name: "conflicting", mutate: func(fx *contributorLaneFixture) { fx.mergeableState = "dirty" }, wantReason: "conflicting"},
		{
			name:       "reviewer configured without approval",
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{Configured: true} },
			wantReason: contributorLaneReasonNoVerdict,
		},
		{
			name:       "no reviewer configured is satisfied",
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{} },
			wantMerges: 1,
		},
		{
			name:       "verdict not required ignores reviewer",
			policy:     func(p *ContributorLanePolicy) { p.RequireReviewerVerdict = false },
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{Configured: true} },
			wantMerges: 1,
		},
		{name: "head changed since eval", mutate: func(fx *contributorLaneFixture) { fx.recheckHeadSHA = "sha8" }, wantReason: "head-changed-since-eval"},
		{name: "merge api error", mutate: func(fx *contributorLaneFixture) { fx.mergeStatus = http.StatusConflict }, wantReason: "merge-failed", wantErr: true, wantMerges: 1},
		{name: "merge not applied", mutate: func(fx *contributorLaneFixture) { fx.mergeNotApplied = true }, wantReason: "merge-not-applied", wantMerges: 1},
		{name: "comment failure does not undo merge", mutate: func(fx *contributorLaneFixture) { fx.commentStatus = http.StatusInternalServerError }, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorLaneVerdict: tc.verdict, ContributorSoak: soakedTracker(clock)})
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges || fx.updates != tc.wantUpdate {
				t.Fatalf("merges=%d updates=%d, want %d/%d", fx.merges, fx.updates, tc.wantMerges, tc.wantUpdate)
			}
			if tc.wantReason == "" && !tc.wantErr {
				if event.Tier != contributorLaneTier || event.Number != 7 || event.MergeSHA != "merge7" || event.Author != contributorLaneTestAuthor {
					t.Fatalf("event = %+v, want contributor-lane merge of PR 7", event)
				}
			}
		})
	}
}

func TestTrySweepContributorLanePRMergeCommentText(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want merge", reason, err)
	}
	want := "Merged by hive contributor-lane auto-merge: CI green for ≥120m, reviewer verdict approve, claim by @mrbobbytables on #42 verified."
	if fx.comments != 1 || fx.lastComment != want {
		t.Fatalf("comment = %q (n=%d), want %q", fx.lastComment, fx.comments, want)
	}
	noReviewer := contributorLaneMergeComment("@bob", 5, 30*time.Minute, ContributorLaneVerdict{})
	if !strings.Contains(noReviewer, "≥30m, no reviewer agent configured, claim by @bob on #5") {
		t.Fatalf("no-reviewer comment = %q", noReviewer)
	}
}

func TestTrySweepContributorLanePRSoakTiming(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	tracker := NewContributorSoakTracker()
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: tracker})
	ctx := context.Background()

	// First green observation: soak starts now, nothing merges.
	_, reason, err := engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonSoaking+"(remaining=2h0m0s)" {
		t.Fatalf("first pass reason=%q err=%v", reason, err)
	}
	firstGreen, ok := tracker.FirstGreen("acme", "widget", 7, "sha7")
	if !ok || !firstGreen.Equal(clock.at) {
		t.Fatalf("first green = %v ok=%v, want %v", firstGreen, ok, clock.at)
	}

	// Half-way through the soak the remaining time is reported.
	clock.advance(time.Hour)
	_, reason, err = engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonSoaking+"(remaining=1h0m0s)" {
		t.Fatalf("mid-soak reason=%q err=%v", reason, err)
	}
	if got, _ := tracker.FirstGreen("acme", "widget", 7, "sha7"); !got.Equal(firstGreen) {
		t.Fatalf("first green moved to %v", got)
	}
	if fx.merges != 0 {
		t.Fatalf("merged during soak")
	}

	// Once the soak elapses the PR merges.
	clock.advance(time.Hour)
	event, reason, err := engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "" || fx.merges != 1 || event.Tier != contributorLaneTier {
		t.Fatalf("post-soak reason=%q err=%v merges=%d event=%+v", reason, err, fx.merges, event)
	}
}

func TestContributorSoakTrackerNewHeadRestartsAndPrunes(t *testing.T) {
	tracker := NewContributorSoakTracker()
	t0 := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	key1 := contributorSoakKey("acme", "widget", 7, "sha7")
	key2 := contributorSoakKey("acme", "widget", 7, "sha8")
	if got := tracker.observeGreen(key1, t0); !got.Equal(t0) {
		t.Fatalf("first observe = %v", got)
	}
	if got := tracker.observeGreen(key2, t0.Add(time.Hour)); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("new head should restart soak, got %v", got)
	}
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha7"); !ok {
		t.Fatalf("old head should still be tracked before prune")
	}
	tracker.observeGreen(key2, t0.Add(contributorSoakPruneAfter+2*time.Hour))
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha7"); ok {
		t.Fatalf("stale head should be pruned")
	}
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha8"); !ok {
		t.Fatalf("live head must survive prune")
	}
	var nilTracker *ContributorSoakTracker
	if got := nilTracker.observeGreen(key1, t0); !got.Equal(t0) {
		t.Fatalf("nil tracker observe = %v", got)
	}
	if _, ok := nilTracker.FirstGreen("acme", "widget", 7, "sha7"); ok {
		t.Fatalf("nil tracker should report nothing")
	}
}

func TestSweepContributorLaneAutoMergesPolicyGates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      func(p *ContributorLanePolicy)
		opts        Options
		mutate      func(fx *contributorLaneFixture)
		wantErr     bool
		wantSeen    int
		wantSkipped int
		wantMerged  int
	}{
		{name: "tier disabled", policy: func(p *ContributorLanePolicy) { p.Enabled = false }},
		{name: "repo outside allow-list", policy: func(p *ContributorLanePolicy) { p.Repos = map[string]bool{"other": true} }},
		{name: "repo inside allow-list merges", policy: func(p *ContributorLanePolicy) { p.Repos = map[string]bool{"widget": true} }, wantSeen: 1, wantMerged: 1},
		{name: "repo auto-merge disabled", opts: Options{RepoAutoMergeEnabled: func(string) bool { return false }}},
		{name: "list error is returned", mutate: func(fx *contributorLaneFixture) { fx.listStatus = http.StatusInternalServerError }, wantErr: true},
		{name: "prefilter skip counts", mutate: func(fx *contributorLaneFixture) { fx.draft = true }, wantSeen: 1, wantSkipped: 1},
		{name: "per-PR skip counts", mutate: func(fx *contributorLaneFixture) { fx.issueComments = nil }, wantSeen: 1, wantSkipped: 1},
		{name: "per-PR error is skipped not fatal", mutate: func(fx *contributorLaneFixture) { fx.issueCommentsSt = http.StatusInternalServerError }, wantSeen: 1, wantSkipped: 1},
		{name: "eligible merges and audits", wantSeen: 1, wantMerged: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			opts := tc.opts
			opts.ContributorSoak = soakedTracker(clock)
			engine := newContributorLaneEngine(api.URL, policy, clock, opts)
			audited := 0
			result, err := engine.SweepContributorLaneAutoMerges(context.Background(), AutoMergeSweepOptions{Audit: func(AutoMergeSweepEvent) { audited++ }})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if result.Seen != tc.wantSeen || result.Skipped != tc.wantSkipped || len(result.Merged) != tc.wantMerged {
				t.Fatalf("seen=%d skipped=%d merged=%d, want %d/%d/%d", result.Seen, result.Skipped, len(result.Merged), tc.wantSeen, tc.wantSkipped, tc.wantMerged)
			}
			if audited != tc.wantMerged || fx.merges != tc.wantMerged {
				t.Fatalf("audited=%d merges=%d, want %d", audited, fx.merges, tc.wantMerged)
			}
		})
	}
}

func TestSweepContributorLaneAutoMergesPackageFuncAndNoClient(t *testing.T) {
	var nilEngine *Engine
	if _, err := nilEngine.SweepContributorLaneAutoMerges(context.Background(), AutoMergeSweepOptions{}); err == nil {
		t.Fatalf("nil engine should fail")
	}
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	policy := defaultContributorLanePolicy()
	opts := Options{
		ContributorLanePolicy:  func() ContributorLanePolicy { return policy },
		ContributorLaneVerdict: approvedVerdict,
		ContributorSoak:        soakedTracker(clock),
		Now:                    clock.now,
	}
	result, err := SweepContributorLaneAutoMerges(context.Background(), client, opts, AutoMergeSweepOptions{})
	if err != nil || len(result.Merged) != 1 {
		t.Fatalf("package func: err=%v result=%+v", err, result)
	}
}

func TestContributorLaneClaimRequiresAppBotLogin(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	policy := defaultContributorLanePolicy()
	engine := New(client, Options{ContributorLanePolicy: func() ContributorLanePolicy { return policy }})
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonNoAppBotLogin {
		t.Fatalf("reason=%q err=%v, want %q", reason, err, contributorLaneReasonNoAppBotLogin)
	}
}

func TestContributorLaneLinkedIssues(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
	}{
		{body: "Fixes #42", want: []int{42}},
		{body: "Closes #9 and fixes #3", want: []int{3, 9}},
		{body: "Resolves acme/widget#5", want: []int{5}},
		{body: "Fixes other/repo#5", want: nil},
		{body: "Refs #42", want: nil},
		{body: "Fixes #42\nFixes #42", want: []int{42}},
		{body: "", want: nil},
	} {
		got := contributorLaneLinkedIssues(tc.body, "acme/widget")
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %v want %v", tc.body, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: got %v want %v", tc.body, got, tc.want)
			}
		}
	}
}

func TestContributorLanePolicyHelpers(t *testing.T) {
	p := ContributorLanePolicy{Enabled: true, Repos: map[string]bool{"widget": true}}
	if !p.repoAllowed("widget") || !p.repoAllowed(" Widget ") || p.repoAllowed("other") {
		t.Fatalf("repoAllowed mismatch")
	}
	if !(ContributorLanePolicy{Enabled: true}).repoAllowed("anything") {
		t.Fatalf("empty allow-list should allow all")
	}
	if (ContributorLanePolicy{}).repoAllowed("anything") {
		t.Fatalf("disabled policy should allow nothing")
	}
	p.ExcludeLabels = map[string]bool{"blocked": true}
	if got := p.excludedLabel([]string{"ok", " Blocked "}); got != "blocked" {
		t.Fatalf("excludedLabel = %q", got)
	}
	if got := p.excludedLabel([]string{"ok"}); got != "" {
		t.Fatalf("excludedLabel = %q, want empty", got)
	}
	if got := contributorLaneSoakReason(90*time.Minute + 20*time.Second); got != "contributor-lane:soaking(remaining=1h30m0s)" {
		t.Fatalf("soak reason = %q", got)
	}
	var nilEngine *Engine
	if nilEngine.currentContributorLanePolicy().Enabled {
		t.Fatalf("nil engine policy should be disabled")
	}
	if nilEngine.contributorLaneVerdictFor("acme/widget", 1, "sha").Configured {
		t.Fatalf("nil engine verdict should be unconfigured")
	}
}

func TestPrefilterContributorLanePR(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	client.SetAppBotLogin(testHiveAppBotLogin)
	client.SetExemptLabels([]string{"lfx"})
	policy := defaultContributorLanePolicy()
	engine := New(client, Options{ContributorLanePolicy: func() ContributorLanePolicy { return policy }})
	pr := func(mutate func(p *gh.PullRequest)) *gh.PullRequest {
		p := &gh.PullRequest{
			Number: gh.Ptr(7),
			State:  gh.Ptr("open"),
			User:   &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)},
			Head:   &gh.PullRequestBranch{SHA: gh.Ptr("sha7")},
		}
		if mutate != nil {
			mutate(p)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		want string
	}{
		{name: "nil", pr: nil, want: "missing-head-sha"},
		{name: "eligible", pr: pr(nil), want: ""},
		{name: "closed", pr: pr(func(p *gh.PullRequest) { p.State = gh.Ptr("closed") }), want: "closed"},
		{name: "no author", pr: pr(func(p *gh.PullRequest) { p.User = nil }), want: contributorLaneReasonNoAuthor},
		{name: "exempt label", pr: pr(func(p *gh.PullRequest) { p.Labels = []*gh.Label{{Name: gh.Ptr("lfx")}} }), want: "exempt-label"},
		{name: "needs-rebase", pr: pr(func(p *gh.PullRequest) { p.Labels = []*gh.Label{{Name: gh.Ptr("needs-rebase")}} }), want: "label:needs-rebase"},
		{name: "missing head", pr: pr(func(p *gh.PullRequest) { p.Head = nil }), want: "missing-head-sha"},
	} {
		if got := engine.prefilterContributorLanePR(tc.pr, policy); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
