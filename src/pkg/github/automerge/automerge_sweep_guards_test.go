package automerge

// Tests for the uncovered guard branches of the self-authored auto-merge
// sweep path (trySweepSelfAuthoredPR), the rate-limit cache refresh, the
// queue-approval trust check (isHiveAppReviewAuthor), and required-checks
// resolution (requiredStatusCheckContexts). These are merge-safety branches:
// each one is a reason the sweep must NOT squash a PR, so each deserves a
// pinned regression test.

import (
	"context"
	"encoding/json"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// selfSweepFixture drives a one-PR httptest GitHub API for direct
// trySweepSelfAuthoredPR calls. Zero values produce a green, mergeable,
// App-authored open PR that merges cleanly; each field overrides one branch.
type selfSweepFixture struct {
	firstGetStatus   int    // non-zero: first PR fetch returns this HTTP status
	state            string // PR state; default "open"
	draft            bool
	labels           []string
	author           string // default testHiveAppBotLogin
	headSHA          *string
	mergeableState   string // default "clean"
	statusState      string // combined-status state; default "success"
	statusHTTPCode   int    // non-zero: status endpoint returns this HTTP status
	recheckGetStatus int    // non-zero: second PR fetch returns this HTTP status
	recheckHeadSHA   *string
	mergeHTTPCode    int  // non-zero: merge endpoint returns this HTTP status
	mergeApplied     bool // "merged" field in merge response; default true via fixture setup
}

func newSelfSweepGuardAPI(t *testing.T, fx selfSweepFixture) *httptest.Server {
	t.Helper()
	if fx.state == "" {
		fx.state = "open"
	}
	if fx.author == "" {
		fx.author = testHiveAppBotLogin
	}
	if fx.mergeableState == "" {
		fx.mergeableState = "clean"
	}
	if fx.statusState == "" {
		fx.statusState = "success"
	}
	sha := "sha7"
	if fx.headSHA == nil {
		fx.headSHA = &sha
	}
	if fx.recheckHeadSHA == nil {
		fx.recheckHeadSHA = fx.headSHA
	}
	fetches := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			fetches++
			if fetches == 1 && fx.firstGetStatus != 0 {
				w.WriteHeader(fx.firstGetStatus)
				return
			}
			if fetches > 1 && fx.recheckGetStatus != 0 {
				w.WriteHeader(fx.recheckGetStatus)
				return
			}
			head := *fx.headSHA
			if fetches > 1 {
				head = *fx.recheckHeadSHA
			}
			var prLabels []map[string]string
			for _, name := range fx.labels {
				prLabels = append(prLabels, map[string]string{"name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number":          7,
				"state":           fx.state,
				"draft":           fx.draft,
				"labels":          prLabels,
				"mergeable_state": fx.mergeableState,
				"mergeable":       fx.mergeableState == "clean",
				"user":            map[string]string{"login": fx.author},
				"head":            map[string]string{"sha": head},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
			if fx.statusHTTPCode != 0 {
				w.WriteHeader(fx.statusHTTPCode)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"state":       fx.statusState,
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": fx.statusState}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []map[string]string{}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			if fx.mergeHTTPCode != 0 {
				w.WriteHeader(fx.mergeHTTPCode)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"merged": fx.mergeApplied, "sha": "merge7"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

func TestTrySweepSelfAuthoredPRGuardBranches(t *testing.T) {
	tests := []struct {
		name       string
		fx         selfSweepFixture
		wantReason string
		wantErr    bool
	}{
		{name: "PR deleted between list and eval", fx: selfSweepFixture{firstGetStatus: http.StatusNotFound}, wantReason: "gone"},
		{name: "PR fetch API error propagates", fx: selfSweepFixture{firstGetStatus: http.StatusInternalServerError}, wantReason: "fetch-pr", wantErr: true},
		{name: "closed PR is never merged", fx: selfSweepFixture{state: "closed"}, wantReason: "closed"},
		{name: "draft PR is never merged", fx: selfSweepFixture{draft: true}, wantReason: "draft"},
		{name: "authorship changed between list and eval", fx: selfSweepFixture{author: "mallory"}, wantReason: "not-app-authored"},
		// #5589: a hold label — including one the hold guard re-applied
		// because the branch moved while hold-gated — must stop the App from
		// squashing its own PR on green. This path lists PRs independently of
		// the enumeration hold gate, so it needs its own check.
		{name: "hold label blocks self-merge", fx: selfSweepFixture{labels: []string{"hold"}}, wantReason: "held"},
		{name: "hold-review label blocks self-merge", fx: selfSweepFixture{labels: []string{"hold/review"}}, wantReason: "held"},
		{name: "do-not-merge label blocks self-merge", fx: selfSweepFixture{labels: []string{"do-not-merge"}}, wantReason: "exempt-label"},
		{name: "missing head SHA blocks merge", fx: selfSweepFixture{headSHA: gh.Ptr("")}, wantReason: "missing-head-sha"},
		{name: "not mergeable blocks merge", fx: selfSweepFixture{mergeableState: "dirty"}, wantReason: "not-mergeable"},
		{name: "commitGreen API error propagates", fx: selfSweepFixture{statusHTTPCode: http.StatusInternalServerError}, wantReason: "status-check", wantErr: true},
		{name: "pending CI blocks merge", fx: selfSweepFixture{statusState: "pending"}, wantReason: "status-pending"},
		{name: "PR deleted between eval and merge", fx: selfSweepFixture{recheckGetStatus: http.StatusNotFound}, wantReason: "gone"},
		{name: "recheck API error propagates", fx: selfSweepFixture{recheckGetStatus: http.StatusInternalServerError}, wantReason: "fetch-pr-recheck", wantErr: true},
		{name: "head SHA vanished at recheck blocks merge", fx: selfSweepFixture{recheckHeadSHA: gh.Ptr("")}, wantReason: "head-changed-since-eval"},
		{name: "push landed between eval and merge blocks merge", fx: selfSweepFixture{recheckHeadSHA: gh.Ptr("newsha")}, wantReason: "head-changed-since-eval"},
		{name: "merge API error propagates", fx: selfSweepFixture{mergeHTTPCode: http.StatusConflict}, wantReason: "merge-failed", wantErr: true},
		{name: "merge accepted but not applied", fx: selfSweepFixture{mergeApplied: false}, wantReason: "merge-not-applied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if event.MergeSHA != "" {
				t.Fatalf("event = %+v, want no merge recorded on skip branch", event)
			}
		})
	}
}

func TestTrySweepSelfAuthoredPRMergesGreenPR(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want clean merge", reason, err)
	}
	if event.MergeSHA != "merge7" || event.HeadSHA != "sha7" || event.QueuedBy != "" {
		t.Fatalf("event = %+v, want merge7 at sha7 with no queuer", event)
	}
}

func TestRefreshRateLimitCache(t *testing.T) {
	t.Run("success refreshes the cached verdict", func(t *testing.T) {
		hits := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/rate_limit" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
			hits++
			json.NewEncoder(w).Encode(map[string]any{
				"resources": map[string]any{"core": map[string]int{"limit": 5000, "remaining": 4999, "reset": 0}},
			})
		}))
		defer api.Close()
		c := newAutoMergeSweepClient(api.URL)
		c.refreshRateLimitCache(context.Background())
		if hits != 1 {
			t.Fatalf("rate_limit endpoint hit %d times, want exactly 1", hits)
		}
	})

	t.Run("API error is swallowed with a warning", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer api.Close()
		c := newAutoMergeSweepClient(api.URL)
		c.refreshRateLimitCache(context.Background()) // must not panic or retry
	})
}

func TestIsHiveAppReviewAuthorGuards(t *testing.T) {
	review := &gh.PullRequestReview{User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}}

	var nilClient *Engine
	if nilClient.isHiveAppReviewAuthor(review) {
		t.Fatal("nil client must never trust a review author")
	}
	hc := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	hc.SetAppBotLogin(testHiveAppBotLogin)
	c := New(hc, Options{})
	if c.isHiveAppReviewAuthor(nil) {
		t.Fatal("nil review must never be trusted")
	}
	blank := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	blank.SetAppBotLogin("  ")
	if New(blank, Options{}).isHiveAppReviewAuthor(review) {
		t.Fatal("blank appBotLogin must never trust any review author")
	}
	if !c.isHiveAppReviewAuthor(review) {
		t.Fatal("App bot review author must be trusted")
	}
	forged := &gh.PullRequestReview{User: &gh.User{Login: gh.Ptr("mallory")}}
	if c.isHiveAppReviewAuthor(forged) {
		t.Fatal("non-App review author must never be trusted")
	}
}

func TestRequiredStatusCheckContextsBranches(t *testing.T) {
	t.Run("blank branch cannot resolve a required set", func(t *testing.T) {
		c := newAutoMergeSweepClient("http://127.0.0.1:0")
		if set, known := c.requiredStatusCheckContexts(context.Background(), "acme", "widget", "  "); known || set != nil {
			t.Fatalf("blank branch = (%v,%v), want unresolved", set, known)
		}
	})

	t.Run("unprotected branch is a known empty set", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "Branch not protected"})
		}))
		defer api.Close()
		c := newAutoMergeSweepClient(api.URL)
		set, known := c.requiredStatusCheckContexts(context.Background(), "acme", "widget", "main")
		if !known || len(set) != 0 {
			t.Fatalf("unprotected branch = (%v,%v), want known empty set", set, known)
		}
	})

	t.Run("API error means required set is unknown", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer api.Close()
		c := newAutoMergeSweepClient(api.URL)
		if set, known := c.requiredStatusCheckContexts(context.Background(), "acme", "widget", "main"); known || set != nil {
			t.Fatalf("API error = (%v,%v), want unresolved so caller fails closed", set, known)
		}
	})

	t.Run("contexts and checks both populate the required set", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"strict":   true,
				"contexts": []string{"build-gate"},
				"checks":   []map[string]any{{"context": "lint"}, nil},
			})
		}))
		defer api.Close()
		c := newAutoMergeSweepClient(api.URL)
		set, known := c.requiredStatusCheckContexts(context.Background(), "acme", "widget", "main")
		if !known || !set["build-gate"] || !set["lint"] || len(set) != 2 {
			t.Fatalf("required set = (%v,%v), want {build-gate,lint} known", set, known)
		}
	})
}
