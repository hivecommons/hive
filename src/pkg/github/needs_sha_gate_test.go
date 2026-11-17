package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func needsSHAIssues() []wireIssue {
	return []wireIssue{
		{Number: 1, Title: "bug without sha", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER",
			Labels: []wireLabel{{Name: "kind/bug"}, {Name: "needs-sha"}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "bug with sha supplied", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER",
			Labels: []wireLabel{{Name: "kind/bug"}}, CreatedAt: hoursAgo(2)},
	}
}

func enumerateNeedsSHA(t *testing.T, f config.IssueFilterConfig, label string) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	server := httptest.NewServer(buildMux(t, org, repo, needsSHAIssues(), nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(f)
	if label != "" {
		c.setNeedsSHALabel(label)
	}
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

// A needs-sha issue is never dispatched, with reporter trust on or off; the
// issue whose label was removed (a SHA arrived) is workable.
func TestEnumerateActionable_NeedsSHAKeepsIssueOutOfWork(t *testing.T) {
	cases := map[string]config.IssueFilterConfig{
		"reporter trust off": {},
		"reporter trust on":  enabledReporterTrust(),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			nums := actionableNumbers(enumerateNeedsSHA(t, f, ""))
			if nums[1] {
				t.Error("needs-sha issue entered the actionable set")
			}
			if !nums[2] {
				t.Error("positive control failed: the issue without needs-sha must be actionable")
			}
		})
	}
}

func TestEnumerateActionable_NeedsSHAConfiguredName(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := needsSHAIssues()
	issues[0].Labels = []wireLabel{{Name: "kind/bug"}, {Name: "awaiting-sha"}}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.setNeedsSHALabel("awaiting-sha")
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if actionableNumbers(result)[1] {
		t.Error("issue wearing the configured SHA label entered the actionable set")
	}
}

func TestClassifyWebhookIssue_NeedsSHASidelined(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()), "o", []string{"r"})
	for name, f := range map[string]config.IssueFilterConfig{"off": {}, "on": enabledReporterTrust()} {
		t.Run(name, func(t *testing.T) {
			c.SetIssueFilter(f)
			issue := &gh.Issue{Number: gh.Ptr(7), State: gh.Ptr("open"), Title: gh.Ptr("bug"),
				User: &gh.User{Login: gh.Ptr("maintainer")}, AuthorAssociation: gh.Ptr("MEMBER"),
				Labels:    []*gh.Label{{Name: gh.Ptr("kind/bug")}, {Name: gh.Ptr("needs-sha")}},
				CreatedAt: &gh.Timestamp{Time: time.Now().Add(-time.Hour)}}
			p := c.classifyWebhookIssue("o/r", issue, issueLabelNames(issue.Labels), time.Now())
			if p.Bucket != WebhookBucketSidelined || p.WorkIssue == nil || p.WorkIssue.Reason != "needs-sha" {
				t.Fatalf("patch = %+v, want sidelined with reason needs-sha", p)
			}
		})
	}
}
