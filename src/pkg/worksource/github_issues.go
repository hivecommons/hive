package worksource

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// githubIssuesSource is the default WorkSource: it delegates to the existing
// github.Client.EnumerateActionable, which handles hold labels, issue filters,
// SLA tracking, and PR exclusion. Zero config change for existing hives.
type githubIssuesSource struct {
	client *github.Client
}

// NewGitHubIssuesSource builds a WorkSource backed by the existing GitHub
// Issues enumeration.
func NewGitHubIssuesSource(client *github.Client) WorkSource {
	return &githubIssuesSource{client: client}
}

func (s *githubIssuesSource) SourceType() string { return "github" }

func (s *githubIssuesSource) ListIssues(ctx context.Context) ([]Issue, error) {
	res, err := s.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("worksource/github: enumerate: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]Issue, 0, len(res.Issues.Items))
	for _, it := range res.Issues.Items {
		out = append(out, Issue{
			SourceType: "github",
			Repo:       it.Repo,
			ExternalID: strconv.Itoa(it.Number),
			Number:     it.Number,
			Title:      it.Title,
			Author:     it.Author,
			Labels:     it.Labels,
			Assignees:  it.Assignees,
			State:      "open",
			CreatedAt:  it.CreatedAt,
			UpdatedAt:  it.UpdatedAt,
			URL:        it.URL,
		})
	}
	return out, nil
}

func (s *githubIssuesSource) AddLabel(ctx context.Context, ref Ref, label string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("worksource/github: client unavailable")
	}
	if !ref.IsGitHubIssue() {
		return fmt.Errorf("worksource/github: labels require a GitHub issue ref")
	}
	if err := s.allowWriteback(ctx, ref, writeback.KindStageLabel, label); err != nil {
		return err
	}
	return s.client.AddLabels(ctx, ref.Repo, ref.Number, []string{label}) // writeback:gated
}

func (s *githubIssuesSource) RemoveLabel(ctx context.Context, ref Ref, label string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("worksource/github: client unavailable")
	}
	if !ref.IsGitHubIssue() {
		return fmt.Errorf("worksource/github: labels require a GitHub issue ref")
	}
	if err := s.allowWriteback(ctx, ref, writeback.KindStageLabel, label); err != nil {
		return err
	}
	return s.client.RemoveLabel(ctx, ref.Repo, ref.Number, label) // writeback:gated
}

func (s *githubIssuesSource) AddComment(ctx context.Context, ref Ref, body string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("worksource/github: client unavailable")
	}
	if !ref.IsGitHubIssue() {
		return fmt.Errorf("worksource/github: comments require a GitHub issue ref")
	}
	if err := s.allowWriteback(ctx, ref, writeback.KindGateComment, body); err != nil {
		return err
	}
	return s.client.CreateIssueComment(ctx, ref.Repo, ref.Number, body) // writeback:gated
}

func (s *githubIssuesSource) allowWriteback(ctx context.Context, ref Ref, kind writeback.Kind, preview string) error {
	if SolicitedWritebackActor(ctx, ref) != "" {
		if _, paused := s.client.WritebackGate().RepoPaused(ref.Repo); paused {
			d := s.client.WritebackAllowed(ref.Repo, writeback.Item{Number: ref.Number}, kind, preview)
			return fmt.Errorf("%w: %s", writeback.ErrSuppressed, d.Detail)
		}
		if s.client.RepoIsPaused(ref.Repo) {
			return fmt.Errorf("%w: %s is paused by the operator", writeback.ErrSuppressed, ref.Repo)
		}
		return nil
	}
	item, err := s.client.WritebackItemForNumber(ctx, ref.Repo, ref.Number)
	if err != nil {
		return err
	}
	if d := s.client.WritebackAllowed(ref.Repo, item, kind, preview); !d.Allowed {
		return fmt.Errorf("%w: %s", writeback.ErrSuppressed, d.Detail)
	}
	return nil
}
