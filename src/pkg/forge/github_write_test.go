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
