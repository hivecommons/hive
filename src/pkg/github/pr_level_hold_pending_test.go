package github

// Coverage for the ACMM-level-change sweep-restart path added in
// pr_level_hold.go: PendingLevelHolds and ReleaseLevelHoldsOnce are the entry
// points a runtime level change calls into, and several of their fail-closed
// and default-value branches were not yet pinned by a test.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingLevelHoldsNilClientFailsClosed(t *testing.T) {
	pending, err := (*Client)(nil).PendingLevelHolds(context.Background())
	if pending != nil || err != ErrNoGitHubClient {
		t.Fatalf("PendingLevelHolds(nil) = (%v, %v), want (nil, ErrNoGitHubClient)", pending, err)
	}
}

func TestReleaseLevelHoldsOnceNilClientFailsClosed(t *testing.T) {
	released, err := (*Client)(nil).ReleaseLevelHoldsOnce(context.Background(), 6, "owner")
	if released != nil || err != ErrNoGitHubClient {
		t.Fatalf("ReleaseLevelHoldsOnce(nil) = (%v, %v), want (nil, ErrNoGitHubClient)", released, err)
	}
}

func TestPendingLevelHoldsSurfacesPullRequestListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	if _, err := c.PendingLevelHolds(context.Background()); err == nil {
		t.Fatal("want pull-request listing failure surfaced, got nil")
	}
}

func TestReleaseLevelHoldsOnceDefaultsActorWhenBlank(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}}
	c := newLevelHoldClient(t, s)
	c.SetRepos([]string{"acme/widget"})

	released, err := c.ReleaseLevelHoldsOnce(context.Background(), 4, "   ")
	if err != nil {
		t.Fatalf("ReleaseLevelHoldsOnce: %v", err)
	}
	if len(released) != 1 {
		t.Fatalf("released = %+v, want one entry", released)
	}
	if got := s.comments[len(s.comments)-1]; !strings.Contains(got, "hold released by operator unknown operator when raising to L4") {
		t.Fatalf("release comment = %q, want default operator name", got)
	}
}

func TestReleaseLevelHoldsOnceSurfacesLabelRemovalError(t *testing.T) {
	// levelHoldServer's routes are fixed, so drive the label-removal failure
	// through a purpose-built routed fake instead.
	routed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   "safe change",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"body": levelHoldNotice("quality"),
				"user": map[string]string{"login": testHiveAppBotLogin},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"label":      map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(routed.Close)
	c := NewClientForTest(routed.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	if _, err := c.ReleaseLevelHoldsOnce(context.Background(), 6, "owner"); err == nil {
		t.Fatal("want label-removal failure surfaced, got nil")
	}
}

func TestReleaseLevelHoldsOnceSurfacesCommentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   "safe change",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"body": levelHoldNotice("quality"),
				"user": map[string]string{"login": testHiveAppBotLogin},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"label":      map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	if _, err := c.ReleaseLevelHoldsOnce(context.Background(), 6, "owner"); err == nil {
		t.Fatal("want release-comment failure surfaced, got nil")
	}
}

func TestPendingLevelHoldsSkipsWhenSelfAuthorizationNoticeAlreadyPosted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   "safe change",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"body": levelHoldNotice("quality"),
					"user": map[string]string{"login": testHiveAppBotLogin},
				},
				{
					"body": "hivecommons/hive#5117 self-authorization hold notice",
					"user": map[string]string{"login": testHiveAppBotLogin},
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	pending, err := c.PendingLevelHolds(context.Background())
	if err != nil {
		t.Fatalf("PendingLevelHolds: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none when a self-authorization notice already exists", pending)
	}
}

func TestPendingLevelHoldsBlockedBySelfAuthorizationHoldActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   "Closes #581",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"body": levelHoldNotice("quality"),
				"user": map[string]string{"login": testHiveAppBotLogin},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/581":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   581,
				"user":     userJSON("some-agent", "Bot"),
				"labels":   []map[string]any{},
				"comments": 0,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), SelfAuthorizationNoticeMarker) {
				t.Fatalf("expected self-authorization notice, got %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})
	c.SetSelfAuthorizationHoldEnabled(func(string) bool { return true })

	pending, err := c.PendingLevelHolds(context.Background())
	if err != nil {
		t.Fatalf("PendingLevelHolds: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none while a self-authorization hold is active", pending)
	}
}

func TestPendingLevelHoldsSurfacesSelfAuthorizationNoticeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   "Closes #581",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"body": levelHoldNotice("quality"),
				"user": map[string]string{"login": testHiveAppBotLogin},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/581":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   581,
				"user":     userJSON("some-agent", "Bot"),
				"labels":   []map[string]any{},
				"comments": 0,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})
	c.SetSelfAuthorizationHoldEnabled(func(string) bool { return true })

	if _, err := c.PendingLevelHolds(context.Background()); err == nil {
		t.Fatal("want self-authorization notice failure surfaced, got nil")
	}
}
