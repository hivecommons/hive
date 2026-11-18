package github

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func TestEnumerateActionable_AgentProposalGate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	bot := wireUser{"hive[bot]"}
	issues := []wireIssue{
		{Number: 1, Title: "proposal", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}}, CreatedAt: hoursAgo(5)},
		{Number: 2, Title: "[Outreach] proposal", User: bot, CreatedAt: hoursAgo(5)},
		{Number: 3, Title: "approved", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}, {Name: HumanAckLabel}}, CreatedAt: hoursAgo(5)},
		{Number: 4, Title: "assigned", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}}, Assignees: []wireUser{{"dan"}}, CreatedAt: hoursAgo(5)},
		{Number: 5, Title: "bot assigned", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}}, Assignees: []wireUser{{"other[bot]"}}, CreatedAt: hoursAgo(5)},
		{Number: 6, Title: "human proposal", User: wireUser{"alice"}, Labels: []wireLabel{{Name: "agent/strategist"}}, CreatedAt: hoursAgo(5)},
		{Number: 7, Title: "unknown author", Labels: []wireLabel{{Name: "agent/strategist"}}, CreatedAt: hoursAgo(5)},
		{Number: 8, Title: "[architect] opt in", User: bot, Labels: []wireLabel{{Name: "agent/architect"}}, CreatedAt: hoursAgo(5)},
		{Number: 9, Title: "bug", User: bot, Labels: []wireLabel{{Name: "agent/scanner"}}, CreatedAt: hoursAgo(5)},
		{Number: 10, Title: "human parent", User: wireUser{"alice"}, State: "open", CreatedAt: hoursAgo(5)},
		{Number: 11, Title: "child", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}}, CreatedAt: hoursAgo(5)},
		{Number: 12, Title: "unacknowledged parent", User: bot, State: "open", CreatedAt: hoursAgo(5)},
		{Number: 13, Title: "child", User: bot, Labels: []wireLabel{{Name: "agent/strategist"}}, CreatedAt: hoursAgo(5)},
	}
	withSplitParentLedger(t, org+"/"+repo, map[int]int{11: 10, 13: 12})
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	nums := actionableNumbers(result)
	for _, n := range []int{1, 2, 5, 13} {
		if nums[n] {
			t.Errorf("unacknowledged proposal #%d is actionable", n)
		}
	}
	for _, n := range []int{3, 4, 6, 7, 8, 9, 10, 11, 12} {
		if !nums[n] {
			t.Errorf("issue #%d should be actionable", n)
		}
	}
	for _, issue := range result.Issues.Items {
		if issue.Number == 11 && (!issue.HumanAcknowledged || issue.AckSource != "parent #10") {
			t.Errorf("child acknowledgement = %v, source %q", issue.HumanAcknowledged, issue.AckSource)
		}
	}
	rows := map[int]RepoWorkIssue{}
	for _, details := range result.WorkDetailsByRepo {
		for _, row := range details.Issues {
			rows[row.Number] = row
		}
	}
	for _, n := range []int{1, 2, 5, 13} {
		if row := rows[n]; row.Bucket != "filtered" || row.Reason != agentProposalFilterReason {
			t.Errorf("issue #%d activity row = %+v", n, row)
		}
	}
}

func TestEnumerateActionable_AgentProposalOverrides(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "[strategist] proposal", User: wireUser{"hive[bot]"}, CreatedAt: hoursAgo(5)},
		{Number: 2, Title: "[architect] proposal", User: wireUser{"hive[bot]"}, CreatedAt: hoursAgo(5)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name         string
		gate         *config.AgentProposalGateConfig
		want1, want2 bool
	}{
		{"disabled", &config.AgentProposalGateConfig{}, true, true},
		{"architect opt in", &config.AgentProposalGateConfig{TitlePrefixes: []string{"[architect]"}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, server, org, []string{repo})
			c.SetIssueFilter(config.IssueFilterConfig{AgentProposal: tc.gate})
			result, err := c.EnumerateActionable(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			nums := actionableNumbers(result)
			if nums[1] != tc.want1 || nums[2] != tc.want2 {
				t.Errorf("actionable = %v, want issue 1=%v issue 2=%v", nums, tc.want1, tc.want2)
			}
		})
	}
}

func TestClassifyWebhookIssue_AgentProposalGate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	server := httptest.NewServer(buildMux(t, org, repo, nil, nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{})
	for _, tc := range []struct {
		name, author string
		labels       []string
		assignee     string
		want         WebhookItemBucket
	}{
		{"unacknowledged", "hive[bot]", []string{"agent/strategist"}, "", WebhookBucketUnknown},
		{"bot assignee", "hive[bot]", []string{"agent/strategist"}, "other[bot]", WebhookBucketUnknown},
		{"approved", "hive[bot]", []string{"agent/strategist", HumanAckLabel}, "", WebhookBucketActionable},
		{"human assignee", "hive[bot]", []string{"agent/strategist"}, "dan", WebhookBucketActionable},
		{"human author", "alice", []string{"agent/strategist"}, "", WebhookBucketActionable},
		{"unknown author", "", []string{"agent/strategist"}, "", WebhookBucketActionable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := &gh.Issue{Number: gh.Ptr(1), Title: gh.Ptr("proposal"), User: &gh.User{Login: gh.Ptr(tc.author)}}
			for _, label := range tc.labels {
				issue.Labels = append(issue.Labels, &gh.Label{Name: gh.Ptr(label)})
			}
			if tc.assignee != "" {
				issue.Assignees = []*gh.User{{Login: gh.Ptr(tc.assignee)}}
			}
			patch := c.classifyWebhookIssue(repo, issue, tc.labels, time.Now())
			if patch.Bucket != tc.want {
				t.Errorf("bucket = %q, want %q", patch.Bucket, tc.want)
			}
			if tc.want == WebhookBucketUnknown && (patch.WorkIssue == nil || patch.WorkIssue.Bucket != "filtered" || patch.WorkIssue.Reason != agentProposalFilterReason) {
				t.Errorf("activity row = %+v", patch.WorkIssue)
			}
		})
	}
}
