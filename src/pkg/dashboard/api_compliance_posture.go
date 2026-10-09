package dashboard

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// compliancePostureHistoryPath persists the posture-check history on the
// /data PVC so it survives restarts and upgrades (hivecommons/hive#11079).
// A var so tests point it at a temp dir.
var compliancePostureHistoryPath = "/data/compliance-posture.jsonl"

// compliancePostureRunTimeout bounds a manual POST /api/compliance/posture/run.
const compliancePostureRunTimeout = 2 * time.Minute

// compliancePostureHistoryLimit caps the runs one GET returns.
const compliancePostureHistoryLimit = 1000

// compliancePostureConfigFiles lists the files the no-secrets check scans:
// the seed hive.yaml, the dashboard overlay, the runtime config and the
// per-agent overlays. A var so tests can keep the scan off the host's /data.
var compliancePostureConfigFiles = func(cfg *config.Config) []string {
	files := []string{cfg.SourcePath, config.DashboardOverlayFile, config.RuntimeConfigFile}
	dir := strings.TrimSpace(cfg.Data.AgentsDir)
	if dir == "" {
		dir = config.DefaultAgentOverlayDir
	}
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		if m, err := filepath.Glob(filepath.Join(dir, pattern)); err == nil {
			files = append(files, m...)
		}
	}
	return files
}

// postureGitHub adapts the hive's GitHub client to the read-only
// evidence interface the posture checks consume.
type postureGitHub struct{ c *ghpkg.Client }

func (p postureGitHub) MergedPRsSince(ctx context.Context, repo string, since time.Time) ([]compliance.PostureMergedPR, error) {
	prs, err := p.c.PostureMergedPRsSince(ctx, repo, since)
	if err != nil {
		return nil, err
	}
	out := make([]compliance.PostureMergedPR, 0, len(prs))
	for _, pr := range prs {
		out = append(out, compliance.PostureMergedPR{
			Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Author: pr.Author,
			MergedBy: pr.MergedBy, MergedByBot: pr.MergedByBot, MergedAt: pr.MergedAt,
			Reviewers: pr.Reviewers,
		})
	}
	return out, nil
}

func (p postureGitHub) RepoLabels(ctx context.Context, repo string) ([]string, error) {
	return p.c.RepoLabelNames(ctx, repo)
}

// postureDeps snapshots what a posture pass reads from the live server.
func (s *Server) postureDeps() compliance.PostureDeps {
	var cfg *config.Config
	var gh *ghpkg.Client
	if s.deps != nil {
		cfg, gh = s.deps.Config, s.deps.GHClient
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	d := compliance.PostureDeps{
		Config:      cfg,
		Getenv:      complianceGetenv,
		Repos:       append([]string(nil), cfg.Project.Repos...),
		HoldLabel:   ghpkg.CanonicalHiveHoldLabel(cfg.HiveID),
		ConfigFiles: compliancePostureConfigFiles(cfg),
	}
	if gh != nil {
		d.GitHub = postureGitHub{c: gh}
	}
	return d
}

// postureRunner lazily builds the runner and loads the persisted history.
func (s *Server) postureRunner() *compliance.PostureRunner {
	s.postureOnce.Do(func() {
		retention := config.ComplianceConfig{}.PostureHistoryRetentionOrDefault()
		if s.deps != nil && s.deps.Config != nil {
			retention = s.deps.Config.Compliance.PostureHistoryRetentionOrDefault()
		}
		hist, err := compliance.NewPostureHistory(compliancePostureHistoryPath, retention, 0)
		if err != nil && s.logger != nil {
			s.logger.Warn("compliance posture history unreadable; starting empty", "path", compliancePostureHistoryPath, "error", err)
		}
		r := compliance.NewPostureRunner(nil, hist, s.postureDeps)
		r.OnTransition = func(prev, cur compliance.Result) {
			s.AuditLog("system", "compliance_posture_failed", auditDetail(
				"check", cur.CheckID,
				"controls", strings.Join(cur.ControlIDs, " "),
				"since", prev.At.Format(time.RFC3339),
				"detail", cur.Detail,
			), "")
		}
		r.OnPersistError = func(err error) {
			if s.logger != nil {
				s.logger.Warn("compliance posture history write failed", "error", err)
			}
		}
		s.posture = r
	})
	return s.posture
}

// postureSchedule reports the live posture interval and whether any
// framework is selected — the same gate as /api/compliance/status.
func (s *Server) postureSchedule() (time.Duration, bool) {
	if s.deps == nil || s.deps.Config == nil {
		return config.DefaultCompliancePostureInterval, false
	}
	c := s.deps.Config.Compliance
	return c.PostureIntervalOrDefault(), c.IsEnabled()
}

// StartCompliancePosture runs the posture checks every
// compliance.posture_checks.interval while a framework is selected, until
// ctx is cancelled. Call once at startup.
func (s *Server) StartCompliancePosture(ctx context.Context) {
	go s.postureRunner().Loop(ctx, s.postureSchedule, nil)
}

// compliancePostureResponse is the GET /api/compliance/posture body.
type compliancePostureResponse struct {
	Disclaimer  string                        `json:"disclaimer"`
	Enabled     bool                          `json:"enabled"`
	Interval    string                        `json:"interval"`
	WindowDays  int                           `json:"window_days"`
	HistoryDays int                           `json:"history_days"`
	Checks      []compliance.PostureCheckInfo `json:"checks"`
	Latest      *compliance.PostureRun        `json:"latest"`
	History     []compliance.PostureRun       `json:"history,omitempty"`
	Truncated   bool                          `json:"truncated,omitempty"`
}

// parsePostureSince accepts an RFC 3339 timestamp, a YYYY-MM-DD date or a
// look-back duration ("168h").
func parsePostureSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, errors.New("since must be an RFC 3339 time, a YYYY-MM-DD date or a positive duration such as 168h")
}

// handleCompliancePosture serves GET /api/compliance/posture
// (hivecommons/hive#11079): the posture-check catalogue and the latest pass,
// plus, with ?since=, every pass since then (oldest first, at most
// compliancePostureHistoryLimit of the most recent). Owner only: results
// name owners, PRs and config file locations.
func (s *Server) handleCompliancePosture(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	cc := s.deps.Config.Compliance
	resp := compliancePostureResponse{
		Disclaimer:  compliance.Disclaimer,
		Enabled:     cc.IsEnabled(),
		Interval:    cc.PostureIntervalOrDefault().String(),
		WindowDays:  int(cc.PostureWindowOrDefault().Hours() / 24),
		HistoryDays: int(cc.PostureHistoryRetentionOrDefault().Hours() / 24),
		Checks:      compliance.PostureCatalogue(),
	}
	hist := s.postureRunner().History()
	if latest, ok := hist.Latest(); ok {
		resp.Latest = &latest
	}
	if v := r.URL.Query().Get("since"); v != "" {
		since, err := parsePostureSince(v, time.Now())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp.History, resp.Truncated = hist.Since(since, compliancePostureHistoryLimit)
		if resp.History == nil {
			resp.History = []compliance.PostureRun{}
		}
	}
	jsonResponse(w, resp)
}

// handleCompliancePostureRun serves POST /api/compliance/posture/run: one
// posture pass now, recorded in the history like a scheduled pass. Owner
// only. 409 when no framework is selected or a pass is already running.
func (s *Server) handleCompliancePostureRun(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	if !s.deps.Config.Compliance.IsEnabled() {
		jsonError(w, "no compliance frameworks selected (compliance.frameworks)", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), compliancePostureRunTimeout)
	defer cancel()
	run, err := s.postureRunner().Run(ctx, compliance.TriggerManual)
	if errors.Is(err, compliance.ErrPostureRunInProgress) {
		jsonError(w, "a posture run is already in progress", http.StatusConflict)
		return
	}
	s.auditFromRequest(r, "compliance_posture_run", auditDetail(
		"pass", strconv.Itoa(run.Summary.Pass),
		"fail", strconv.Itoa(run.Summary.Fail),
		"skip", strconv.Itoa(run.Summary.Skip),
		"error", strconv.Itoa(run.Summary.Error),
	), "")
	jsonResponse(w, run)
}
