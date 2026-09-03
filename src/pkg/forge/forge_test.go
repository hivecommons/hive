package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/github"
)

func TestNewForge(t *testing.T) {
	tests := []struct {
		name     string
		kind     Kind
		wantKind Kind
		wantErr  bool
	}{
		{"github", KindGitHub, KindGitHub, false},
		{"gitlab", KindGitLab, KindGitLab, false},
		{"empty defaults to github", Kind(""), KindGitHub, false},
		{"unknown errors", Kind("bitbucket"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := NewForge(tt.kind, "token", Options{Org: "acme"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewForge: %v", err)
			}
			if f.Kind() != tt.wantKind {
				t.Errorf("Kind() = %q, want %q", f.Kind(), tt.wantKind)
			}
		})
	}
}

func TestSplitRepo(t *testing.T) {
	tests := []struct {
		repo, defOwner, wantOwner, wantName string
	}{
		{"org/repo", "def", "org", "repo"},
		{"repo", "def", "def", "repo"},
		{"a/b/c", "def", "a", "b/c"},
	}
	for _, tt := range tests {
		owner, name := splitRepo(tt.repo, tt.defOwner)
		if owner != tt.wantOwner || name != tt.wantName {
			t.Errorf("splitRepo(%q,%q) = %q,%q want %q,%q",
				tt.repo, tt.defOwner, owner, name, tt.wantOwner, tt.wantName)
		}
	}
}

// stubGitHub implements githubReader for the GitHub adapter smoke test without
// any network access.
type stubGitHub struct {
	repo   *gh.Repository
	result *github.ActionableResult
	err    error
}

func (s *stubGitHub) GetRepo(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.repo, &gh.Response{}, nil
}

func (s *stubGitHub) EnumerateActionable(ctx context.Context) (*github.ActionableResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func TestGitHubForgeGetRepo(t *testing.T) {
	stub := &stubGitHub{
		repo: &gh.Repository{
			FullName:      gh.Ptr("acme/widget"),
			HTMLURL:       gh.Ptr("https://github.com/acme/widget"),
			DefaultBranch: gh.Ptr("main"),
			Description:   gh.Ptr("a widget"),
		},
	}
	f := newGitHubForgeWithReader(stub, "acme")

	repo, err := f.GetRepo(context.Background(), "widget")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.FullName != "acme/widget" {
		t.Errorf("FullName = %q", repo.FullName)
	}
	if repo.Owner != "acme" || repo.Name != "widget" {
		t.Errorf("owner/name = %q/%q", repo.Owner, repo.Name)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", repo.DefaultBranch)
	}
}

func TestGitHubForgeListIssuesAndCRs(t *testing.T) {
	now := time.Now()
	stub := &stubGitHub{
		result: &github.ActionableResult{
			Issues: github.IssueResult{
				Count: 2,
				Items: []github.Issue{
					{Repo: "acme/widget", Number: 1, Title: "bug", Author: "alice", Labels: []string{"kind/bug"}, CreatedAt: now, URL: "u1"},
					{Repo: "acme/other", Number: 2, Title: "elsewhere", Author: "bob", CreatedAt: now, URL: "u2"},
				},
			},
			PRs: github.PRResult{
				Count: 2,
				Items: []github.PullRequest{
					{Repo: "acme/widget", Number: 10, Title: "fix", Author: "carol", Draft: false, CreatedAt: now, URL: "p1", HeadSHA: "sha1"},
					{Repo: "acme/other", Number: 11, Title: "other pr", Author: "dave", CreatedAt: now, URL: "p2"},
				},
			},
		},
	}
	f := newGitHubForgeWithReader(stub, "acme")

	issues, err := f.ListOpenIssues(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	// Filtered to the requested repo.
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1 (filtered)", len(issues))
	}
	if issues[0].Number != 1 || issues[0].Author != "alice" || issues[0].State != "open" {
		t.Errorf("issue = %+v", issues[0])
	}

	crs, err := f.ListOpenChangeRequests(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	if len(crs) != 1 {
		t.Fatalf("got %d change requests, want 1 (filtered)", len(crs))
	}
	if crs[0].Number != 10 || crs[0].HeadSHA != "sha1" || crs[0].State != "open" {
		t.Errorf("cr = %+v", crs[0])
	}

	// Empty repo filter returns everything.
	allIssues, _ := f.ListOpenIssues(context.Background(), "")
	if len(allIssues) != 2 {
		t.Errorf("unfiltered issues = %d, want 2", len(allIssues))
	}

	// Empty repo filter also returns all change requests.
	allCRs, _ := f.ListOpenChangeRequests(context.Background(), "")
	if len(allCRs) != 2 {
		t.Errorf("unfiltered change requests = %d, want 2", len(allCRs))
	}
}

// errStub is an underlying-client error injected to exercise the adapter's
// error-wrapping branches.
var errStub = errors.New("boom from underlying client")

// TestGitHubForgeGetRepoError verifies GetRepo wraps the underlying client
// error rather than panicking.
func TestGitHubForgeGetRepoError(t *testing.T) {
	f := newGitHubForgeWithReader(&stubGitHub{err: errStub}, "acme")
	_, err := f.GetRepo(context.Background(), "acme/widget")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errStub) {
		t.Errorf("error should wrap underlying error, got %v", err)
	}
}

// TestGitHubForgeGetRepoNil covers the branch where the underlying client
// returns a nil repository with no error.
func TestGitHubForgeGetRepoNil(t *testing.T) {
	f := newGitHubForgeWithReader(&stubGitHub{repo: nil}, "acme")
	_, err := f.GetRepo(context.Background(), "acme/widget")
	if err == nil {
		t.Fatal("expected error for nil repository, got nil")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got %v", err)
	}
}

// TestGitHubForgeListErrors covers the error-wrapping branches on the list
// operations.
func TestGitHubForgeListErrors(t *testing.T) {
	f := newGitHubForgeWithReader(&stubGitHub{err: errStub}, "acme")

	if _, err := f.ListOpenIssues(context.Background(), "acme/widget"); err == nil {
		t.Error("ListOpenIssues: expected error")
	} else if !errors.Is(err, errStub) {
		t.Errorf("ListOpenIssues error should wrap underlying, got %v", err)
	}

	if _, err := f.ListOpenChangeRequests(context.Background(), "acme/widget"); err == nil {
		t.Error("ListOpenChangeRequests: expected error")
	} else if !errors.Is(err, errStub) {
		t.Errorf("ListOpenChangeRequests error should wrap underlying, got %v", err)
	}
}

// TestGitHubForgeNilResult covers the branch where EnumerateActionable returns a
// nil result with no error: the adapter returns nil, nil.
func TestGitHubForgeNilResult(t *testing.T) {
	f := newGitHubForgeWithReader(&stubGitHub{result: nil}, "acme")

	issues, err := f.ListOpenIssues(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if issues != nil {
		t.Errorf("issues = %v, want nil for nil result", issues)
	}

	crs, err := f.ListOpenChangeRequests(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	if crs != nil {
		t.Errorf("change requests = %v, want nil for nil result", crs)
	}
}

// TestNewGitHubForgeReal constructs the real GitHub adapter (no network is
// touched by construction) to cover newGitHubForge and its Kind.
func TestNewGitHubForgeReal(t *testing.T) {
	f := newGitHubForge("test-token", Options{Org: "acme", BaseURL: ""})
	if f.Kind() != KindGitHub {
		t.Errorf("Kind() = %q, want %q", f.Kind(), KindGitHub)
	}
	if f.org != "acme" {
		t.Errorf("org = %q, want acme", f.org)
	}
}
