package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/github"
)

type RunAdmitter interface {
	AdmitTriagedRun(repo string, number int, title, verdict, rationale string, now time.Time) error
}

type TriageCommenter interface {
	IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error)
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

type runTriageFixRetirer interface {
	RunTriageFixRetired(repo string, number int) bool
}

const triageCommentMarker = "<!-- hive-triage -->"

func (s *Scheduler) applyRunTriage(issues []github.Issue) []github.Issue {
	if s == nil || s.cfg == nil || !s.cfg.Runs.Triage.Enabled || len(issues) == 0 {
		return issues
	}
	admitter, commenter := s.runTriageDeps()
	out := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		c := classify.Classification{
			Tier:  classify.Tier(issue.ComplexityTier),
			Model: classify.ModelRecommendation(issue.ModelRec),
			Lane:  classify.Lane(issue.Lane),
		}
		decision := classify.Triage(issue, c, s.cfg.Runs.Triage)
		switch decision.Verdict {
		case classify.TriageSpec:
			if retired, ok := admitter.(runTriageFixRetirer); ok && retired.RunTriageFixRetired(issue.Repo, issue.Number) && !issueHasLabel(issue, "run/spec") {
				out = append(out, issue)
				continue
			}
			if admitter == nil {
				out = append(out, issue)
				continue
			}
			if err := admitter.AdmitTriagedRun(issue.Repo, issue.Number, issue.Title, string(decision.Verdict), decision.Rationale, time.Now()); err != nil {
				if s.logger != nil {
					s.logger.Warn("runs triage admission failed", "repo", issue.Repo, "number", issue.Number, "error", err)
				}
				out = append(out, issue)
			}
		case classify.TriageClarify:
			s.postTriageClarifyComment(commenter, issue, decision)
		default:
			out = append(out, issue)
		}
	}
	return out
}

func issueHasLabel(issue github.Issue, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, label := range issue.Labels {
		if strings.ToLower(strings.TrimSpace(label)) == want {
			return true
		}
	}
	return false
}

func (s *Scheduler) postTriageClarifyComment(commenter TriageCommenter, issue github.Issue, decision classify.TriageDecision) {
	if commenter == nil || !s.cfg.Runs.Triage.ShouldClarifyComment() || issue.Repo == "" || issue.Number <= 0 {
		return
	}
	ctx := context.Background()
	seen, err := commenter.IssueCommentsContain(ctx, issue.Repo, issue.Number, triageCommentMarker)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("runs triage comment scan failed", "repo", issue.Repo, "number", issue.Number, "error", err)
		}
		return
	}
	if seen {
		return
	}
	body := fmt.Sprintf("%s\nhive-triage: this issue needs one more detail before Hive can route it. %s Please update the issue body with the missing context, expected behavior, and acceptance criteria.", triageCommentMarker, decision.Rationale)
	if err := commenter.CreateIssueComment(ctx, issue.Repo, issue.Number, body); err != nil && s.logger != nil {
		s.logger.Warn("runs triage comment failed", "repo", issue.Repo, "number", issue.Number, "error", err)
	}
}
