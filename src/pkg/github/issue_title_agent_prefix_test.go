package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// Every title-producing helper in this package tags the filing agent, and
// the one create path tags whatever a caller hands it, so a hive-filed issue
// always says who filed it (the owner's rule: "agents that need to open
// issues need to use [agent name]").
func TestHiveFiledIssueTitlesCarryTheFilingAgent(t *testing.T) {
	finding := outputschema.Finding{Title: "token logged", File: "pkg/a.go", Line: 3}
	helpers := map[string]string{
		"review backlog":       reviewBacklogIssueTitle(review.PerspectiveSecurity, finding),
		"review backlog empty": reviewBacklogIssueTitle(review.PerspectiveStyle, outputschema.Finding{}),
	}
	for name, title := range helpers {
		if issueshape.AgentPrefix(title) != config.OriginSourceReviewer {
			t.Errorf("%s title %q does not start with [reviewer]", name, title)
		}
	}
	if got := reviewBacklogIssueTitle(review.PerspectiveSecurity, finding); got != "[reviewer] Review backlog (security): token logged" {
		t.Fatalf("review backlog title = %q", got)
	}
	if got := reviewBacklogItemCategory("[reviewer] Review backlog (security): token logged", ""); got != "security" {
		t.Fatalf("tagged title category = %q, want security", got)
	}
	if got := reviewBacklogItemCategory("Review backlog (style): x", ""); got != "style" {
		t.Fatalf("legacy untagged title category = %q, want style", got)
	}

	filers := []struct {
		name string
		req  CreateIssueRequest
		want string
	}{
		{"agent replica", CreateIssueRequest{Origin: "reviewer-2"}, "reviewer"},
		{"custom agent", CreateIssueRequest{Origin: "adjudicator"}, "adjudicator"},
		{"governor hotspot routed to a lane", CreateIssueRequest{Origin: config.OriginSourceGovernor, Labels: []string{"agent/ci-maintainer", "kind/cleanup"}}, "governor"},
		{"self report", CreateIssueRequest{Origin: config.OriginSourceSelfReport}, "selfreport"},
		{"convergence through the forge seam", CreateIssueRequest{Origin: config.OriginSourceForge, Labels: []string{"origin/convergence"}}, "convergence"},
		{"label only", CreateIssueRequest{Labels: []string{"agent/scanner"}}, "scanner"},
	}
	for _, tc := range filers {
		if got := tc.req.TitleFiler(); got != tc.want {
			t.Errorf("%s: TitleFiler = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// createIssue tags the title for every caller and dedupes against an open
// issue that was filed before titles carried the tag, so the roll that
// introduces the convention does not refile every open finding.
func TestCreateIssueTagsTitleAndDedupesAgainstUntaggedTwin(t *testing.T) {
	var createdTitles []string
	var listed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			if listed == "" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			b, _ := json.Marshal([]map[string]any{{"number": 7, "title": listed, "html_url": "https://github.example/o/r/issues/7"}})
			_, _ = w.Write(b)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			var body struct {
				Title string `json:"title"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdTitles = append(createdTitles, body.Title)
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	ctx := context.Background()

	res, err := c.CreateIssue(ctx, CreateIssueRequest{Repo: "o/r", Title: "Review backlog (security): token logged", Body: "body", Origin: "reviewer-2"})
	if err != nil || res.AlreadyExisted {
		t.Fatalf("create: res=%+v err=%v", res, err)
	}
	res, err = c.CreateIssue(ctx, CreateIssueRequest{Repo: "o/r", Title: "[selfreport] already tagged", Body: "body", Origin: config.OriginSourceSelfReport})
	if err != nil || res.AlreadyExisted {
		t.Fatalf("create tagged: res=%+v err=%v", res, err)
	}
	want := []string{"[reviewer] Review backlog (security): token logged", "[selfreport] already tagged"}
	if strings.Join(createdTitles, "|") != strings.Join(want, "|") {
		t.Fatalf("created titles = %q, want %q", createdTitles, want)
	}

	listed = "Review backlog (security): token logged"
	res, err = c.CreateIssue(ctx, CreateIssueRequest{Repo: "o/r", Title: "Review backlog (security): token logged", Body: "body", Origin: "reviewer"})
	if err != nil || !res.AlreadyExisted || res.Number != 7 {
		t.Fatalf("untagged open twin must dedupe the tagged re-filing: res=%+v err=%v", res, err)
	}
	listed = "[reviewer] Review backlog (security): token logged"
	res, err = c.CreateIssue(ctx, CreateIssueRequest{Repo: "o/r", Title: "Review backlog (security): token logged", Body: "body", Origin: "reviewer"})
	if err != nil || !res.AlreadyExisted || res.Number != 7 {
		t.Fatalf("tagged open twin must dedupe: res=%+v err=%v", res, err)
	}
	if len(createdTitles) != 2 {
		t.Fatalf("dedupe must not create: %q", createdTitles)
	}
}
