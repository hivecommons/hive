package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

func (s *Server) spektacularDesignEnabled() bool {
	return s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.Runs.Spektacular.Enabled
}

func designRunKey(issue github.Issue) string {
	return worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}.Key()
}

func (s *Server) startDesignSpektacular(ctx context.Context, store *beads.Store, issue github.Issue, body string, applySignal bool) (*beads.Bead, string, error) {
	if !s.spektacularDesignEnabled() {
		return nil, "", fmt.Errorf("spektacular design mode is disabled")
	}

	runKey := designRunKey(issue)
	if runKey == "" || issue.Repo == "" {
		return nil, "", fmt.Errorf("design run requires a stable work item ref")
	}
	if applySignal {
		if err := s.applyDesignLabel(ctx, issue, s.designConfig().DesignLabelOrDefault()); err != nil {
			return nil, "", err
		}
	}
	epic, err := planning.EpicFromIssue(store, issue, body)
	if err != nil {
		return nil, "", err
	}
	if err := planning.RequestDesign(store, epic.ID); err != nil {
		return nil, "", err
	}
	for key, value := range map[string]string{
		planning.MetaDesignVia: planning.DesignViaSpektacular,
		planning.MetaRunKey:    runKey,
	} {
		if err := store.SetMetadata(epic.ID, key, value); err != nil {
			return nil, "", err
		}
	}
	_ = store.SetMetadata(epic.ID, planning.MetaDesignStatus, planning.DesignStatusRequested)
	if issue.Number <= 0 {
		return nil, "", fmt.Errorf("spektacular admission currently requires a GitHub issue number")
	}
	if err := s.AdmitRun(issue.Repo, issue.Number, strings.TrimSpace(issue.Title), time.Now()); err != nil {
		return nil, "", err
	}
	epic, _ = store.Get(epic.ID)
	return epic, runKey, nil
}

// StartDesignSpektacularFromIssue is the label-triage entry point used by the
// governor loop. It starts (or finds) the same Spektacular-backed design run as
// the dashboard and chat paths, but does not re-apply the label that caused the
// call.
func (s *Server) StartDesignSpektacularFromIssue(ctx context.Context, store *beads.Store, issue github.Issue) (*beads.Bead, string, error) {
	return s.startDesignSpektacular(ctx, store, issue, "", false)
}

func (s *Server) applyDesignLabel(ctx context.Context, issue github.Issue, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil
	}
	if issue.Number <= 0 {
		return fmt.Errorf("design label requires a source adapter for %s", designRunKey(issue))
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return fmt.Errorf("github client not initialized")
	}
	return s.deps.GHClient.AddLabels(ctx, issue.Repo, issue.Number, []string{label})
}

func issueFromEpic(epic *beads.Bead) github.Issue {
	n, _ := strconv.Atoi(epic.Meta(planning.MetaIssueNumber))
	return github.Issue{
		Repo:       epic.Meta(planning.MetaIssueRepo),
		Number:     n,
		ExternalID: firstRunNonEmpty(epic.Meta(planning.MetaIssueNumber), epic.ExternalRef),
		URL:        epic.Meta(planning.MetaIssueURL),
		Title:      epic.Title,
	}
}
