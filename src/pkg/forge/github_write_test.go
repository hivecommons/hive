package forge

import (
	"context"
	"errors"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// stubGitHubWriter records write calls and can inject an error. It implements
// githubWriter for the GitHub adapter write-path tests without any network.
type stubGitHubWriter struct {
	err error

	commentOwner, commentRepo, commentBody string
	commentNum                             int

	addOwner, addRepo string
	addNum            int
	addLabels         []string

	removeOwner, removeRepo, removeLabel string
	removeNum                            int

	createOwner, createRepo string
	createReq               *gh.IssueRequest
	created                 int

	listed []*gh.Issue
	pages  int
}

func (s *stubGitHubWriter) Create(ctx context.Context, owner, repo string, issue *gh.IssueRequest) (*gh.Issue, *gh.Response, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	s.createOwner, s.createRepo, s.createReq = owner, repo, issue
	s.created++
	return &gh.Issue{Number: gh.Ptr(77), HTMLURL: gh.Ptr("https://example.test/" + owner + "/" + repo + "/issues/77")}, &gh.Response{}, nil
}

func (s *stubGitHubWriter) ListByRepo(ctx context.Context, owner, repo string, opts *gh.IssueListByRepoOptions) ([]*gh.Issue, *gh.Response, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	s.pages++
	return s.listed, &gh.Response{}, nil
}

func (s *stubGitHubWriter) CreateComment(ctx context.Context, owner, repo string, number int, comment *gh.IssueComment) (*gh.IssueComment, *gh.Response, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	s.commentOwner, s.commentRepo, s.commentNum = owner, repo, number
	s.commentBody = comment.GetBody()
	return comment, &gh.Response{}, nil
}

func (s *stubGitHubWriter) AddLabelsToIssue(ctx context.Context, owner, repo string, number int, labels []string) ([]*gh.Label, *gh.Response, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	s.addOwner, s.addRepo, s.addNum, s.addLabels = owner, repo, number, labels
	return nil, &gh.Response{}, nil
}

func (s *stubGitHubWriter) RemoveLabelForIssue(ctx context.Context, owner, repo string, number int, label string) (*gh.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.removeOwner, s.removeRepo, s.removeNum, s.removeLabel = owner, repo, number, label
	return &gh.Response{}, nil
}

// newGitHubForgeWithWriter builds an adapter with an injected writer stub.
func newGitHubForgeWithWriter(w githubWriter, org string) *gitHubForge {
	return &gitHubForge{writer: w, org: org}
}

func TestGitHubCreateIssueComment(t *testing.T) {
	w := &stubGitHubWriter{}
	f := newGitHubForgeWithWriter(w, "acme")
	if err := f.CreateIssueComment(context.Background(), "widget", 5, "hi"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if w.commentOwner != "acme" || w.commentRepo != "widget" || w.commentNum != 5 {
		t.Errorf("comment target = %s/%s#%d", w.commentOwner, w.commentRepo, w.commentNum)
	}
	if w.commentBody != "hi" {
		t.Errorf("comment body = %q", w.commentBody)
	}
}

func TestGitHubAddLabels(t *testing.T) {
	w := &stubGitHubWriter{}
	f := newGitHubForgeWithWriter(w, "acme")

	// Empty labels => no-op, no delegation.
	if err := f.AddLabels(context.Background(), "acme/widget", 5, nil); err != nil {
		t.Fatalf("AddLabels(empty): %v", err)
	}
	if w.addLabels != nil {
		t.Errorf("empty AddLabels delegated: %v", w.addLabels)
	}

	if err := f.AddLabels(context.Background(), "acme/widget", 5, []string{"kind/bug"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if w.addOwner != "acme" || w.addRepo != "widget" || w.addNum != 5 {
		t.Errorf("add target = %s/%s#%d", w.addOwner, w.addRepo, w.addNum)
	}
	if len(w.addLabels) != 1 || w.addLabels[0] != "kind/bug" {
		t.Errorf("add labels = %v", w.addLabels)
	}
}

func TestGitHubRemoveLabel(t *testing.T) {
	w := &stubGitHubWriter{}
	f := newGitHubForgeWithWriter(w, "acme")
	if err := f.RemoveLabel(context.Background(), "acme/widget", 5, "hold"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if w.removeOwner != "acme" || w.removeRepo != "widget" || w.removeNum != 5 || w.removeLabel != "hold" {
		t.Errorf("remove = %s/%s#%d label=%q", w.removeOwner, w.removeRepo, w.removeNum, w.removeLabel)
	}
}

func TestGitHubSetHold(t *testing.T) {
	w := &stubGitHubWriter{}
	f := newGitHubForgeWithWriter(w, "acme")

	if err := f.SetHold(context.Background(), "acme/widget", 5, true); err != nil {
		t.Fatalf("SetHold(true): %v", err)
	}
	if len(w.addLabels) != 1 || w.addLabels[0] != holdLabel {
		t.Errorf("SetHold(true) added %v, want [%s]", w.addLabels, holdLabel)
	}

	if err := f.SetHold(context.Background(), "acme/widget", 5, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}
	if w.removeLabel != holdLabel {
		t.Errorf("SetHold(false) removed %q, want %s", w.removeLabel, holdLabel)
	}
}

func TestGitHubWriteErrors(t *testing.T) {
	sentinel := errors.New("boom write")
	w := &stubGitHubWriter{err: sentinel}
	f := newGitHubForgeWithWriter(w, "acme")
	ctx := context.Background()

	if err := f.CreateIssueComment(ctx, "acme/widget", 5, "x"); err == nil {
		t.Error("CreateIssueComment: expected error")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("comment error should wrap sentinel: %v", err)
	}
	if err := f.AddLabels(ctx, "acme/widget", 5, []string{"x"}); err == nil {
		t.Error("AddLabels: expected error")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("add error should wrap sentinel: %v", err)
	}
	if err := f.RemoveLabel(ctx, "acme/widget", 5, "x"); err == nil {
		t.Error("RemoveLabel: expected error")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("remove error should wrap sentinel: %v", err)
	}
	// SetHold surfaces the underlying add/remove error too.
	if err := f.SetHold(ctx, "acme/widget", 5, true); err == nil {
		t.Error("SetHold(true): expected error")
	}
	if err := f.SetHold(ctx, "acme/widget", 5, false); err == nil {
		t.Error("SetHold(false): expected error")
	}
}

func TestGitHubCreateIssueAndMarkerLookup(t *testing.T) {
	w := &stubGitHubWriter{}
	f := newGitHubForgeWithWriter(w, "acme")
	ctx := context.Background()

	ref, err := f.CreateIssue(ctx, "widget", "finding: auth", "body <!-- hive-finding: abc -->", []string{"audit"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ref.Number != 77 || ref.URL == "" || w.created != 1 {
		t.Fatalf("create ref = %+v created=%d", ref, w.created)
	}
	if w.createOwner != "acme" || w.createRepo != "widget" || w.createReq.GetTitle() != "finding: auth" || len(w.createReq.GetLabels()) != 1 {
		t.Fatalf("create request = %s/%s %+v", w.createOwner, w.createRepo, w.createReq)
	}

	w.listed = []*gh.Issue{
		{Number: gh.Ptr(1), Body: gh.Ptr("unrelated"), PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("pr")}},
		{Number: gh.Ptr(2), Body: gh.Ptr("carries <!-- hive-finding: abc --> marker"), HTMLURL: gh.Ptr("u2")},
	}
	got, ok, err := f.FindIssueByMarker(ctx, "acme/widget", "<!-- hive-finding: abc -->")
	if err != nil || !ok || got.Number != 2 || got.URL != "u2" {
		t.Fatalf("marker lookup = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, err := f.FindIssueByMarker(ctx, "acme/widget", "<!-- hive-finding: zzz -->"); err != nil || ok {
		t.Fatalf("absent marker must report ok=false without error, got ok=%v err=%v", ok, err)
	}
	if _, _, err := f.FindIssueByMarker(ctx, "acme/widget", ""); err == nil {
		t.Fatal("empty marker must be refused")
	}
}

func TestGitHubIssueSeamErrors(t *testing.T) {
	sentinel := errors.New("boom issue")
	f := newGitHubForgeWithWriter(&stubGitHubWriter{err: sentinel}, "acme")
	ctx := context.Background()
	if _, err := f.CreateIssue(ctx, "acme/widget", "t", "b", nil); !errors.Is(err, sentinel) {
		t.Fatalf("create error should wrap sentinel: %v", err)
	}
	if _, _, err := f.FindIssueByMarker(ctx, "acme/widget", "m"); !errors.Is(err, sentinel) {
		t.Fatalf("lookup error should wrap sentinel: %v", err)
	}
	bare := &gitHubForge{org: "acme"}
	if _, err := bare.CreateIssue(ctx, "acme/widget", "t", "b", nil); err == nil {
		t.Fatal("create without a writer must fail")
	}
	if _, _, err := bare.FindIssueByMarker(ctx, "acme/widget", "m"); err == nil {
		t.Fatal("lookup without a writer must fail")
	}
	if NewGitHubIssueSeam(nil, "acme") != nil {
		t.Fatal("nil client must yield no seam")
	}
}
