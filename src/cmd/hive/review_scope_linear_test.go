package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/worksource"
)

type fakeScopeReader struct {
	issue worksource.LinearScopeIssue
	err   error
	asked []string
}

func (f *fakeScopeReader) ScopeContractIssue(_ context.Context, id string) (worksource.LinearScopeIssue, error) {
	f.asked = append(f.asked, id)
	return f.issue, f.err
}

// isolateReviewScopeFailures gives each test a fresh failure cache, including
// repeated runs in the same process. These tests must not run in parallel
// because reviewScopeFailures is process-global.
func isolateReviewScopeFailures(t *testing.T) {
	t.Helper()
	reviewScopeFailures.mu.Lock()
	saved := reviewScopeFailures.at
	reviewScopeFailures.at = nil
	reviewScopeFailures.mu.Unlock()
	t.Cleanup(func() {
		reviewScopeFailures.mu.Lock()
		reviewScopeFailures.at = saved
		reviewScopeFailures.mu.Unlock()
	})
}

func TestReviewScopeContractForLinear(t *testing.T) {
	isolateReviewScopeFailures(t)
	reader := &fakeScopeReader{issue: worksource.LinearScopeIssue{Identifier: "ONB-3872", State: "In Progress", Text: "ONB-3872: Onboarding"}}
	item := review.PullRequest{Repo: "r", Number: 1, ScopeContract: "linked Linear issue ONB-3872"}
	reviewScopeContractFor(context.Background(), reader, &item, restoreTestLogger())
	if item.ScopeContractState != "In Progress" || item.ScopeContractText != "ONB-3872: Onboarding" {
		t.Fatalf("item = %+v", item)
	}
	if len(reader.asked) != 1 || reader.asked[0] != "ONB-3872" {
		t.Fatalf("asked = %v", reader.asked)
	}
}

func TestReviewScopeContractForSkipsAndFailures(t *testing.T) {
	isolateReviewScopeFailures(t)
	reader := &fakeScopeReader{issue: worksource.LinearScopeIssue{Text: "x"}}
	gh := review.PullRequest{ScopeContract: "linked issue o/r#1 from the PR closing keyword"}
	reviewScopeContractFor(context.Background(), reader, &gh, restoreTestLogger())
	if len(reader.asked) != 0 || gh.ScopeContractText != "" {
		t.Fatalf("GitHub contract fetched from Linear: %v %+v", reader.asked, gh)
	}

	failing := &fakeScopeReader{err: errors.New("boom")}
	item := review.PullRequest{ScopeContract: "linked Linear issue ONB-3872"}
	reviewScopeContractFor(context.Background(), failing, &item, restoreTestLogger())
	if len(failing.asked) != 1 || failing.asked[0] != "ONB-3872" {
		t.Fatalf("asked = %v, want one failed fetch for ONB-3872", failing.asked)
	}
	if item.ScopeContractText != "" {
		t.Fatalf("failed fetch left text %q", item.ScopeContractText)
	}

	reviewScopeContractFor(context.Background(), nil, &item, restoreTestLogger())
}

func TestReviewScopeContractForCachesFailures(t *testing.T) {
	isolateReviewScopeFailures(t)
	failing := &fakeScopeReader{err: errors.New("boom")}
	item := review.PullRequest{ScopeContract: "linked Linear issue ZZZ-1"}
	reviewScopeContractFor(context.Background(), failing, &item, restoreTestLogger())
	reviewScopeContractFor(context.Background(), failing, &item, restoreTestLogger())
	if len(failing.asked) != 1 {
		t.Fatalf("asked = %v, want one fetch within the failure TTL", failing.asked)
	}
}

func TestReviewScopeLinearKeyTracksTeams(t *testing.T) {
	a := config.LinearSourceConfig{APIKey: "k", Teams: []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "o/r"}}}
	b := a
	b.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "o/other"}}
	if reviewScopeLinearKey(a) == reviewScopeLinearKey(b) {
		t.Fatal("team change must change the key")
	}
}
