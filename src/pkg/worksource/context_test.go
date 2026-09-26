package worksource

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestWorkItemContextNormalizesAndBuildsRef(t *testing.T) {
	ctx := WorkItemContext{
		SourceType: " ",
		Repo:       " acme/widgets ",
		Number:     42,
		Title:      " Ship it ",
		URL:        " https://github.com/acme/widgets/issues/42 ",
	}.Normalized()
	if ctx.SourceType != "github" || ctx.ExternalID != "42" || ctx.Repo != "acme/widgets" || ctx.Title != "Ship it" {
		t.Fatalf("normalized context = %+v", ctx)
	}
	if ref := ctx.Ref(); ref.Key() != "acme/widgets#42" || ref.URL != "https://github.com/acme/widgets/issues/42" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestWorkItemContextFromIssuesCarriesBodyAndExternalID(t *testing.T) {
	ws := WorkItemContextFromIssue(Issue{SourceType: "linear", Repo: "acme/widgets", ExternalID: "LIN-7", Title: "T", Body: "B", URL: "https://linear/LIN-7"})
	if ws.Ref().Key() != "acme/widgets!LIN-7" || ws.Body != "B" {
		t.Fatalf("worksource context = %+v", ws)
	}
	gh := WorkItemContextFromGitHubIssue(github.Issue{SourceType: "jira", Repo: "acme/widgets", ExternalID: "ENG-8", Title: "J", Body: "Desc", URL: "https://jira/ENG-8"})
	if gh.Ref().Key() != "acme/widgets!ENG-8" || gh.Body != "Desc" {
		t.Fatalf("github envelope context = %+v", gh)
	}
}

func TestJiraDescriptionTextHandlesWikiADFAndEmpty(t *testing.T) {
	if got := jiraDescriptionText([]byte(`"plain wiki"`)); got != "plain wiki" {
		t.Fatalf("wiki description = %q", got)
	}
	adf := []byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`)
	if got := jiraDescriptionText(adf); got != "hello world" {
		t.Fatalf("adf description = %q", got)
	}
	if got := jiraDescriptionText([]byte(`null`)); got != "" {
		t.Fatalf("null description = %q", got)
	}
}
