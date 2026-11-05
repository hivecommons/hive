package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// projectbluefin/utah-packages#868 cites config/factory-build-backlog.toml
// (its evidence) and docs/skills/factory-build-backlog.md; #852 cites the
// docs file but never the TOML. The cross-link said "both cite
// `config/factory-build-backlog.toml`". It names the shared path now.
func TestReviewBacklogSameFileNamesSharedPath(t *testing.T) {
	withReviewBacklogState(t)
	twins := []map[string]any{}
	for i, n := range []int{852, 840, 831} {
		twins = append(twins, map[string]any{
			"number": n, "state": "open", "html_url": fmt.Sprintf("https://github.example/o/r/issues/%d", n),
			"title": fmt.Sprintf("[reviewer] Review backlog (docs-currency): skill doc %d drifts", i),
			"body":  fmt.Sprintf("Perspective: docs-currency\nEvidence: `docs/skills/font-package-recipes.md:%d`\n\ndocs/skills/factory-build-backlog.md now points at the new recipe.", 100+i),
			"user":  map[string]any{"login": testBot},
		})
	}
	raw, err := json.Marshal(twins)
	if err != nil {
		t.Fatal(err)
	}
	comments := map[int][]string{}
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			if r.URL.Query().Get("state") != "open" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = w.Write(raw)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/comments", &n)
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			comments[n] = append(comments[n], payload.Body)
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			created++
			_, _ = io.WriteString(w, `{"number":868,"html_url":"https://github.example/o/r/issues/868"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit"}},
		}
	})
	finding := nit("Tracking issue #490 already closed despite five outputs still pending", "low", "config/factory-build-backlog.toml", 12,
		"docs/skills/factory-build-backlog.md says the batch stays open until every output builds.")
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 867, Agent: "reviewer", Report: routingReport(t, 867, "abc", finding)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if created != 1 || len(comments[868]) != 1 {
		t.Fatalf("created=%d comments on #868=%q, want one new issue with one cross-link", created, comments[868])
	}
	note := comments[868][0]
	if !strings.Contains(note, "both cite `docs/skills/factory-build-backlog.md`") || strings.Contains(note, "config/factory-build-backlog.toml") {
		t.Fatalf("cross-link must name the shared path, not the finding's own file:\n%s", note)
	}
}

func TestReviewBacklogSameFileCommentFallsBackToFindingFile(t *testing.T) {
	finding := outputschema.Finding{Title: "x", Severity: outputschema.SeverityLow, File: "pkg/a.go", Line: 3, Summary: "y"}
	if got := reviewBacklogSameFileComment(finding, 41, ""); !strings.Contains(got, "both cite `pkg/a.go`") {
		t.Fatalf("equal file set keeps the finding's file:\n%s", got)
	}
	if got := reviewBacklogSameFileComment(finding, 41, " docs/x.md "); !strings.Contains(got, "both cite `docs/x.md`") {
		t.Fatalf("shared path wins:\n%s", got)
	}
}
