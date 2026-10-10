package github

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Covers ActiveRepositories (client.go), previously 0%: the pause-aware
// transport capability the automerge sweep consumes (#6203).
func TestActiveRepositoriesFiltersPausedRepos(t *testing.T) {
	var nilClient *Client
	if got := nilClient.ActiveRepositories(); got != nil {
		t.Fatalf("nil client ActiveRepositories = %v, want nil", got)
	}

	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"o/a", "o/b", "o/c"}, nil)
	if got := c.ActiveRepositories(); !reflect.DeepEqual(got, []string{"o/a", "o/b", "o/c"}) {
		t.Fatalf("no pause predicate: ActiveRepositories = %v, want full configured list", got)
	}

	c.SetRepoPausedFunc(func(repo string) bool { return repo == "o/b" })
	if got := c.ActiveRepositories(); !reflect.DeepEqual(got, []string{"o/a", "o/c"}) {
		t.Fatalf("paused o/b: ActiveRepositories = %v, want [o/a o/c]", got)
	}

	c.SetRepoPausedFunc(func(string) bool { return true })
	if got := c.ActiveRepositories(); len(got) != 0 {
		t.Fatalf("all paused: ActiveRepositories = %v, want empty", got)
	}

	// Repositories() must stay unfiltered: it is the configured list, not
	// the actionable one.
	if got := c.Repositories(); !reflect.DeepEqual(got, []string{"o/a", "o/b", "o/c"}) {
		t.Fatalf("Repositories = %v, want unfiltered configured list", got)
	}
}

// Covers refreshRateLimitCache (automerge_sweep.go), previously 0%: the
// GET /rate_limit cache correction issued after a pre-emptive go-github
// rate-limit refusal.
func TestRefreshRateLimitCache(t *testing.T) {
	// Nil receiver and nil inner client are both no-ops.
	var nilClient *Client
	nilClient.refreshRateLimitCache(context.Background())
	(&Client{}).refreshRateLimitCache(context.Background())

	t.Run("success refreshes and logs info", func(t *testing.T) {
		var rateLimitCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rate_limit") {
				rateLimitCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"resources":{"core":{"limit":6900,"remaining":6613,"reset":1}}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, logger)
		c.refreshRateLimitCache(context.Background())

		if rateLimitCalls != 1 {
			t.Fatalf("rate_limit endpoint called %d times, want 1", rateLimitCalls)
		}
		if !strings.Contains(buf.String(), "refreshed rate-limit cache") {
			t.Fatalf("expected refresh info log, got: %s", buf.String())
		}
	})

	t.Run("API error logs warning and does not panic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, logger)
		c.refreshRateLimitCache(context.Background())

		if !strings.Contains(buf.String(), "could not refresh rate-limit cache") {
			t.Fatalf("expected refresh-failure warning, got: %s", buf.String())
		}
	})
}

// Covers maybeDowngradeNoCIVerdict (merge_request_policy.go), previously
// 33.3%: the auto_merge.no_ci_ok opt-in that turns an unverified CI verdict
// green for explicitly allowed repos, and nothing else.
func TestMaybeDowngradeNoCIVerdict(t *testing.T) {
	req := MergeRequest{Repo: "o/r", Number: 7, Agent: "quality"}

	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"o/r"}, nil)

	// Non-unverified verdicts pass through untouched even with the opt-in.
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})
	for _, v := range []mergeCIVerdict{mergeCIGreen, mergeCIRed, mergeCIPending} {
		got, why := c.maybeDowngradeNoCIVerdict(req, v, "orig")
		if got != v || why != "orig" {
			t.Fatalf("verdict %v: got (%v,%q), want unchanged (%v,%q)", v, got, why, v, "orig")
		}
	}

	// Unverified without the opt-in stays unverified.
	c.SetMergeRequestNoCIAllowedRepos(nil)
	got, why := c.maybeDowngradeNoCIVerdict(req, mergeCIUnverified, "no CI evidence")
	if got != mergeCIUnverified || why != "no CI evidence" {
		t.Fatalf("no opt-in: got (%v,%q), want unchanged unverified", got, why)
	}

	// Unverified with the opt-in downgrades to green, appends the reason,
	// and logs the acceptance.
	var buf bytes.Buffer
	logged := NewClientForTest("http://127.0.0.1:0", "o", []string{"o/r"},
		slog.New(slog.NewTextHandler(&buf, nil)))
	logged.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})
	got, why = logged.maybeDowngradeNoCIVerdict(req, mergeCIUnverified, "no CI evidence")
	if got != mergeCIGreen {
		t.Fatalf("opt-in verdict = %v, want mergeCIGreen", got)
	}
	if !strings.HasPrefix(why, "no CI evidence; ") || !strings.Contains(why, "auto_merge.no_ci_ok") {
		t.Fatalf("opt-in reason = %q, want original reason plus no_ci_ok note", why)
	}
	if !strings.Contains(buf.String(), "no-CI repo opt-in accepted") {
		t.Fatalf("expected opt-in acceptance log, got: %s", buf.String())
	}
}

// Covers RequiredStatusCheckContexts (commit_ci.go), previously 38.1%: the
// branch-protection-first / config-fallback resolution of the required-check
// set that decides which CI contexts gate a merge.
func TestRequiredStatusCheckContexts(t *testing.T) {
	ctx := context.Background()

	t.Run("config-known set is fallback when API cannot be called", func(t *testing.T) {
		cfg := map[string]bool{"build": true}
		set, known := RequiredStatusCheckContexts(ctx, nil, "o", "r", "main", cfg, true)
		if !known || !reflect.DeepEqual(set, cfg) {
			t.Fatalf("configKnown: got (%v,%v), want (%v,true)", set, known, cfg)
		}
	})

	t.Run("branch protection wins over config", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"strict":true,"contexts":["protected"]}`)
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", map[string]bool{"config": true}, true)
		want := map[string]bool{"protected": true}
		if !known || !reflect.DeepEqual(set, want) {
			t.Fatalf("protected branch: got (%v,%v), want (%v,true)", set, known, want)
		}
	})

	t.Run("nil client or blank branch is unknown", func(t *testing.T) {
		if set, known := RequiredStatusCheckContexts(ctx, nil, "o", "r", "main", nil, false); known || set != nil {
			t.Fatalf("nil client: got (%v,%v), want (nil,false)", set, known)
		}
		c := NewClientForTest("http://127.0.0.1:0", "o", []string{"o/r"}, nil)
		if set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "  ", nil, false); known || set != nil {
			t.Fatalf("blank branch: got (%v,%v), want (nil,false)", set, known)
		}
	})

	t.Run("unprotected branch is a known empty set", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			// go-github maps this exact message to gh.ErrBranchNotProtected.
			_, _ = io.WriteString(w, `{"message":"Branch not protected"}`)
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false)
		if !known || len(set) != 0 || set == nil {
			t.Fatalf("unprotected branch: got (%v,%v), want (empty map, true)", set, known)
		}
	})

	t.Run("other API errors leave the set unknown", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		if set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false); known || set != nil {
			t.Fatalf("API error: got (%v,%v), want (nil,false)", set, known)
		}
	})

	t.Run("forbidden protection falls back to branch payload", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/branches/main":
				_, _ = io.WriteString(w, `{"protected":true,"protection":{"required_status_checks":{"contexts":["branch-status"],"checks":[{"context":"branch-check"}]}}}`)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known, fromConfig, fallback, source := RequiredStatusCheckContextsDetailedWithSource(ctx, c.client, "o", "r", "main", nil, false)
		want := map[string]bool{"branch-status": true, "branch-check": true}
		if !known || fromConfig || fallback || source != "branch" || !reflect.DeepEqual(set, want) {
			t.Fatalf("branch fallback: got set=%v known=%v fromConfig=%v fallback=%v source=%q, want %v true false false branch", set, known, fromConfig, fallback, source, want)
		}
	})

	t.Run("forbidden protection and branch falls back to rules endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/branches/main":
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/rules/branches/main":
				_, _ = io.WriteString(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"rules-check"}]}}]`)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known, fromConfig, fallback, source := RequiredStatusCheckContextsDetailedWithSource(ctx, c.client, "o", "r", "main", nil, false)
		want := map[string]bool{"rules-check": true}
		if !known || fromConfig || fallback || source != "rules" || !reflect.DeepEqual(set, want) {
			t.Fatalf("rules fallback: got set=%v known=%v fromConfig=%v fallback=%v source=%q, want %v true false false rules", set, known, fromConfig, fallback, source, want)
		}
	})

	t.Run("all API sources unavailable leaves source none", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/branches/main":
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/rules/branches/main":
				_, _ = io.WriteString(w, `[]`)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known, fromConfig, fallback, source := RequiredStatusCheckContextsDetailedWithSource(ctx, c.client, "o", "r", "main", nil, false)
		if known || set != nil || fromConfig || fallback || source != "none" {
			t.Fatalf("unavailable sources: got set=%v known=%v fromConfig=%v fallback=%v source=%q, want nil false false false none", set, known, fromConfig, fallback, source)
		}
	})

	t.Run("forbidden branch protection lookup is negative cached", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		restore := resetRequiredChecksForbiddenCacheForTest(func() time.Time { return now }, time.Hour)
		defer restore()

		protectionCalls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
				protectionCalls++
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/branches/main":
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			case r.URL.Path == "/repos/o/r/rules/branches/main":
				_, _ = io.WriteString(w, `[]`)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		if set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false); known || set != nil {
			t.Fatalf("first 403: got (%v,%v), want (nil,false)", set, known)
		}
		if set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false); known || set != nil {
			t.Fatalf("cached 403: got (%v,%v), want (nil,false)", set, known)
		}
		if protectionCalls != 1 {
			t.Fatalf("forbidden protection lookup calls = %d, want 1", protectionCalls)
		}

		now = now.Add(time.Hour + time.Second)
		RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false)
		if protectionCalls != 2 {
			t.Fatalf("expired forbidden protection lookup calls = %d, want 2", protectionCalls)
		}
	})

	t.Run("transient errors are not negative cached", func(t *testing.T) {
		restore := resetRequiredChecksForbiddenCacheForTest(time.Now, time.Hour)
		defer restore()

		protectionCalls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
				protectionCalls++
				w.WriteHeader(http.StatusInternalServerError)
			case r.URL.Path == "/repos/o/r/branches/main":
				w.WriteHeader(http.StatusInternalServerError)
			case r.URL.Path == "/repos/o/r/rules/branches/main":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false)
		RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false)
		if protectionCalls != 2 {
			t.Fatalf("transient protection lookup calls = %d, want 2", protectionCalls)
		}
	})

	t.Run("contexts and checks merge, nil check entries skipped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/protection/required_status_checks") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"strict":true,"contexts":["ctx-a"],"checks":[{"context":"check-b","app_id":1},null]}`)
		}))
		defer srv.Close()

		c := NewClientForTest(srv.URL, "o", []string{"o/r"}, nil)
		set, known := RequiredStatusCheckContexts(ctx, c.client, "o", "r", "main", nil, false)
		want := map[string]bool{"ctx-a": true, "check-b": true}
		if !known || !reflect.DeepEqual(set, want) {
			t.Fatalf("protected branch: got (%v,%v), want (%v,true)", set, known, want)
		}
	})
}
