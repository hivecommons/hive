package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousengine"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// nousReportFile is the campaign report Nous writes beside meta_findings.json.
const nousReportFile = "report.md"

// nousVerdictStore adapts the experimentation engine's Verdict face to the
// dashboard's NousVerdictStore seam (RFC #11585 part 6, #11699). It reads
// the governor campaign — the one the hive's own experimenter runs — on
// every call so the card follows the file without a restart.
type nousVerdictStore struct {
	engine     nousengine.Engine
	campaign   nousengine.Campaign
	targetRepo string
}

func (v nousVerdictStore) LoadVerdict() (dashboard.NousVerdict, bool, error) {
	verdict, err := v.engine.Verdict(v.campaign)
	if errors.Is(err, nousengine.ErrNoVerdict) {
		return dashboard.NousVerdict{}, false, nil
	}
	if err != nil {
		return dashboard.NousVerdict{}, false, err
	}
	out := dashboard.NousVerdict{
		Campaign:                nousstore.GovernorSubdir,
		Recommendation:          verdict.Recommendation,
		Score:                   verdict.Score,
		Baseline:                verdict.Baseline,
		MeasuredAgainstBaseline: verdict.MeasuredAgainstBaseline,
		Citations:               verdict.Citations,
		Caveats:                 verdict.Caveats,
		Summary:                 verdict.Summary,
		TargetRepo:              v.targetRepo,
	}
	if v.campaign.ID != "" {
		out.Campaign = v.campaign.ID
	}
	report := filepath.Join(v.engine.Layout(v.campaign).Root, nousReportFile)
	if _, statErr := os.Stat(report); statErr == nil {
		out.Report = report
	}
	return out, true, nil
}

// nousPromotionStore adapts nousstore's append-only promotion record to the
// dashboard's NousPromotionStore seam.
type nousPromotionStore struct {
	layout nousstore.Layout
	now    func() time.Time
}

func (p nousPromotionStore) RecordPromotion(in dashboard.NousPromotion) error {
	return p.layout.RecordPromotion(nousstore.Promotion{
		Campaign:       in.Campaign,
		Target:         in.Target,
		Recommendation: in.Recommendation,
		PromotedBy:     in.PromotedBy,
		PromotedAt:     in.PromotedAt,
		Repo:           in.Repo,
		Number:         in.Number,
		URL:            in.URL,
		RunKey:         in.RunKey,
	}, p.now())
}

func (p nousPromotionStore) Promotions() ([]dashboard.NousPromotion, error) {
	all, err := p.layout.Promotions()
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.NousPromotion, len(all))
	for i, r := range all {
		out[i] = dashboard.NousPromotion{
			Campaign:       r.Campaign,
			Target:         r.Target,
			Recommendation: r.Recommendation,
			PromotedBy:     r.PromotedBy,
			PromotedAt:     r.PromotedAt,
			Repo:           r.Repo,
			Number:         r.Number,
			URL:            r.URL,
			RunKey:         r.RunKey,
		}
	}
	return out, nil
}

// nousDefaultTargetRepo is the repo a promotion lands in when the hive
// watches exactly one; otherwise the owner names it on the card.
func nousDefaultTargetRepo(cfg *config.Config) string {
	if cfg == nil || len(cfg.Project.Repos) != 1 {
		return ""
	}
	return config.QualifyRepo(cfg.Project.Org, cfg.Project.Repos[0])
}

// wireNousVerdict attaches the engine's verdict and the durable promotion
// record to state and performs the initial sync, so the boot snapshot and
// every later request come from one path. Promotion itself stays a manual,
// owner-only dashboard action (POST /api/nous/promote).
func wireNousVerdict(state *dashboard.NousState, cfg *config.Config, layout nousstore.Layout, getenv func(string) string, logger *slog.Logger) {
	if state == nil {
		return
	}
	dispatcher := ""
	if cfg != nil {
		dispatcher = cfg.Nous.ResolveDispatcher(getenv)
	}
	engine, err := nousengine.New("", nousengine.Settings{Root: layout.Root, Dispatcher: dispatcher})
	if err != nil {
		if logger != nil {
			logger.Warn("nous: experiment engine unavailable, verdict card disabled", "error", err)
		}
	} else {
		state.VerdictStore = nousVerdictStore{engine: engine, targetRepo: nousDefaultTargetRepo(cfg)}
	}
	state.PromotionStore = nousPromotionStore{layout: layout, now: time.Now}
	if err := state.SyncVerdict(); err != nil && logger != nil {
		logger.Warn("nous: verdict or promotions unreadable at boot", "error", err)
	}
}
