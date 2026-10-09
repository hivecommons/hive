package worksource

// Review backlog sinks (hivecommons/hive#11089): where the review backlog
// filer in pkg/github sends below-the-line findings when review.backlog
// names a destination other than plain GitHub issues. Each sink is a thin
// adapter over the work-source client for that tracker, so credentials,
// endpoints and TLS settings come from governor.work_source.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// NewReviewBacklogSink returns the sink for review.backlog with the active
// work source. github_issue, and any destination whose work source is not
// active (config.ReviewBacklogConfig.EffectiveDestination), get ghClient's
// GitHub issue sink. When the destination's work source is active but cannot
// be built, the GitHub issue sink is returned together with the error, so the
// caller can log it and still file.
func NewReviewBacklogSink(ws config.WorkSourceConfig, backlog config.ReviewBacklogConfig, ghClient *github.Client, ghToken, ghOrg string, logger *slog.Logger) (github.ReviewBacklogSink, error) {
	fallback := ghClient.ReviewBacklogIssueSink()
	dest := backlog.EffectiveDestination(ws.Type)
	if dest == config.ReviewBacklogGitHubIssue {
		return fallback, nil
	}
	src, err := reviewBacklogWorkSource(ws, ghClient, ghToken, ghOrg, logger)
	if err != nil {
		return fallback, fmt.Errorf("review.backlog.destination %s: %w; filing GitHub issues instead", dest, err)
	}
	switch dest {
	case config.ReviewBacklogGitHubProject:
		p, ok := src.(*githubProjectsSource)
		col := strings.TrimSpace(backlog.ProjectColumnID)
		if !ok || col == "" {
			return fallback, fmt.Errorf("review.backlog.destination %s needs a github_projects work source and project_column_id; filing GitHub issues instead", dest)
		}
		return &projectBacklogSink{issues: fallback, projects: p, columnID: col, org: ghOrg}, nil
	case config.ReviewBacklogLinear:
		l, ok := src.(*LinearSource)
		if !ok {
			return fallback, fmt.Errorf("review.backlog.destination %s needs a linear work source; filing GitHub issues instead", dest)
		}
		return &linearBacklogSink{src: l, state: strings.TrimSpace(backlog.LinearState)}, nil
	case config.ReviewBacklogJira:
		j, ok := src.(*jiraSource)
		if !ok {
			return fallback, fmt.Errorf("review.backlog.destination %s needs a jira work source; filing GitHub issues instead", dest)
		}
		return &jiraBacklogSink{src: j, status: strings.TrimSpace(backlog.JiraStatus)}, nil
	}
	return fallback, nil
}

// reviewBacklogWorkSource builds only the primary work-source client: the
// additive sources are irrelevant to writing, and assigned_only narrows
// enumeration, not where new items can be filed.
func reviewBacklogWorkSource(ws config.WorkSourceConfig, ghClient *github.Client, ghToken, ghOrg string, logger *slog.Logger) (WorkSource, error) {
	ws.RunStages = false
	ws.Wavefront.Enabled = false
	ws.Linear.AssignedOnly = false
	return FromConfig(ws, ghClient, ghToken, ghOrg, logger)
}

// projectBacklogSink files GitHub issues and places each on the configured
// Projects v2 column. Like the Jira sink, a failure after the item exists
// returns the ref with the error so the filer records it and does not file
// it twice.
type projectBacklogSink struct {
	issues   github.ReviewBacklogSink
	projects *githubProjectsSource
	columnID string
	org      string
}

func (s *projectBacklogSink) Destination() string { return config.ReviewBacklogGitHubProject }

func (s *projectBacklogSink) CreateItem(ctx context.Context, item github.ReviewBacklogItem) (github.ReviewBacklogRef, error) {
	ref, err := s.issues.CreateItem(ctx, item)
	if err != nil || ref.Number <= 0 {
		return ref, err
	}
	owner, repo := splitOwnerRepo(item.Repo, coalesce(s.projects.cfg.Org, s.org))
	if _, err := s.projects.AddIssueToColumn(ctx, owner, repo, ref.Number, s.columnID); err != nil {
		return ref, fmt.Errorf("review backlog: add %s to project column: %w", ref.Key, err)
	}
	return ref, nil
}

func (s *projectBacklogSink) AppendToItem(ctx context.Context, repo string, ref github.ReviewBacklogRef, comment string) error {
	return s.issues.AppendToItem(ctx, repo, ref, comment)
}

func splitOwnerRepo(full, defaultOwner string) (string, string) {
	full = strings.TrimSpace(full)
	if i := strings.Index(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return defaultOwner, full
}

// linearBacklogSink files Linear issues on the team mapped to the PR's repo.
type linearBacklogSink struct {
	src   *LinearSource
	state string
}

func (s *linearBacklogSink) Destination() string { return config.ReviewBacklogLinear }

func (s *linearBacklogSink) CreateItem(ctx context.Context, item github.ReviewBacklogItem) (github.ReviewBacklogRef, error) {
	team, ok := s.src.TeamForRepo(item.Repo)
	if !ok {
		return github.ReviewBacklogRef{}, fmt.Errorf("review backlog: no linear team configured")
	}
	issue, err := s.src.CreateIssueInState(ctx, team.Key, item.Title, item.Body, s.state, item.Labels)
	if err != nil {
		return github.ReviewBacklogRef{}, err
	}
	return github.ReviewBacklogRef{ID: issue.ID, Key: issue.Identifier, URL: issue.URL}, nil
}

func (s *linearBacklogSink) AppendToItem(ctx context.Context, _ string, ref github.ReviewBacklogRef, comment string) error {
	return s.src.CommentOnIssue(ctx, ref.ID, comment)
}

// jiraBacklogSink files Jira Tasks in the first configured project.
type jiraBacklogSink struct {
	src    *jiraSource
	status string
}

func (s *jiraBacklogSink) Destination() string { return config.ReviewBacklogJira }

func (s *jiraBacklogSink) CreateItem(ctx context.Context, item github.ReviewBacklogItem) (github.ReviewBacklogRef, error) {
	if len(s.src.cfg.ProjectKeys) == 0 {
		return github.ReviewBacklogRef{}, fmt.Errorf("review backlog: work_source.jira.project_keys is empty")
	}
	created, err := s.src.createIssue(ctx, s.src.cfg.ProjectKeys[0], item.Title, item.Body, item.Labels)
	if err != nil {
		return github.ReviewBacklogRef{}, err
	}
	ref := github.ReviewBacklogRef{ID: created.Key, Key: created.Key, URL: s.src.browseURL(created.Key)}
	if err := s.src.transitionToStatus(ctx, created.Key, s.status); err != nil {
		return ref, err
	}
	return ref, nil
}

func (s *jiraBacklogSink) AppendToItem(ctx context.Context, _ string, ref github.ReviewBacklogRef, comment string) error {
	return s.src.addComment(ctx, ref.ID, comment)
}
