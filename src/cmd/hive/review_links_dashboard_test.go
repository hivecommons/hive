package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

func TestAttachReviewLinksForDashboardAnnotatesRestoredRefreshes(t *testing.T) {
	oldPath := github.ReviewLinksPath
	github.ReviewLinksPath = filepath.Join(t.TempDir(), github.ReviewLinksFile)
	t.Cleanup(func() { github.ReviewLinksPath = oldPath })

	const reviewURL = "https://github.com/projectbluefin/server/pull/90#pullrequestreview-5254900000"
	reviewedAt := time.Date(2026, 9, 19, 14, 4, 0, 0, time.UTC)
	if err := github.RecordReviewLink("", "projectbluefin/server", 90, github.ReviewLink{
		URL:   reviewURL,
		State: "commented",
		At:    reviewedAt,
		Count: 1,
	}); err != nil {
		t.Fatalf("record review link: %v", err)
	}

	payload := &dashboard.StatusPayload{
		Repos: []dashboard.FrontendRepo{{
			Name: "server",
			Full: "projectbluefin/server",
			OpenPrs: []any{
				dashboard.FrontendPR{PullRequest: github.PullRequest{Repo: "server", Number: 90}},
			},
		}},
	}

	attachReviewLinksForDashboard(payload, nil)

	got := payload.Repos[0].OpenPrs[0].(dashboard.FrontendPR)
	if got.ReviewURL != reviewURL {
		t.Fatalf("review_url = %q, want %q", got.ReviewURL, reviewURL)
	}
	if got.ReviewCount != 1 || got.ReviewState != "commented" {
		t.Fatalf("review metadata = count %d state %q, want count 1 state commented", got.ReviewCount, got.ReviewState)
	}
}
