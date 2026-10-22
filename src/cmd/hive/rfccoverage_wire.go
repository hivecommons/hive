package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rfccoverage"
)

// rfcCoverageState is the process-lifetime half of the triage
// existing-coverage check (issues.rfc_existing_check.*): the sweeper's memory
// of checked issues and the Settings search index, parsed once.
type rfcCoverageState struct {
	sweeper  *rfccoverage.Sweeper
	once     sync.Once
	settings []rfccoverage.Setting
	// page reads the dashboard page carrying the index; tests replace it.
	page func() ([]byte, error)
}

func newRFCCoverageState() *rfcCoverageState {
	return &rfcCoverageState{sweeper: rfccoverage.NewSweeper(), page: dashboard.SettingsSearchPage}
}

// rfcCoverageState lazily builds the boot's check state.
func (b *boot) rfcCoverageState() *rfcCoverageState {
	if b.rfcCoverage == nil {
		b.rfcCoverage = newRFCCoverageState()
	}
	return b.rfcCoverage
}

func (s *rfcCoverageState) index(logger *slog.Logger) []rfccoverage.Setting {
	s.once.Do(func() {
		page, err := s.page()
		if err != nil {
			logger.Warn("rfc existing-coverage: settings index unreadable", "error", err)
			return
		}
		s.settings = rfccoverage.ParseSettingsIndex(page)
	})
	return s.settings
}

// rfcCoverageForge adapts the GitHub client to rfccoverage.Forge. The marker
// lookup passes an empty body with create=false, so it never edits.
type rfcCoverageForge struct{ c *github.Client }

// newRFCCoverageForge returns nil without a client, which turns the pass off.
func newRFCCoverageForge(c *github.Client) rfccoverage.Forge {
	if c == nil {
		return nil
	}
	return rfcCoverageForge{c: c}
}

func (f rfcCoverageForge) FindMarkerComment(ctx context.Context, repo string, number int, marker string) (bool, error) {
	res, err := f.c.UpsertIssueMarkerComment(ctx, repo, number, marker, "", false)
	return res.Found, err
}

func (f rfcCoverageForge) CreateComment(ctx context.Context, repo string, number int, body string) error {
	return f.c.CreateIssueComment(ctx, repo, number, body)
}

// rfcCoverageCandidates projects the last enumeration's open issues.
func rfcCoverageCandidates(org string, result *github.ActionableResult) []rfccoverage.Candidate {
	if result == nil {
		return nil
	}
	var out []rfccoverage.Candidate
	for _, is := range result.Issues.Items {
		if is.Number <= 0 || (is.State != "" && !strings.EqualFold(is.State, "open")) {
			continue
		}
		out = append(out, rfccoverage.Candidate{
			Repo: qualifyClaimRepo(org, is.Repo), Number: is.Number, Title: is.Title, Body: is.Body,
			Labels: is.Labels, CreatedAt: is.CreatedAt,
		})
	}
	return out
}

// runRFCExistingCheck is the per-cycle triage pass
// (issues.rfc_existing_check.enabled, default off). It reads the last
// enumeration rather than re-listing issues, and costs GitHub calls only for
// an RFC it has not checked yet: one comment scan and at most one comment.
// It never labels or closes.
func runRFCExistingCheck(ctx context.Context, forge rfccoverage.Forge, cfg *config.Config, dashSrv *dashboard.Server, state *rfcCoverageState, last *github.ActionableResult, logger *slog.Logger) rfccoverage.Result {
	if forge == nil || cfg == nil || state == nil || last == nil || !cfg.Issues.RFCExistingCheck.Enabled {
		return rfccoverage.Result{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	candidates := rfcCoverageCandidates(cfg.Project.Org, last)
	if len(candidates) == 0 {
		return rfccoverage.Result{}
	}
	rc := cfg.Issues.RFCExistingCheck
	res := state.sweeper.Sweep(ctx, forge, candidates, rfccoverage.Options{
		Settings:             state.index(logger),
		Origin:               gfiPublicOrigin(cfg),
		Labels:               rc.EffectiveLabels(),
		MaxMatches:           rc.EffectiveMaxMatches(),
		MaxAge:               rc.EffectiveMaxAge(),
		PostWhenNothingFound: rc.PostWhenNothingFound,
		Logger:               logger,
	})
	if dashSrv != nil {
		for _, key := range res.Commented {
			repo, num := rfcCoverageIssueKey(key)
			dashSrv.AuditLogRecord("system", "rfc-existing-coverage", fmt.Sprintf("issue=%s", key), "", repo, num)
		}
	}
	if len(res.Commented) > 0 || res.Errors > 0 {
		logger.Info("rfc existing-coverage sweep", "scanned", res.Scanned, "commented", res.Commented, "errors", res.Errors, "skipped", fmt.Sprint(res.Skipped))
	}
	return res
}

// rfcCoverageIssueKey splits an owner/repo#n key for the audit log.
func rfcCoverageIssueKey(key string) (string, int) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return key, 0
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return key, 0
	}
	return key[:i], n
}
