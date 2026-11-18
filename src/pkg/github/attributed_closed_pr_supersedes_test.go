package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// #18719: closed attributed PR records carry the "Supersedes #N" number the
// open-PR path already records, so the PRs-by-model card can tell a PR that
// was replaced from one that failed.
func TestPullRequestAttributionRecordCopiesSupersedes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "declares", body: "Re-authored.\n\nSupersedes #41\n\n-- hive: agent=scanner backend=bob model=auto", want: 41},
		{name: "none", body: "done\n\n-- hive: agent=scanner backend=bob model=auto", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := &gh.PullRequest{Number: gh.Ptr(42), Body: gh.Ptr(tc.body), State: gh.Ptr("closed")}
			got := pullRequestAttributionRecord("r", pr, InvocationMeta{Agent: "scanner", Backend: "bob", Model: "auto"})
			if got.Supersedes != tc.want {
				t.Fatalf("Supersedes = %d, want %d", got.Supersedes, tc.want)
			}
		})
	}
}

func TestFetchAttributedClosedPRsRecordsSupersedesWithoutExtraCalls(t *testing.T) {
	t.Setenv(attributedClosedPRLookbackEnv, "24h")
	now := time.Now().UTC()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("state") != "closed" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		superseder := attributedClosedPR(7, now.Add(-time.Hour))
		superseder["body"] = "Takes over the work.\n\nSupersedes #5\n\n-- hive: agent=scanner backend=bob model=auto"
		writeJSON(t, w, []map[string]any{superseder, attributedClosedPR(5, now.Add(-2*time.Hour))})
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatalf("fetchAttributedClosedPRs: %v", err)
	}
	if calls != 1 {
		t.Fatalf("GitHub calls = %d, want 1 (same as before #18719)", calls)
	}
	if len(prs) != 2 || prs[0].Number != 7 || prs[0].Supersedes != 5 || prs[1].Supersedes != 0 {
		t.Fatalf("records = %+v, want #7 supersedes 5 and #5 none", prs)
	}
}
