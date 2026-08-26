package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// digestCommentMux serves an advisory issue whose comment list is fixed, and
// records whether the digest path PATCHed an existing comment or POSTed a new
// one.
func digestCommentMux(org, repo string, comments []map[string]any, patched map[int64]bool, created *bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(comments)
		case "POST":
			*created = true
			json.NewEncoder(w).Encode(map[string]any{"id": 9999, "user": map[string]any{"login": "hive-app[bot]"}})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			var id int64
			fmt.Sscanf(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/%%d", org, repo), &id)
			patched[id] = true
			json.NewEncoder(w).Encode(map[string]any{"id": id, "user": map[string]any{"login": "hive-app[bot]"}})
		}
	})
	return mux
}

func newAppTestClient(t *testing.T, server *httptest.Server, org, repo, botLogin string) *Client {
	t.Helper()
	c := newTestClient(t, server, org, []string{repo})
	// Promote the token test client to an App-authenticated one: only the
	// presence of appAuth and the bot login matter to the digest author gate.
	c.appAuth = &AppAuth{}
	c.appBotLogin = botLogin
	return c
}

// A digest comment left behind by the removed user-token fallback (#1927) is
// authored by a HUMAN. GitHub hard-forbids an App from editing any
// foreign-authored comment (403 "Resource not accessible by integration"), so
// the App must never adopt it: it posts a fresh comment of its own instead of
// PATCHing the human's forever (kalantar-msb/soft-reflective#1).
func TestPostAdvisoryDigest_SkipsHumanAuthoredDigestComment(t *testing.T) {
	org, repo := "kalantar-msb", "soft-reflective"
	patched := map[int64]bool{}
	var created bool
	server := httptest.NewServer(digestCommentMux(org, repo, []map[string]any{
		{"id": 5243180833, "body": advisoryDigestPrefix + " — legacy user-token digest", "user": map[string]any{"login": "kalantar", "type": "User"}},
	}, patched, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if patched[5243180833] {
		t.Error("App PATCHed a human-authored comment — GitHub always 403s this")
	}
	if !created {
		t.Error("expected a fresh App-authored digest comment to be created")
	}
}

// The App's own digest comment (author == appBotLogin) is still updated in
// place — no duplicates per cycle — and wins over an earlier foreign one.
func TestPostAdvisoryDigest_UpdatesOwnBotComment(t *testing.T) {
	org, repo := "testorg", "testrepo"
	patched := map[int64]bool{}
	var created bool
	server := httptest.NewServer(digestCommentMux(org, repo, []map[string]any{
		{"id": 111, "body": advisoryDigestPrefix + " — human leftover", "user": map[string]any{"login": "someone", "type": "User"}},
		{"id": 222, "body": advisoryDigestPrefix + " — ours", "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}},
	}, patched, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !patched[222] {
		t.Error("expected the App's own digest comment to be updated in place")
	}
	if created || patched[111] {
		t.Errorf("wrong write path: created=%v patchedForeign=%v", created, patched[111])
	}
}

// With the bot login unknown (older config), a bot-authored digest comment is
// still adopted — refusing our own comment on a missing slug would create a
// duplicate every cycle — while a human-authored one is still skipped.
func TestPostAdvisoryDigest_BotLoginUnknownAdoptsBotComment(t *testing.T) {
	org, repo := "testorg", "testrepo"
	patched := map[int64]bool{}
	var created bool
	server := httptest.NewServer(digestCommentMux(org, repo, []map[string]any{
		{"id": 111, "body": advisoryDigestPrefix + " — human leftover", "user": map[string]any{"login": "someone", "type": "User"}},
		{"id": 333, "body": advisoryDigestPrefix + " — bot", "user": map[string]any{"login": "some-app[bot]", "type": "Bot"}},
	}, patched, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "")
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !patched[333] {
		t.Error("expected the bot-authored digest comment to be updated")
	}
	if created || patched[111] {
		t.Errorf("wrong write path: created=%v patchedForeign=%v", created, patched[111])
	}
}

// A token (PAT) client keeps the historical prefix-only match: the credential
// may be the very human who authored the comment.
func TestPostAdvisoryDigest_TokenClientKeepsPrefixMatch(t *testing.T) {
	org, repo := "testorg", "testrepo"
	patched := map[int64]bool{}
	var created bool
	server := httptest.NewServer(digestCommentMux(org, repo, []map[string]any{
		{"id": 444, "body": advisoryDigestPrefix + " — mine", "user": map[string]any{"login": "kalantar", "type": "User"}},
	}, patched, &created))
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !patched[444] || created {
		t.Errorf("token client behavior changed: patched=%v created=%v", patched[444], created)
	}
}
