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
// config-first / branch-protection-second resolution of the required-check
// set that decides which CI contexts gate a merge.
func TestRequiredStatusCheckContexts(t *testing.T) {
	ctx := context.Background()

	t.Run("config-known set wins without any API call", func(t *testing.T) {
		cfg := map[string]bool{"build": true}
		set, known := RequiredStatusCheckContexts(ctx, nil, "o", "r", "main", cfg, true)
		if !known || !reflect.DeepEqual(set, cfg) {
			t.Fatalf("configKnown: got (%v,%v), want (%v,true)", set, known, cfg)
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
