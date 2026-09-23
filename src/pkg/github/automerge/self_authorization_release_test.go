package automerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// stubTransport satisfies Transport but deliberately NOT levelHoldTransport,
// so releaseLevelHoldIfEligible must treat it as "no level-hold support".
type stubTransport struct{}

func (stubTransport) GoGitHub() *gh.Client              { return gh.NewClient(nil) }
func (stubTransport) Repositories() []string            { return nil }
func (stubTransport) SplitRepo(string) (string, string) { return "acme", "widget" }
func (stubTransport) AutoMergeLabel() string            { return "automerge" }
func (stubTransport) AppBotLogin() string               { return testHiveAppBotLogin }
func (stubTransport) IsExemptLabels([]string) bool      { return false }
func (stubTransport) UpdateBranch(context.Context, string, int) error {
	return nil
}
func (stubTransport) RecordPRMergedAudit(string, int, string, string) {}

func TestSelfAuthorizationReleaseBudget(t *testing.T) {
	var nilEngine *Engine
	if got := nilEngine.selfAuthorizationReleaseBudget(); got != 0 {
		t.Fatalf("nil engine budget = %d, want 0", got)
	}
	if got := (&Engine{}).selfAuthorizationReleaseBudget(); got != 0 {
		t.Fatalf("unconfigured engine budget = %d, want 0 (hold switch not wired)", got)
	}
	enabled := func(string) bool { return true }
	c := &Engine{selfAuthorizationHoldEnabled: enabled}
	if got := c.selfAuthorizationReleaseBudget(); got != defaultSelfAuthorizationReleaseLimit {
		t.Fatalf("default budget = %d, want %d", got, defaultSelfAuthorizationReleaseLimit)
	}
	c.selfAuthorizationHoldReleaseLimit = 3
	if got := c.selfAuthorizationReleaseBudget(); got != 3 {
		t.Fatalf("configured budget = %d, want 3", got)
	}
}

func TestSelfAuthorizationHoldActiveDefaultsOn(t *testing.T) {
	var nilEngine *Engine
	if !nilEngine.selfAuthorizationHoldActive("acme/widget") {
		t.Fatal("nil engine must default the #5117 hold to active")
	}
	if !(&Engine{}).selfAuthorizationHoldActive("acme/widget") {
		t.Fatal("engine without a hold switch must default the #5117 hold to active")
	}
	c := &Engine{selfAuthorizationHoldEnabled: func(repo string) bool { return repo == "acme/other" }}
	if c.selfAuthorizationHoldActive("acme/widget") {
		t.Fatal("hold switch returning false must deactivate the hold")
	}
}

func TestReleaseSelfAuthorizationHoldNilGuards(t *testing.T) {
	var nilEngine *Engine
	released, err := nilEngine.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 1)
	if released || err != nil {
		t.Fatalf("nil engine = (%v, %v), want no-op", released, err)
	}
	released, err = (&Engine{}).releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 1)
	if released || err != nil {
		t.Fatalf("engine without transport = (%v, %v), want no-op", released, err)
	}
}

// releaseEngine builds an Engine over an httptest server whose handler owns
// every route the release path touches.
func releaseEngine(t *testing.T, handler http.HandlerFunc) (*Engine, *httptest.Server) {
	t.Helper()
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	return New(client, Options{}), api
}

func selfAuthNotice(createdAt string) map[string]any {
	return map[string]any{
		"body":       hgithub.SelfAuthorizationNoticeMarker + "\nheld",
		"user":       map[string]string{"login": testHiveAppBotLogin},
		"created_at": createdAt,
	}
}

func TestReleaseSelfAuthorizationHoldListCommentsError(t *testing.T) {
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if err == nil || !strings.Contains(err.Error(), "listing comments") {
		t.Fatalf("err = %v, want listing-comments failure to propagate", err)
	}
}

func TestReleaseSelfAuthorizationHoldListEventsError(t *testing.T) {
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/issues/11/comments":
			json.NewEncoder(w).Encode([]map[string]any{selfAuthNotice("2026-09-11T12:00:00Z")})
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	})
	_, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if err == nil || !strings.Contains(err.Error(), "listing issue events") {
		t.Fatalf("err = %v, want listing-issue-events failure to propagate", err)
	}
}

func selfAuthHoldEvents() []map[string]any {
	return []map[string]any{{
		"event":      "labeled",
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": testHiveAppBotLogin},
		"created_at": "2026-09-11T11:59:59Z",
	}}
}

func TestReleaseSelfAuthorizationHoldRemoveLabelError(t *testing.T) {
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			json.NewEncoder(w).Encode([]map[string]any{selfAuthNotice("2026-09-11T12:00:00Z")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			json.NewEncoder(w).Encode(selfAuthHoldEvents())
		case r.Method == http.MethodDelete:
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	released, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if released || err == nil || !strings.Contains(err.Error(), "removing hold label") {
		t.Fatalf("(%v, %v), want removing-hold-label failure and no release", released, err)
	}
}

func TestReleaseSelfAuthorizationHoldTolerates404LabelButPropagatesCommentError(t *testing.T) {
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			json.NewEncoder(w).Encode([]map[string]any{selfAuthNotice("2026-09-11T12:00:00Z")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			json.NewEncoder(w).Encode(selfAuthHoldEvents())
		case r.Method == http.MethodDelete:
			// Label already gone: must be tolerated, not treated as failure.
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	released, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if released || err == nil || !strings.Contains(err.Error(), "posting release comment") {
		t.Fatalf("(%v, %v), want posting-release-comment failure after tolerated 404", released, err)
	}
}

func TestReleaseSelfAuthorizationHoldPaginatesCommentsAndEvents(t *testing.T) {
	var removed, commented bool
	c, api := releaseEngine(t, nil)
	api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			if page == "" || page == "1" {
				// Page 1 carries only skippable comments: a null entry, a
				// human comment, and a bot comment that is not the notice.
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widget/issues/11/comments?page=2>; rel="next"`, api.URL))
				fmt.Fprint(w, `[null,
					{"body":"hold please","user":{"login":"clubanderson"}},
					{"body":"routine status","user":{"login":"`+testHiveAppBotLogin+`"}}]`)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{selfAuthNotice("2026-09-11T12:00:00Z")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			if page == "" || page == "1" {
				// Page 1 carries only skippable events: a null entry, a
				// non-label event, a non-hold label, and a hold label on an
				// event type that is neither labeled nor unlabeled.
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widget/issues/11/events?page=2>; rel="next"`, api.URL))
				fmt.Fprint(w, `[null,
					{"event":"assigned","created_at":"2026-09-11T11:00:00Z"},
					{"event":"labeled","label":{"name":"quality"},"created_at":"2026-09-11T11:00:01Z"},
					{"event":"reopened","label":{"name":"hold"},"created_at":"2026-09-11T11:00:02Z"}]`)
				return
			}
			json.NewEncoder(w).Encode(selfAuthHoldEvents())
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			removed = true
			json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			commented = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			http.NotFound(w, r)
		}
	})
	released, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if err != nil {
		t.Fatalf("release returned error: %v", err)
	}
	if !released || !removed || !commented {
		t.Fatalf("released=%v removed=%v commented=%v, want release after paginating past skippable pages", released, removed, commented)
	}
}

func TestReleaseSelfAuthorizationHoldSkipsWhenLatestHoldEventIsUnlabeled(t *testing.T) {
	var removed bool
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			json.NewEncoder(w).Encode([]map[string]any{selfAuthNotice("2026-09-11T12:00:00Z")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			// Hold was applied, then removed BEFORE the notice. The latest
			// hold event is an unlabel, so no current bot hold exists and
			// nothing must be released.
			json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"label":      map[string]string{"name": "hold"},
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"created_at": "2026-09-11T11:00:00Z",
			}, {
				"event":      "unlabeled",
				"label":      map[string]string{"name": "hold"},
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"created_at": "2026-09-11T11:30:00Z",
			}})
		case r.Method == http.MethodDelete:
			removed = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	released, err := c.releaseSelfAuthorizationHoldIfEligible(context.Background(), "acme/widget", "acme", "widget", 11)
	if err != nil {
		t.Fatalf("release returned error: %v", err)
	}
	if released || removed {
		t.Fatalf("released=%v removed=%v, want no release when the bot hold was already lifted", released, removed)
	}
}

func TestReleaseLevelHoldIfEligibleGuards(t *testing.T) {
	var nilEngine *Engine
	released, reason, err := nilEngine.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", &gh.PullRequest{})
	if released || reason != "" || err != nil {
		t.Fatalf("nil engine = (%v, %q, %v), want no-op", released, reason, err)
	}
	released, reason, err = (&Engine{}).releaseLevelHoldIfEligible(context.Background(), "acme", "widget", &gh.PullRequest{})
	if released || reason != "" || err != nil {
		t.Fatalf("engine without transport = (%v, %q, %v), want no-op", released, reason, err)
	}
	c := New(stubTransport{}, Options{})
	released, reason, err = c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", &gh.PullRequest{})
	if released || reason != "" || err != nil {
		t.Fatalf("transport without level-hold support = (%v, %q, %v), want no-op", released, reason, err)
	}
}
