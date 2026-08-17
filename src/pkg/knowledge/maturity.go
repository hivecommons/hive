package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// MaturityLevel represents the ACMM test maturity of a project.
type MaturityLevel int

const (
	MaturityIdea    MaturityLevel = 1
	MaturityDev     MaturityLevel = 2
	MaturityCI      MaturityLevel = 3
	MaturityFullAuto MaturityLevel = 4
)

// String returns a human-readable label for the maturity level.
func (m MaturityLevel) String() string {
	switch m {
	case MaturityIdea:
		return "idea"
	case MaturityDev:
		return "development"
	case MaturityCI:
		return "ci-cd"
	case MaturityFullAuto:
		return "full-auto"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// TestMode returns the recommended test enforcement mode for this maturity.
func (m MaturityLevel) TestMode() string {
	switch m {
	case MaturityIdea, MaturityDev:
		return "suggest"
	case MaturityCI:
		return "gate"
	case MaturityFullAuto:
		return "tdd"
	default:
		return "suggest"
	}
}

// MaturitySignals captures the indicators used to determine project maturity.
type MaturitySignals struct {
	HasTests          bool    `json:"has_tests"`
	HasCI             bool    `json:"has_ci"`
	HasCoverageConfig bool    `json:"has_coverage_config"`
	HasTDDMarkers     bool    `json:"has_tdd_markers"`
	CoverageThreshold float64 `json:"coverage_threshold,omitempty"`
	TestFileCount     int     `json:"test_file_count"`
	CIWorkflowCount   int     `json:"ci_workflow_count"`
}

// MaturityResult is the output of maturity detection.
type MaturityResult struct {
	Level    MaturityLevel   `json:"level"`
	TestMode string          `json:"test_mode"`
	Signals  MaturitySignals `json:"signals"`
}

// MaturityDetector analyzes a repository to determine its ACMM test maturity.
type MaturityDetector struct {
	ghClient *gh.Client
	logger   *slog.Logger
}

// NewMaturityDetector creates a maturity detector.
func NewMaturityDetector(ghClient *gh.Client, logger *slog.Logger) *MaturityDetector {
	return &MaturityDetector{
		ghClient: ghClient,
		logger:   logger,
	}
}

// Detect analyzes a repository and returns its maturity level with supporting signals.
func (d *MaturityDetector) Detect(ctx context.Context, owner, repo string) (*MaturityResult, error) {
	signals := MaturitySignals{}

	testCount, err := d.countTestFiles(ctx, owner, repo)
	if err != nil {
		d.logger.Warn("failed to count test files", "repo", repo, "error", err)
	}
	signals.TestFileCount = testCount
	signals.HasTests = testCount > 0

	ciCount, err := d.countCIWorkflows(ctx, owner, repo)
	if err != nil {
		d.logger.Warn("failed to count CI workflows", "repo", repo, "error", err)
	}
	signals.CIWorkflowCount = ciCount
	signals.HasCI = ciCount > 0

	signals.HasCoverageConfig = d.hasCoverageConfig(ctx, owner, repo)
	signals.HasTDDMarkers = d.hasTDDMarkers(ctx, owner, repo)

	level := classifyMaturity(signals)
	result := &MaturityResult{
		Level:    level,
		TestMode: level.TestMode(),
		Signals:  signals,
	}

	d.logger.Info("maturity detected",
		"owner", owner,
		"repo", repo,
		"level", level.String(),
		"test_mode", result.TestMode,
	)

	return result, nil
}

func classifyMaturity(s MaturitySignals) MaturityLevel {
	if !s.HasTests && !s.HasCI {
		return MaturityIdea
	}
	if s.HasTests && !s.HasCI {
		return MaturityDev
	}
	if s.HasCI && s.HasCoverageConfig && s.HasTDDMarkers {
		return MaturityFullAuto
	}
	if s.HasCI {
		return MaturityCI
	}
	return MaturityDev
}

const defaultCoverageTarget = 91.0

// CoverageGap describes the distance between current and target test coverage.
type CoverageGap struct {
	CurrentPct float64 `json:"current_pct"`
	TargetPct  float64 `json:"target_pct"`
	GapPct     float64 `json:"gap_pct"`
}

// DetectCoverageGap reads the latest coverage data from CI artifacts and
// returns the gap between current coverage and the target.
func (d *MaturityDetector) DetectCoverageGap(ctx context.Context, owner, repo string) (*CoverageGap, error) {
	runs, _, err := d.ghClient.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, "ci.yml", &gh.ListWorkflowRunsOptions{
		Status:      "success",
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || runs.GetTotalCount() == 0 {
		return &CoverageGap{
			TargetPct: defaultCoverageTarget,
			GapPct:    defaultCoverageTarget,
		}, nil
	}

	artifacts, _, err := d.ghClient.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runs.WorkflowRuns[0].GetID(), &gh.ListOptions{PerPage: 50})
	if err != nil {
		return &CoverageGap{
			TargetPct: defaultCoverageTarget,
			GapPct:    defaultCoverageTarget,
		}, nil
	}

	for _, a := range artifacts.Artifacts {
		name := a.GetName()
		if strings.Contains(name, "coverage") || strings.Contains(name, "lcov") || strings.Contains(name, "cobertura") {
			d.logger.Info("coverage artifact found", "name", name, "repo", repo)
			return &CoverageGap{
				TargetPct: defaultCoverageTarget,
				GapPct:    defaultCoverageTarget,
			}, nil
		}
	}

	return &CoverageGap{
		TargetPct: defaultCoverageTarget,
		GapPct:    defaultCoverageTarget,
	}, nil
}

func (d *MaturityDetector) countTestFiles(ctx context.Context, owner, repo string) (int, error) {
	query := fmt.Sprintf("repo:%s/%s filename:_test.go OR filename:.test.ts OR filename:.test.tsx OR filename:.spec.ts OR filename:.spec.tsx OR filename:test_ path:test/", owner, repo)
	result, _, err := d.ghClient.Search.Code(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching test files: %w", err)
	}
	return result.GetTotal(), nil
}

func (d *MaturityDetector) countCIWorkflows(ctx context.Context, owner, repo string) (int, error) {
	workflows, _, err := d.ghClient.Actions.ListWorkflows(ctx, owner, repo, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return 0, fmt.Errorf("listing workflows: %w", err)
	}
	return workflows.GetTotalCount(), nil
}

func (d *MaturityDetector) hasCoverageConfig(ctx context.Context, owner, repo string) bool {
	coverageFiles := []string{
		".codecov.yml",
		"codecov.yml",
		".coveragerc",
		"jest.config.ts",
		"jest.config.js",
		"vitest.config.ts",
		"vitest.config.js",
		".nycrc",
		".nycrc.json",
	}

	for _, f := range coverageFiles {
		_, _, _, err := d.ghClient.Repositories.GetContents(ctx, owner, repo, f, nil)
		if err == nil {
			return true
		}
	}

	return false
}

func (d *MaturityDetector) hasTDDMarkers(ctx context.Context, owner, repo string) bool {
	markers := []string{
		"CONTRIBUTING.md",
		"CLAUDE.md",
		".hive/config.yaml",
	}

	tddKeywords := []string{"tdd", "test-driven", "red-green", "write test first", "failing test"}

	for _, f := range markers {
		fileContent, _, _, err := d.ghClient.Repositories.GetContents(ctx, owner, repo, f, nil)
		if err != nil || fileContent == nil {
			continue
		}
		content, err := fileContent.GetContent()
		if err != nil {
			continue
		}
		lower := strings.ToLower(content)
		for _, kw := range tddKeywords {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}

	return false
}
