package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type fakeRunTriageDeps struct {
	admitted []github.Issue
	comments []string
	seen     bool
}

func (f *fakeRunTriageDeps) AdmitTriagedRun(repo string, number int, title, verdict, rationale string, now time.Time) error {
	f.admitted = append(f.admitted, github.Issue{Repo: repo, Number: number, Title: title, ComplexityTier: verdict, Body: rationale})
	return nil
}

func (f *fakeRunTriageDeps) IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error) {
	return f.seen, nil
}

func (f *fakeRunTriageDeps) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

func TestRunTriageAdmitsClarifiesAndKeepsFixes(t *testing.T) {
	cfg := &config.Config{
		Runs: config.RunsConfig{Triage: config.TriageConfig{Enabled: true}, Spektacular: config.SpektacularConfig{Enabled: true}},
	}
	s := New(cfg, slog.Default())
	deps := &fakeRunTriageDeps{}
	s.SetRunTriageDeps(deps, deps)
	body := "This issue has enough detail to describe the change, acceptance criteria, risks, and expected behaviour."
	actionable := actionableWithIssues([]github.Issue{
		{Repo: "o/r", Number: 1, Title: "new workflow", Labels: []string{"kind/feature"}, Body: body},
		{Repo: "o/r", Number: 2, Title: "typo in docs", Body: body},
		{Repo: "o/r", Number: 3, Title: "unclear", Body: "short"},
	})
	msgs := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(deps.admitted) != 1 || deps.admitted[0].Number != 1 || deps.admitted[0].ComplexityTier != "spec" {
		t.Fatalf("admitted = %+v", deps.admitted)
	}
	if len(deps.comments) != 1 || !strings.Contains(deps.comments[0], triageCommentMarker) || !strings.Contains(deps.comments[0], "hive-triage:") {
		t.Fatalf("comments = %+v", deps.comments)
	}
	if len(msgs) != 1 || len(msgs[0].IssueRefs) != 1 || msgs[0].IssueRefs[0] != "o/r#2" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestRunTriageClarifyCommentDedupes(t *testing.T) {
	cfg := &config.Config{Runs: config.RunsConfig{Triage: config.TriageConfig{Enabled: true}}}
	s := New(cfg, slog.Default())
	deps := &fakeRunTriageDeps{seen: true}
	s.SetRunTriageDeps(nil, deps)
	actionable := actionableWithIssues([]github.Issue{{Repo: "o/r", Number: 3, Title: "unclear", Body: "short"}})
	msgs := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(deps.comments) != 0 {
		t.Fatalf("comments = %+v, want none", deps.comments)
	}
	if len(msgs) != 1 || len(msgs[0].IssueRefs) != 0 {
		t.Fatalf("clarify issue should be skipped: %+v", msgs)
	}
}
