package github

import (
	"context"
	"reflect"
	"testing"
)

// Sidelined PRs (drafts, exempt labels) are RepoWorkPR rows, which the
// enumeration never enriches; the activity monitor still needs their
// closing references to pair them with the issue they fix (#13421).
func TestEnrichWorkPRLinkedIssues_StampsSidelinedPRs(t *testing.T) {
	p := &protectionServer{
		closingIssues: []map[string]any{
			{"number": 12, "title": "Fix login", "state": "OPEN", "url": "https://github.test/org/repo/issues/12", "repository": map[string]any{"nameWithOwner": "org/repo"}},
		},
	}
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	details := map[string]RepoWorkDetails{
		"org/repo": {PRs: []RepoWorkPR{
			{Repo: "org/repo", Number: 1, Draft: true, Bucket: "draft"},
			{Repo: "org/repo", Number: 2, Bucket: "filtered"}, // not in the forge's answer
		}},
	}
	c.EnrichWorkPRLinkedIssues(context.Background(), details)
	want := []PRLinkedIssue{{Number: 12, Title: "Fix login", Repo: "org/repo", State: "open", URL: "https://github.test/org/repo/issues/12"}}
	if got := details["org/repo"].PRs[0].LinkedIssues; !reflect.DeepEqual(got, want) {
		t.Errorf("draft LinkedIssues = %+v, want %+v", got, want)
	}
	if got := details["org/repo"].PRs[1].LinkedIssues; got != nil {
		t.Errorf("unknown PR LinkedIssues = %+v, want nil", got)
	}
	p.mu.Lock()
	calls, rest := p.graphQLCalls, p.restCalls
	p.mu.Unlock()
	if calls != 1 || rest != 0 {
		t.Errorf("graphQL calls = %d, REST calls = %d; want one query per repo and no REST", calls, rest)
	}

	var nilClient *Client
	nilClient.EnrichWorkPRLinkedIssues(context.Background(), details)
}
