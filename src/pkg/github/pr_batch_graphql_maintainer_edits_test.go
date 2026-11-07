package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The GraphQL batch stores a PR detail that EnrichCIStatus reuses in place
// of a REST GET, and the PR's "allow edits by maintainers" is read from that
// detail. Without the field the batch detail said "no" for every fork PR:
// the hold guard stopped updating projectbluefin/gutenprint-printer-app#196
// and hplip-printer-app#199 as "a fork the Hive app cannot push to" without
// ever asking GitHub, while both forks allowed maintainer edits.
func TestGraphQLPRBatchCarriesMaintainerCanModify(t *testing.T) {
	restore := resetPRDetailCacheForTest(time.Now, 128)
	defer restore()

	withEdits := func(node string, allow bool) string {
		return strings.Replace(node, `"isDraft":false,`, fmt.Sprintf(`"isDraft":false,"maintainerCanModify":%t,`, allow), 1)
	}
	var pullGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s,%s]}},"rateLimit":{"cost":3,"remaining":4997,"resetAt":"2026-10-08T23:00:00Z"}}}`,
				withEdits(gqlPRNodeJSON(1, "BEHIND", "MERGEABLE", "sha-1", "completed", "success"), true),
				withEdits(gqlPRNodeJSON(2, "BEHIND", "MERGEABLE", "sha-2", "completed", "success"), false))
		case strings.Contains(r.URL.Path, "/pulls/"):
			pullGets.Add(1)
			t.Fatalf("unexpected REST pull GET %s: the batch detail should be reused", r.URL.Path)
		case r.URL.Path == "/rate_limit":
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4997,"reset":1791500400}}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "sha-1", UpdatedAt: mustTime("2026-10-08T20:00:00Z"), FromFork: true},
		{Repo: "org/repo", Number: 2, HeadSHA: "sha-2", UpdatedAt: mustTime("2026-10-08T20:00:00Z"), FromFork: true},
	}
	c.EnrichCIStatus(context.Background(), prs)
	if got := pullGets.Load(); got != 0 {
		t.Fatalf("REST pull GETs = %d, want 0", got)
	}
	if prs[0].MergeableState != "behind" {
		t.Fatalf("mergeable_state = %q, want behind from the batch", prs[0].MergeableState)
	}
	if !prs[0].MaintainerCanModify {
		t.Fatal("fork PR that allows maintainer edits read as not editable from the batch detail")
	}
	if prs[1].MaintainerCanModify {
		t.Fatal("fork PR that does not allow maintainer edits read as editable")
	}
	if got := ReachableAction(prs[0]); got != ReachableActionPush {
		t.Fatalf("ReachableAction = %q, want %q for an editable fork", got, ReachableActionPush)
	}
}
