package taskmcp

import (
	"testing"
	"time"
)

func TestFilterRelatedWorkMatchesFilesCitationsAndDuplicateSweep(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	merged := now.Add(-24 * time.Hour)
	data, _ := FilterRelatedWork(Scope{Repo: "owner/repo", Number: 10}, RelatedItem{Kind: "issue", Repo: "owner/repo", Number: 10, Files: []string{"a.go"}}, []RelatedItem{
		{Kind: "pull_request", Repo: "owner/repo", Number: 11, Files: []string{"a.go"}, Data: ServedText{Title: "file"}},
		{Kind: "issue", Repo: "owner/repo", Number: 12, Data: ServedText{Body: "Fixes #10"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 13, Reasons: []string{DuplicateSweepReason}, MergedAt: &merged, Data: ServedText{Title: "dup"}},
		{Kind: "pull_request", Repo: "other/repo", Number: 14, Files: []string{"a.go"}},
	}, 14*24*time.Hour, now, PageRequest{Limit: 20})
	if len(data.Items) != 3 {
		t.Fatalf("items = %#v", data.Items)
	}
	for _, item := range data.Items {
		if item.Repo != "owner/repo" || len(item.Reasons) == 0 {
			t.Fatalf("unscoped or unreasonsed item: %#v", item)
		}
	}
}

func TestFilterRelatedWorkCapsAndPaginates(t *testing.T) {
	items := make([]RelatedItem, MaxPageSize+2)
	for i := range items {
		items[i] = RelatedItem{Kind: "issue", Repo: "owner/repo", Number: i + 2, Data: ServedText{Body: "refs #1"}}
	}
	data, page := FilterRelatedWork(Scope{Repo: "owner/repo", Number: 1}, RelatedItem{Repo: "owner/repo", Number: 1}, items, 0, time.Now(), PageRequest{Limit: 99})
	if len(data.Items) != MaxPageSize || !page.More || page.NextCursor == "" {
		t.Fatalf("items=%d page=%#v", len(data.Items), page)
	}
}

func TestFilterRelatedWorkCitationDoesNotPrefixMatch(t *testing.T) {
	data, _ := FilterRelatedWork(Scope{Repo: "owner/repo", Number: 10}, RelatedItem{Repo: "owner/repo", Number: 10}, []RelatedItem{
		{Kind: "issue", Repo: "owner/repo", Number: 100, Data: ServedText{Body: "refs #100"}},
	}, 0, time.Now(), PageRequest{})
	if len(data.Items) != 0 {
		t.Fatalf("items = %#v, want no prefix citation match", data.Items)
	}
}

func TestFilterHistoryRequiresRecentClosedAndSameThreadOrPath(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	merged := now.Add(-time.Hour)
	old := now.Add(-72 * time.Hour)
	items := FilterHistory(Scope{Repo: "owner/repo", Number: 10}, RelatedItem{Kind: "issue", Repo: "owner/repo", Number: 10, Files: []string{"a.go"}}, []RelatedItem{
		{Kind: "pull_request", Repo: "owner/repo", Number: 11, State: "closed", MergedAt: &merged, Files: []string{"a.go"}, Data: ServedText{Title: "same file"}},
		{Kind: "issue", Repo: "owner/repo", Number: 12, State: "closed", UpdatedAt: &merged, Data: ServedText{Body: "refs #10"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 13, State: "closed", MergedAt: &old, Files: []string{"a.go"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 14, State: "open", Files: []string{"a.go"}},
		{Kind: "pull_request", Repo: "other/repo", Number: 15, State: "closed", MergedAt: &merged, Files: []string{"a.go"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 16, State: "closed", UpdatedAt: &merged, Files: []string{"a.go"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 17, State: "closed", MergedAt: &old, UpdatedAt: &merged, Files: []string{"a.go"}},
	}, 24*time.Hour, now, 10)
	if len(items) != 2 {
		t.Fatalf("history = %#v", items)
	}
	for _, item := range items {
		if len(item.Reasons) == 0 || item.Repo != "owner/repo" {
			t.Fatalf("bad history item: %#v", item)
		}
	}
}

func TestFilterHistoryCapsAndDefaults(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	merged := now.Add(-time.Hour)
	candidates := make([]RelatedItem, DefaultTaskMCPHistoryLimit+2)
	for i := range candidates {
		candidates[i] = RelatedItem{Kind: "pull_request", Repo: "owner/repo", Number: i + 2, State: "closed", MergedAt: &merged, Files: []string{"a.go"}}
	}
	items := FilterHistory(Scope{Repo: "owner/repo", Number: 1}, RelatedItem{Repo: "owner/repo", Number: 1, Files: []string{"a.go"}}, candidates, 24*time.Hour, now, 0)
	if len(items) != DefaultTaskMCPHistoryLimit {
		t.Fatalf("history len = %d", len(items))
	}
	items = FilterHistory(Scope{Repo: "owner/repo", Number: 1}, RelatedItem{Repo: "owner/repo", Number: 1, Files: []string{"a.go"}}, candidates, 24*time.Hour, now, DefaultTaskMCPHistoryLimit+2)
	if len(items) != DefaultTaskMCPHistoryLimit+2 {
		t.Fatalf("configured history len = %d", len(items))
	}
}
