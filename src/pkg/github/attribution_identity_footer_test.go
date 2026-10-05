package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const contributorAttributionBody = "Done\n\n— hive: backend=codex model=gpt-6-astra effort=medium\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `671737a3b`"

func TestParseAttributionTrailerWithIdentityFooter(t *testing.T) {
	for _, body := range []string{
		contributorAttributionBody,
		contributorAttributionBody + "\n\n",
		strings.ReplaceAll(contributorAttributionBody, "\n", "\r\n"),
		strings.ReplaceAll(contributorAttributionBody, " | **SHA:**", " | **Instance:** `hive-bold-fox` | **SHA:**"),
		strings.ReplaceAll(contributorAttributionBody, " `contributor`", ""),
	} {
		meta, ok := ParseAttributionTrailer(body)
		if !ok || meta.Backend != "codex" || meta.Model != "gpt-6-astra" {
			t.Errorf("ParseAttributionTrailer(%q) = %+v, %v", body, meta, ok)
		}
	}
}

func TestParseAttributionTrailerDoesNotScanPastOrdinaryContent(t *testing.T) {
	for _, body := range []string{
		contributorAttributionBody + "\nMore prose",
		strings.ReplaceAll(contributorAttributionBody, "\n\n---", "\nMore prose\n\n---"),
		strings.ReplaceAll(contributorAttributionBody, "\n\n---", "\n\n"),
		strings.ReplaceAll(contributorAttributionBody, "**SHA:**", "**Other:**"),
		strings.ReplaceAll(contributorAttributionBody, "— hive:", "> — hive:"),
		"```text\n" + contributorAttributionBody + "\n```",
		strings.ReplaceAll(contributorAttributionBody, "\n\n---", "\n```\n\n---"),
		contributorAttributionBody + "\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `unknown`",
	} {
		if meta, ok := ParseAttributionTrailer(body); ok {
			t.Errorf("accepted nonterminal or quoted attribution in %q: %+v", body, meta)
		}
	}
}

func TestCollectAttributedPRsWithIdentityFooter(t *testing.T) {
	pr := &gh.PullRequest{Number: gh.Ptr(10480), Body: gh.Ptr(contributorAttributionBody)}
	prs := collectAttributedPRsFromList("hivecommons/hive", []*gh.PullRequest{pr})
	if len(prs) != 1 || !prs[0].HiveAttributed || prs[0].HiveBackend != "codex" || prs[0].HiveModel != "gpt-6-astra" {
		t.Fatalf("attributed PRs = %+v", prs)
	}
}

func TestFetchAttributedClosedPRsWithIdentityFooter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("state") != "closed" {
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		pr := attributedClosedPR(10480, now)
		pr["body"] = contributorAttributionBody
		pr["merged_at"] = now.Format(time.RFC3339)
		writeJSON(t, w, []map[string]any{pr})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].HiveBackend != "codex" || prs[0].HiveModel != "gpt-6-astra" || !prs[0].MergedAt.Equal(now) {
		t.Fatalf("merged attributed PRs = %+v", prs)
	}
}
