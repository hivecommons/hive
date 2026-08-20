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
			CreatedAt:  created,
			UpdatedAt:  updated,
			URL:        "https://linear.app/eng/issue/ENG-123",
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
	// GitHub-native fields must stay zero — populated downstream by classify.
	if g.AgeMinutes != 0 || g.IsTracker || g.ComplexityTier != "" || g.ModelRec != "" || g.Lane != "" {
		t.Errorf("github-native fields must be zero: %+v", g)
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
