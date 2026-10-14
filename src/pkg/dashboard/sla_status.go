package dashboard

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

// Delivery SLA (hivecommons/hive#11841 follow-on). Everything here is derived
// at /api/status serve time from the lifecycle timeline the hive already
// records — issue created_at retained on enumeration, classified, the
// hive/in-progress `started` stage, pr_opened on the issues a PR fixes, and
// merged from the audit trail / observed-merge hook. No GitHub call is made:
// the charged-call accounting in /api/gh-rate-limits is unaffected.

// FrontendSLA is the fleet-wide block on StatusPayload.
type FrontendSLA struct {
	timeline.SLARepoReport
	// Targets are the effective per-span budgets (hours) the UI colours against.
	Targets config.SLATargets `json:"targets"`
	// StaleCount is len(StaleInProgress), for the badge.
	StaleCount int `json:"stale_count"`
	// ClaimTTLS is the agent claim TTL (seconds) that bounds "stale".
	ClaimTTLS int `json:"claim_ttl_s"`
}

var (
	slaTrackerOnce sync.Once
	slaTracker     *timeline.SLATracker
)

// SLATracker returns the process-wide tracker, constructing it on first use.
func (s *Server) SLATracker() *timeline.SLATracker {
	slaTrackerOnce.Do(func() { slaTracker = timeline.NewSLATracker() })
	return slaTracker
}

// EnableSLAHistoryPersistence loads the persisted 30-day daily buckets from
// path and turns on bounded re-persistence, so the Overview sparklines keep
// their history across a pod restart even after old journeys are evicted.
func (s *Server) EnableSLAHistoryPersistence(path string) {
	if err := s.SLATracker().History.EnablePersistence(path, s.logger); err != nil && s.logger != nil {
		s.logger.Warn("sla history persistence unavailable — sparklines reset on restart",
			"path", path, "error", err)
	}
}

// slaClaimTTL is the agent claim TTL that bounds a healthy in-progress mark.
func slaClaimTTL(cfg *config.Config) time.Duration {
	claims := config.ClaimsConfig{}
	if cfg != nil {
		claims = cfg.Governor.Claims
	}
	_, agent, _, _ := claims.EffectiveTTLs()
	return agent
}

// attachSLA fills StatusPayload.SLA and each repo's SLA from the lifecycle
// timeline. Pure in-memory derivation; cheap enough to run per request.
func (s *Server) attachSLA(out *StatusPayload, now time.Time) {
	if s == nil || out == nil {
		return
	}
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	repos := make([]string, 0, len(out.Repos))
	for _, r := range out.Repos {
		repos = append(repos, overviewRepoName(r))
	}
	ttl := slaClaimTTL(cfg)
	report := s.SLATracker().Report(s.LifecycleTimeline().Journeys(0), repos, ttl, now)
	var targets config.SLATargets
	if cfg != nil {
		targets = cfg.SLA.EffectiveTargets()
	} else {
		targets = (*config.SLAConfig)(nil).EffectiveTargets()
	}
	out.SLA = &FrontendSLA{
		SLARepoReport: report.Fleet,
		Targets:       targets,
		StaleCount:    len(report.Fleet.StaleInProgress),
		ClaimTTLS:     int(ttl / time.Second),
	}
	for i := range out.Repos {
		short := timeline.ShortRepo(overviewRepoName(out.Repos[i]))
		if rep, ok := report.Repos[short]; ok {
			rep := rep
			out.Repos[i].SLA = &rep
		}
	}
}

// handleSLAConfigGet returns the configured and effective SLA targets.
func (s *Server) handleSLAConfigGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, slaSectionResponse(s.deps.Config))
}

// handleSLAConfigPut updates sla.targets. Invalid budgets are rejected with
// 400 before anything is mutated or persisted; 0 restores a default.
func (s *Server) handleSLAConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Targets config.SLATargets `json:"targets"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := config.ValidateSLATargets(body.Targets); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	cfg.SLA.Targets = body.Targets
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after sla targets update", "error", err)
	}
	eff := cfg.SLA.EffectiveTargets()
	s.auditFromRequest(r, "config_sla", auditDetail("section", "sla",
		"start_to_pr_h", fmt.Sprint(eff.StartToPRH), "pr_to_merge_h", fmt.Sprint(eff.PRToMergeH),
		"open_to_merge_h", fmt.Sprint(eff.OpenToMergeH)), "")
	jsonResponse(w, slaSectionResponse(cfg))
}

// slaSectionResponse renders the sla block: what the file says and what applies.
func slaSectionResponse(cfg *config.Config) map[string]interface{} {
	return map[string]interface{}{
		"targets":           cfg.SLA.Targets,
		"effective_targets": cfg.SLA.EffectiveTargets(),
		"defaults": config.SLATargets{
			StartToPRH: config.DefaultSLAStartToPRHours, PRToMergeH: config.DefaultSLAPRToMergeHours, OpenToMergeH: config.DefaultSLAOpenToMergeHours,
		},
	}
}
