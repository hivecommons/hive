package worksource

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	SourceTypeRun = "run"

	RunStageSpec      = "spec"
	RunStagePlan      = "plan"
	RunStageImplement = "implement"
)

// RunStage describes one pending run stage as exposed by the lease registry
// seam. The registry itself lands in #8297; this issue only defines the read
// contract and a source that can be driven by a stub until then.
type RunStage struct {
	RunKey    string
	Stage     string
	Title     string
	Repo      string
	Author    string
	Priority  string
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunStageLeaseAccessor is the narrow #8297 integration seam. PendingRunStages
// returns the registry's current pending stage per run; StageHasLiveLease
// reports whether that exact stage is already leased and should not be listed.
type RunStageLeaseAccessor interface {
	PendingRunStages(ctx context.Context) ([]RunStage, error)
	StageHasLiveLease(ctx context.Context, runKey, stage string) (bool, error)
}

type RunStageSource struct {
	leases RunStageLeaseAccessor
}

func NewRunStageSource(leases RunStageLeaseAccessor) *RunStageSource {
	return &RunStageSource{leases: leases}
}

func (s *RunStageSource) SourceType() string { return SourceTypeRun }

func (s *RunStageSource) ListIssues(ctx context.Context) ([]Issue, error) {
	if s == nil || s.leases == nil {
		return []Issue{}, nil
	}
	stages, err := s.leases.PendingRunStages(ctx)
	if err != nil {
		return nil, fmt.Errorf("worksource/run: pending stages: %w", err)
	}
	out := make([]Issue, 0, len(stages))
	for _, stage := range stages {
		stage.Stage = strings.TrimSpace(strings.ToLower(stage.Stage))
		stage.RunKey = strings.TrimSpace(stage.RunKey)
		stage.Repo = strings.TrimSpace(stage.Repo)
		if stage.RunKey == "" || stage.Stage == "" || stage.Repo == "" {
			continue
		}
		live, err := s.leases.StageHasLiveLease(ctx, stage.RunKey, stage.Stage)
		if err != nil {
			return nil, fmt.Errorf("worksource/run: live lease %s:%s: %w", stage.RunKey, stage.Stage, err)
		}
		if live {
			continue
		}
		out = append(out, runStageIssue(stage))
	}
	return out, nil
}

func runStageIssue(stage RunStage) Issue {
	externalID := stage.RunKey + ":" + stage.Stage
	title := stage.Title
	if title == "" {
		title = stage.RunKey
	}
	issue := Issue{
		SourceType: SourceTypeRun,
		Repo:       stage.Repo,
		ExternalID: externalID,
		Number:     0,
		Title:      stage.Stage + ": " + title,
		Author:     stage.Author,
		Labels:     []string{"hive-run", "stage/" + stage.Stage},
		IsTracker:  false,
		Priority:   stage.Priority,
		State:      "open",
		CreatedAt:  stage.CreatedAt,
		UpdatedAt:  stage.UpdatedAt,
		URL:        stage.URL,
	}
	if prev, ok := previousRunStage(stage.Stage); ok {
		issue.DependsOn = []Dependency{{
			Ref: Ref{
				SourceType: SourceTypeRun,
				Repo:       stage.Repo,
				ExternalID: stage.RunKey + ":" + prev,
			},
			Resolved: true,
		}}
	}
	return issue
}

func previousRunStage(stage string) (string, bool) {
	switch stage {
	case RunStagePlan:
		return RunStageSpec, true
	case RunStageImplement:
		return RunStagePlan, true
	default:
		return "", false
	}
}
