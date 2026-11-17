package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hivecommons/hive#18516: the remote-items fetcher reads each repo under the
// token TokenFor gives for it (the scoped bot token on a bot-identity repo),
// and when TokenFor is set it never falls back to the owner's token.
func TestRemoteItemsFetcher_TokenForPerRepo(t *testing.T) {
	var (
		mu   sync.Mutex
		seen = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	ownerReads := 0
	f := &RemoteItemsFetcher{
		Token: func() (string, bool) { ownerReads++; return "gho_owner", true },
		TokenFor: func(repo string) (string, bool) {
			switch repo {
			case "hivecommons/docs":
				return "ghs_bot_docs", true
			case "acme/site":
				return "gho_owner", true
			}
			return "", false
		},
		APIURL: srv.URL,
	}
	ctx := context.Background()
	for _, repo := range []string{"hivecommons/docs", "acme/site"} {
		if _, err := f.ListOwnOpenItems(ctx, repo, "someone"); err != nil {
			t.Fatalf("%s: %v", repo, err)
		}
	}
	if got := seen["/repos/hivecommons/docs/issues"]; !strings.HasSuffix(got, "ghs_bot_docs") {
		t.Fatalf("bot repo read with %q, want the bot token", got)
	}
	if got := seen["/repos/acme/site/issues"]; !strings.HasSuffix(got, "gho_owner") {
		t.Fatalf("owner repo read with %q, want the owner token", got)
	}
	// No token for the repo: an error, never the owner's token instead.
	if _, err := f.ListOwnOpenItems(ctx, "other/lib", "someone"); err == nil {
		t.Fatal("a repo without a token must error")
	}
	if _, err := f.CommentsSince(ctx, "other/lib", 1, true, time.Time{}); err == nil {
		t.Fatal("comments on a repo without a token must error")
	}
	if ownerReads != 0 {
		t.Fatalf("TokenFor is authoritative: the owner token source was read %d times", ownerReads)
	}
	if _, ok := seen["/repos/other/lib/issues"]; ok {
		t.Fatal("no request may leave without a token")
	}
}
