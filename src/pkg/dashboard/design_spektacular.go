package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
		if err := s.applyDesignSignal(ctx, issue, s.designConfig().DesignLabelOrDefault(), s.designRequestedStatus()); err != nil {
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
	if err := s.AdmitRunRef(issueWorkRef(issue), strings.TrimSpace(issue.Title), time.Now()); err != nil {
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

func (s *Server) applyDesignSignal(ctx context.Context, issue github.Issue, label, status string) error {
	label = strings.TrimSpace(label)
	status = strings.TrimSpace(status)
	ref := issueWorkRef(issue)
	if label != "" {
		mut, err := s.designLabelMutator(issue)
		if err != nil {
			return err
		}
		if err := mut.AddLabel(ctx, ref, label); err != nil {
			return err
		}
	}
	if status == "" {
		return nil
	}
	transitioner, err := s.designStatusTransitioner(issue)
	if err != nil {
		return err
	}
	if err := transitioner.TransitionStatus(ctx, ref, status); err != nil {
		if errors.Is(err, worksource.ErrStatusTransitionUnsupported) {
			return fmt.Errorf("design status transition %q unsupported for %s: %w", status, designRunKey(issue), err)
		}
		return err
	}
	return nil
}

func (s *Server) postDesignArtifact(ctx context.Context, store *beads.Store, epic *beads.Bead, digest, body string) error {
	body = strings.TrimSpace(body)
	digest = strings.TrimSpace(digest)
	if store == nil || epic == nil || body == "" {
		return nil
	}
	if digest != "" && epic.Meta(planning.MetaDesignArtifactDigest) == digest {
		return nil
	}
	issue := issueFromEpic(epic)
	commenter, err := s.designCommenter(issue)
	if err != nil {
		return err
	}
	if err := commenter.AddComment(ctx, issueWorkRef(issue), designArtifactComment(body)); err != nil {
		return err
	}
	if digest != "" {
		return store.SetMetadata(epic.ID, planning.MetaDesignArtifactDigest, digest)
	}
	return nil
}

func designArtifactComment(body string) string {
	return strings.TrimSpace("📐 Spektacular design artifact\n\n" + strings.TrimSpace(body))
}

func designArtifactDigest(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Server) designRequestedStatus() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	return strings.TrimSpace(s.deps.Config.Planning.DesignRequestedStatus)
}

func (s *Server) designApprovedStatus() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	return strings.TrimSpace(s.deps.Config.Planning.DesignApprovedStatus)
}

func issueWorkRef(issue github.Issue) worksource.Ref {
	externalID := issue.ExternalID
	if externalID == "" && issue.Number > 0 {
		externalID = strconv.Itoa(issue.Number)
	}
	return worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: externalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}
}

func (s *Server) designLabelMutator(issue github.Issue) (worksource.LabelMutator, error) {
	src, err := s.designWorkSource(issue)
	if err != nil {
		return nil, err
	}
	mut, ok := src.(worksource.LabelMutator)
	if !ok {
		return nil, fmt.Errorf("worksource/%s: labels unsupported", src.SourceType())
	}
	return mut, nil
}

func (s *Server) designCommenter(issue github.Issue) (worksource.Commenter, error) {
	src, err := s.designWorkSource(issue)
	if err != nil {
		return nil, err
	}
	commenter, ok := src.(worksource.Commenter)
	if !ok {
		return nil, fmt.Errorf("worksource/%s: comments unsupported", src.SourceType())
	}
	return commenter, nil
}

func (s *Server) designStatusTransitioner(issue github.Issue) (worksource.StatusTransitioner, error) {
	src, err := s.designWorkSource(issue)
	if err != nil {
		return nil, err
	}
	transitioner, ok := src.(worksource.StatusTransitioner)
	if !ok {
		return nil, worksource.ErrStatusTransitionUnsupported
	}
	return transitioner, nil
}

func (s *Server) designWorkSource(issue github.Issue) (worksource.WorkSource, error) {
	if s == nil || s.deps == nil {
		return nil, fmt.Errorf("dashboard dependencies unavailable")
	}
	if issue.Number > 0 || issue.SourceType == "" || issue.SourceType == "github" || issue.SourceType == "github_projects" {
		if s.deps.GHClient == nil {
			return nil, fmt.Errorf("github client not initialized")
		}
		return worksource.NewGitHubIssuesSource(s.deps.GHClient), nil
	}
	if s.deps.Config == nil {
		return nil, fmt.Errorf("worksource config unavailable for %s", designRunKey(issue))
	}
	cfg := s.deps.Config.Governor.WorkSource
	if cfg.Type != issue.SourceType {
		return nil, fmt.Errorf("worksource/%s not configured (active work_source=%q)", issue.SourceType, cfg.Type)
	}
	logger := slog.Default()
	if s.logger != nil {
		logger = s.logger
	}
	return worksource.FromConfig(cfg, s.deps.GHClient, "", "", logger)
}

func issueFromEpic(epic *beads.Bead) github.Issue {
	n, _ := strconv.Atoi(epic.Meta(planning.MetaIssueNumber))
	return github.Issue{
		SourceType: firstRunNonEmpty(epic.Meta(planning.MetaIssueSourceType), "github"),
		Repo:       epic.Meta(planning.MetaIssueRepo),
		Number:     n,
		ExternalID: firstRunNonEmpty(epic.Meta(planning.MetaIssueExternalID), epic.Meta(planning.MetaIssueNumber)),
		URL:        epic.Meta(planning.MetaIssueURL),
		Title:      epic.Title,
	}
}
