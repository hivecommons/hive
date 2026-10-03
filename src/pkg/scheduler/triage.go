package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

type RunAdmitter interface {
	AdmitTriagedRun(repo string, number int, title, verdict, rationale string, now time.Time) error
}

type RunAdmitterRefContext interface {
	AdmitTriagedRunRefWithContext(ref worksource.Ref, ctx worksource.WorkItemContext, verdict, rationale string, now time.Time) error
}

type TriageCommenter interface {
	IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error)
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

func (s *Scheduler) runTriageDeps() (RunAdmitter, TriageCommenter) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runAdmitter, s.triageCommenter
}

type runAbandonmentReader interface {
	RunAbandoned(key string) bool
}

type runTriageFixRetirer interface {
	RunTriageFixRetired(repo string, number int) bool
}

const triageCommentMarker = "<!-- hive-triage -->"

func (s *Scheduler) applyRunTriage(ctx context.Context, issues []github.Issue) []github.Issue {
	if s == nil || s.cfg == nil || !s.cfg.Runs.Triage.Enabled || len(issues) == 0 {
		return issues
	}
	admitter, commenter := s.runTriageDeps()
	out := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if retired, ok := admitter.(runAbandonmentReader); ok && retired.RunAbandoned(worksource.Ref{
			SourceType: issue.SourceType, Repo: issue.Repo, Number: issue.Number, ExternalID: issue.ExternalID,
		}.Key()) {
			continue
		}
		c := classify.Classification{
			Tier:  classify.Tier(issue.ComplexityTier),
			Model: classify.ModelRecommendation(issue.ModelRec),
			Lane:  classify.Lane(issue.Lane),
		}
		decision := classify.Triage(ctx, issue, c, s.cfg.Runs.Triage)
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
			var err error
			if refAdmitter, ok := admitter.(RunAdmitterRefContext); ok {
				err = refAdmitter.AdmitTriagedRunRefWithContext(worksource.Ref{
					SourceType: issue.SourceType,
					Repo:       issue.Repo,
					ExternalID: issue.ExternalID,
					Number:     issue.Number,
					URL:        issue.URL,
				}, worksource.WorkItemContextFromGitHubIssue(issue), string(decision.Verdict), decision.Rationale, time.Now())
			} else {
				err = admitter.AdmitTriagedRun(issue.Repo, issue.Number, issue.Title, string(decision.Verdict), decision.Rationale, time.Now())
			}
			if err != nil {
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
