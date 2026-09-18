package github

// Guard-branch tests for the level-hold release path (pr_level_hold.go).
//
// releaseLevelHoldIfEligible is the only code in the hive that REMOVES a
// `hold` label, so every fail-closed branch in it is merge-authorization
// logic: an API error, a missing policy, or a forged marker must all leave
// the hold in place. The happy paths are exercised end-to-end through the
// automerge sweep in pr_level_hold_test.go; these tests pin the guards
// directly, including comment/event pagination and 404 tolerance on release.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// newLevelHoldGuardClient starts a fake forge whose routes are supplied per
// test and returns a Client pointed at it with the app bot login set.
func newLevelHoldGuardClient(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h, ok := routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "o", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	return c
}

func heldPR(body string) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(7),
		Title:  gh.Ptr("fix"),
		Body:   gh.Ptr(body),
		Labels: []*gh.Label{{Name: gh.Ptr("hold")}},
	}
}

func botComments(bodies ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, len(bodies))
		for _, b := range bodies {
			out = append(out, map[string]any{"body": b, "user": map[string]string{"login": testHiveAppBotLogin}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func TestLevelHoldAgentFromNoticeRejectsMalformedMarkers(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantAgent string
		wantOK    bool
	}{
		{"no marker", "just a comment", "", false},
		{"malformed json", levelHoldNoticePrefix + `{"agent": -->`, "", false},
		{"empty agent", levelHoldNoticePrefix + `{"agent":""} -->`, "", false},
		{"missing suffix", levelHoldNoticePrefix + `{"agent":"quality"}`, "", false},
		{"valid after malformed line", levelHoldNoticePrefix + `{bad -->` + "\n" + levelHoldMarker("quality"), "quality", true},
		{"valid", levelHoldNotice("quality"), "quality", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, ok := levelHoldAgentFromNotice(tc.body)
			if agent != tc.wantAgent || ok != tc.wantOK {
				t.Fatalf("levelHoldAgentFromNotice(%q) = (%q, %v), want (%q, %v)", tc.body, agent, ok, tc.wantAgent, tc.wantOK)
			}
		})
	}
}

func TestIsTrustedLevelHoldNoticeAuthorGuards(t *testing.T) {
	comment := &gh.IssueComment{User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}}
	if (*Client)(nil).isTrustedLevelHoldNoticeAuthor(comment) {
		t.Fatal("nil client must not trust any author")
	}
	withLogin := &Client{appBotLogin: testHiveAppBotLogin}
	if withLogin.isTrustedLevelHoldNoticeAuthor(nil) {
		t.Fatal("nil comment must not be trusted")
	}
	if (&Client{appBotLogin: "  "}).isTrustedLevelHoldNoticeAuthor(comment) {
		t.Fatal("blank app bot login must fail closed: no author can be trusted")
	}
	if withLogin.isTrustedLevelHoldNoticeAuthor(&gh.IssueComment{User: &gh.User{Login: gh.Ptr("alice")}}) {
		t.Fatal("non-bot author must not be trusted")
	}
	upper := &gh.IssueComment{User: &gh.User{Login: gh.Ptr(strings.ToUpper(testHiveAppBotLogin))}}
	if !withLogin.isTrustedLevelHoldNoticeAuthor(upper) {
		t.Fatal("app bot login match must be case-insensitive")
	}
}

func TestLatestHoldLabelEventBlankLoginFailsClosed(t *testing.T) {
	ok, err := (&Client{appBotLogin: " "}).latestHoldLabelEventWasByApp(context.Background(), "o", "r", 7)
	if err != nil || ok {
		t.Fatalf("blank app bot login: got (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = (*Client)(nil).latestHoldLabelEventWasByApp(context.Background(), "o", "r", 7)
	if err != nil || ok {
		t.Fatalf("nil client: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestReleaseLevelHoldSkipsNilAndUnheldPRs(t *testing.T) {
	c := &Client{appBotLogin: testHiveAppBotLogin}
	if released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", nil); released || reason != "" || err != nil {
		t.Fatalf("nil PR: got (%v, %q, %v), want no-op", released, reason, err)
	}
	unheld := &gh.PullRequest{Number: gh.Ptr(7), Labels: []*gh.Label{{Name: gh.Ptr("quality")}}}
	if released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", unheld); released || reason != "" || err != nil {
		t.Fatalf("unheld PR: got (%v, %q, %v), want no-op", released, reason, err)
	}
}

func TestReleaseLevelHoldCommentListErrorFailsClosed(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if released || reason != "level-hold-comment-check" || err == nil {
		t.Fatalf("got (%v, %q, %v), want hold kept with comment-check error", released, reason, err)
	}
}

func TestReleaseLevelHoldWithoutPolicyFailsClosed(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
	})
	c.prHoldLabel = nil
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if released || reason != "level-hold-policy-unavailable" || err != nil {
		t.Fatalf("got (%v, %q, %v), want hold kept while policy is unavailable", released, reason, err)
	}
}

func TestReleaseLevelHoldEventCheckErrorFailsClosed(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
		"GET /repos/o/r/issues/7/events": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if released || reason != "level-hold-event-check" || err == nil {
		t.Fatalf("got (%v, %q, %v), want hold kept with event-check error", released, reason, err)
	}
}

// TestReleaseLevelHoldIgnoresUnrelatedEventsAcrossPages drives the event scan
// through nil entries, non-hold labels, non-label event kinds, and a second
// page whose newer app-applied `labeled` event must win over the older
// `unlabeled` one on page one.
func TestReleaseLevelHoldIgnoresUnrelatedEventsAcrossPages(t *testing.T) {
	removes := 0
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
		"GET /repos/o/r/issues/7/events": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"event":      "labeled",
					"created_at": "2026-09-16T12:00:00Z",
					"actor":      map[string]string{"login": testHiveAppBotLogin},
					"label":      map[string]string{"name": "hold"},
				}})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_ = json.NewEncoder(w).Encode([]any{
				nil, // a nil event must be skipped, not dereferenced
				map[string]any{ // different label: ignored
					"event":      "labeled",
					"created_at": "2026-09-17T12:00:00Z",
					"actor":      map[string]string{"login": "alice"},
					"label":      map[string]string{"name": "quality"},
				},
				map[string]any{ // hold label but not a label event kind: ignored
					"event":      "commented",
					"created_at": "2026-09-17T12:00:00Z",
					"actor":      map[string]string{"login": "alice"},
					"label":      map[string]string{"name": "hold"},
				},
				map[string]any{ // older unlabeled: superseded by page 2
					"event":      "unlabeled",
					"created_at": "2026-09-15T12:00:00Z",
					"actor":      map[string]string{"login": "alice"},
					"label":      map[string]string{"name": "hold"},
				},
			})
		},
		"DELETE /repos/o/r/issues/7/labels/hold": func(w http.ResponseWriter, _ *http.Request) {
			removes++
			w.WriteHeader(http.StatusOK)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if err != nil || !released || reason != "level-hold-released" {
		t.Fatalf("got (%v, %q, %v), want release after paginated event scan", released, reason, err)
	}
	if removes != 1 {
		t.Fatalf("removes=%d, want exactly one label removal", removes)
	}
}

func TestReleaseLevelHoldRemoveLabelErrorFailsClosed(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
		"GET /repos/o/r/issues/7/events": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"label":      map[string]string{"name": "hold"},
			}})
		},
		"DELETE /repos/o/r/issues/7/labels/hold": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if released || reason != "level-hold-release" || err == nil {
		t.Fatalf("got (%v, %q, %v), want failed removal reported as an error", released, reason, err)
	}
}

func TestReleaseLevelHoldToleratesLabelAlreadyGone(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
		"GET /repos/o/r/issues/7/events": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": testHiveAppBotLogin},
				"label":      map[string]string{"name": "hold"},
			}})
		},
		"DELETE /repos/o/r/issues/7/labels/hold": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("safe change"))
	if err != nil || !released || reason != "level-hold-released" {
		t.Fatalf("got (%v, %q, %v), want a racing 404 treated as released", released, reason, err)
	}
}

// TestReleaseLevelHoldSelfAuthNoticeCommentErrorFailsClosed pins the branch
// where the level hold is promotable but the self-authorization gate holds
// the PR and the explanatory comment cannot be posted: the sweep must report
// the error and keep the hold rather than release silently.
func TestReleaseLevelHoldSelfAuthNoticeCommentErrorFailsClosed(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": botComments(levelHoldNotice("quality")),
		"GET /repos/o/r/issues/581": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   581,
				"user":     userJSON(testHiveAppBotLogin, "Bot"),
				"labels":   []map[string]any{},
				"comments": 0,
			})
		},
		"POST /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	c.prHoldLabel = func(string) bool { return false }
	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", heldPR("Closes #581"))
	if released || reason != "self-authorization-notice" || err == nil {
		t.Fatalf("got (%v, %q, %v), want hold kept when the notice cannot be posted", released, reason, err)
	}
	if !strings.Contains(err.Error(), "self-authorization hold") {
		t.Fatalf("err=%v, want it to name the self-authorization notice", err)
	}
}

// TestEnsureLevelHoldNoticeIgnoresUntrustedNoticeAndSurfacesPostError shows a
// forged marker from a non-bot author does not satisfy the dedupe check, and
// that a failed comment post is surfaced rather than swallowed.
func TestEnsureLevelHoldNoticeIgnoresUntrustedNoticeAndSurfacesPostError(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"body": levelHoldNotice("quality"),
				"user": map[string]string{"login": "alice"},
			}})
		},
		"POST /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	err := c.ensureLevelHoldNotice(context.Background(), "o/r", 7, "quality")
	if err == nil || !strings.Contains(err.Error(), "commenting on level hold") {
		t.Fatalf("err=%v, want forged notice ignored and post failure surfaced", err)
	}
}

func TestEnsureLevelHoldNoticeListErrorSurfaced(t *testing.T) {
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	if err := c.ensureLevelHoldNotice(context.Background(), "o/r", 7, "quality"); err == nil {
		t.Fatal("want listing failure surfaced, got nil")
	}
}

// TestEnsureLevelHoldNoticeFindsExistingNoticeOnLaterPage pins comment
// pagination: a trusted notice on page two must suppress a duplicate post.
func TestEnsureLevelHoldNoticeFindsExistingNoticeOnLaterPage(t *testing.T) {
	posted := errors.New("unexpected duplicate notice post")
	c := newLevelHoldGuardClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/7/comments": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				botComments(levelHoldNotice("quality"))(w, r)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			botComments("unrelated chatter")(w, r)
		},
		"POST /repos/o/r/issues/7/comments": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // reaching here fails the test below
		},
	})
	if err := c.ensureLevelHoldNotice(context.Background(), "o/r", 7, "quality"); err != nil {
		t.Fatalf("ensureLevelHoldNotice: %v (%v)", err, posted)
	}
}
