package worksource

import (
	"testing"
	"time"
)

// TestToGitHubIssues verifies the normalized worksource Issue fields map onto
// the github.Issue shape the scheduler consumes.
func TestToGitHubIssues(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := created.Add(time.Hour)
	in := []Issue{
		{
			SourceType: "linear",
			Repo:       "my-org/my-repo",
			ExternalID: "ENG-123",
			Number:     0,
			Title:      "Fix the flux capacitor",
			Author:     "doc",
			Labels:     []string{"bug", "urgent"},
			Assignees:  []string{"marty"},
			IsTracker:  true,
			Priority:   "high",
			State:      "Todo",
			CreatedAt:  created,
			UpdatedAt:  updated,
			URL:        "https://linear.app/eng/issue/ENG-123",
			DependsOn: []Dependency{
				{Ref: Ref{SourceType: "linear", Repo: "my-org/my-repo", ExternalID: "ENG-100"}},
				{Ref: Ref{SourceType: "linear", Repo: "my-org/my-repo", ExternalID: "ENG-101"}, Resolved: true},
			},
		},
	}
	out := ToGitHubIssues(in)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	g := out[0]
	if g.Repo != "my-org/my-repo" || g.Number != 0 || g.Title != "Fix the flux capacitor" ||
		g.Author != "doc" || g.URL != "https://linear.app/eng/issue/ENG-123" {
		t.Errorf("scalar fields not mapped: %+v", g)
	}
	if len(g.Labels) != 2 || g.Labels[0] != "bug" {
		t.Errorf("labels not mapped: %v", g.Labels)
	}
	if len(g.Assignees) != 1 || g.Assignees[0] != "marty" {
		t.Errorf("assignees not mapped: %v", g.Assignees)
	}
	if !g.CreatedAt.Equal(created) || !g.UpdatedAt.Equal(updated) {
		t.Errorf("timestamps not mapped: created=%v updated=%v", g.CreatedAt, g.UpdatedAt)
	}
	if !g.IsTracker || g.Priority != "high" || g.State != "Todo" {
		t.Errorf("worksource fields not mapped: %+v", g)
	}
	if len(g.DependsOn) != 2 || g.DependsOn[0].Key != "my-org/my-repo!ENG-100" || g.DependsOn[0].Resolved || !g.DependsOn[1].Resolved {
		t.Errorf("dependency edges not mapped through compatibility envelope: %+v", g.DependsOn)
	}
	wantAge := int(time.Since(created).Minutes())
	if g.AgeMinutes < wantAge-1 || g.AgeMinutes > wantAge+1 {
		t.Errorf("AgeMinutes = %d, want approximately %d", g.AgeMinutes, wantAge)
	}
	// Classification fields must stay zero — populated downstream by classify.
	if g.ComplexityTier != "" || g.ModelRec != "" || g.Lane != "" {
		t.Errorf("classification fields must be zero: %+v", g)
	}
}

func TestToGitHubIssuesUnknownOrFutureAge(t *testing.T) {
	out := ToGitHubIssues([]Issue{
		{CreatedAt: time.Time{}},
		{CreatedAt: time.Now().Add(time.Hour)},
	})
	for i, issue := range out {
		if issue.AgeMinutes != 0 {
			t.Errorf("issue %d AgeMinutes = %d, want 0", i, issue.AgeMinutes)
		}
	}
}

// TestToGitHubIssues_Empty pins that an empty input gives an empty (non-nil)
// slice so IssueResult.Items is never nil after an overlay.
func TestToGitHubIssues_Empty(t *testing.T) {
	out := ToGitHubIssues(nil)
	if out == nil || len(out) != 0 {
		t.Errorf("want empty non-nil slice, got %v", out)
	}
}
