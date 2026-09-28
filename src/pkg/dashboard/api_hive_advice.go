package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

const hiveAdviceEpochPath = "/data/hive-advice-epoch.json"

// handleHiveAdvice serves the same frozen, weekly owner-advice epoch embedded
// in the dashboard status payload and advisory digest. The computation is pure;
// this handler only supplies already-collected status/config signals and
// persists the current epoch so the top-three list does not flicker across eval cycles or restarts.
func (s *Server) handleHiveAdvice(w http.ResponseWriter, r *http.Request) {
	var status *StatusPayload
	if s != nil {
		s.statusMu.RLock()
		status = s.status
		s.statusMu.RUnlock()
	}
	if status == nil {
		if last := s.CurrentHiveAdvice(); last != nil {
			jsonResponse(w, last)
			return
		}
		jsonResponse(w, hiveadvisor.Result{})
		return
	}
	if status.HiveAdvice != nil {
		out := cloneHiveAdvisorResult(status.HiveAdvice)
		jsonResponse(w, out)
		return
	}
	if last := s.CurrentHiveAdvice(); last != nil {
		jsonResponse(w, last)
		return
	}
	jsonResponse(w, hiveadvisor.Result{})
}

// AttachHiveAdvice computes or reuses the frozen hive-advice epoch for status
// and stores it on status. Callers building the advisory digest call this before
// rendering so the digest and dashboard card share one source of truth.
func (s *Server) AttachHiveAdvice(status *StatusPayload, now time.Time) *hiveadvisor.Result {
	if status == nil {
		status = &StatusPayload{}
	}
	if status.ACMMAdvice == nil {
		advice := acmmadvisor.RecommendFromStatus(s.buildACMMStatusInputsFromStatus(status))
		status.ACMMAdvice = &advice
	}

	var previous *hiveadvisor.Epoch
	if s != nil {
		s.ensureHiveAdviceEpochLoaded()
		s.hiveAdviceMu.RLock()
		if s.hiveAdviceEpoch != nil {
			prev := *s.hiveAdviceEpoch
			previous = &prev
		}
		s.hiveAdviceMu.RUnlock()
	}
	res := hiveadvisor.Recommend(hiveadvisor.Request{
		Now:        now,
		Signals:    s.buildHiveAdvisorSignals(status, now),
		Previous:   previous,
		Thresholds: s.hiveAdviceThresholds(),
	})
	if s != nil {
		s.hiveAdviceMu.Lock()
		epoch := res.Epoch
		s.hiveAdviceEpoch = &epoch
		last := res
		s.hiveAdviceLast = &last
		s.hiveAdviceMu.Unlock()
		s.persistHiveAdviceEpoch(epoch)
	}
	status.HiveAdvice = &res
	return &res
}

// CurrentHiveAdvice returns the last persisted advice epoch for read-only
// renderers such as the advisory digest. The returned value is a defensive copy.
func (s *Server) CurrentHiveAdvice() *hiveadvisor.Result {
	if s == nil {
		return nil
	}
	s.ensureHiveAdviceEpochLoaded()
	s.hiveAdviceMu.RLock()
	defer s.hiveAdviceMu.RUnlock()
	if s.hiveAdviceLast == nil {
		return nil
	}
	out := cloneHiveAdvisorResult(s.hiveAdviceLast)
	out.NextReviewInDays = hiveAdviceDaysUntil(time.Now().UTC(), out.Epoch.End)
	return out
}

// buildHiveAdvisorSignals collects the advisor's inputs. The governor
// counters (Governor.Issues/PRs) are the ACTIONABLE queue; Queue is the
// Overview chart's per-item breakdown, classified by the same bands.go
// classifier the Overview donuts and the /api/overview exports use (#9102).
func (s *Server) buildHiveAdvisorSignals(status *StatusPayload, now time.Time) hiveadvisor.Signals {
	if status == nil {
		return hiveadvisor.Signals{}
	}
	var acmm acmmadvisor.Recommendation
	if status.ACMMAdvice != nil {
		acmm = *status.ACMMAdvice
	}
	return hiveadvisor.Signals{
		Mode:                status.Governor.Mode,
		QueueIssues:         status.Governor.Issues,
		QueuePRs:            status.Governor.PRs,
		HoldCount:           status.Hold.Total,
		RepoCount:           len(status.Repos),
		DisabledAgentCount:  countDisabledConfiguredAgents(status.ConfiguredAgents),
		NoCadenceAgentCount: countNoCadenceAgents(status.Agents),
		BudgetUsedPct:       status.Budget.PctUsed,
		BudgetExhausted:     status.Budget.Exhausted,
		ACMM:                acmm,
		Queue:               s.buildHiveAdvisorQueue(status, now),
	}
}

func (s *Server) issueBandsConfig() config.DashboardIssueBandsConfig {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Dashboard.IssueBands
	}
	return config.DashboardIssueBandsConfig{}
}

func (s *Server) hiveAdviceThresholds() hiveadvisor.Thresholds {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return hiveadvisor.Thresholds{}
	}
	q := s.deps.Config.Governor.Advisory.QueueHealth
	return hiveadvisor.Thresholds{
		BlockedPRPct:       q.BlockedPRPct,
		NeedsHumanPRPct:    q.NeedsHumanPRPct,
		NeedsHumanIssuePct: q.NeedsHumanIssuePct,
		StaleBlockedPRs:    q.StaleBlockedPRs,
		LaneSharePct:       q.LaneSharePct,
		CheckSharePct:      q.CheckSharePct,
	}
}

// buildHiveAdvisorQueue walks every open and held issue/PR across repo cards
// through the Overview classifier and hands the advisor one PRItem/IssueItem
// per item plus the band rule text the Overview tooltips print.
func (s *Server) buildHiveAdvisorQueue(status *StatusPayload, now time.Time) hiveadvisor.Queue {
	cfg := s.issueBandsConfig()
	q := hiveadvisor.Queue{StaleDays: normalizeIssueBandsConfig(cfg).StaleDays}
	for _, spec := range PRBandSpecs(cfg) {
		q.PRBands = append(q.PRBands, hiveadvisor.BandRule{Kind: "pr", Key: spec.Key, Label: spec.Label, Rule: spec.Rule})
	}
	for _, spec := range IssueBandSpecs(cfg) {
		q.IssueBands = append(q.IssueBands, hiveadvisor.BandRule{Kind: "issue", Key: spec.Key, Label: spec.Label, Rule: spec.Rule})
	}
	for _, item := range s.overviewPRItems(status, cfg, overviewFilters{}, now) {
		pr := item.pr
		labels := labelSet(pr.Labels)
		q.PRs = append(q.PRs, hiveadvisor.PRItem{
			Repo:           pr.Repo,
			Number:         pr.Number,
			URL:            overviewItemURL(pr.URL, pr.Repo, pr.Number, "pr", status.GitHubBaseURL),
			Band:           item.info.Band,
			Held:           item.held || len(holdLabels(pr.Labels, status.HiveID)) > 0,
			NeedsHuman:     labels["needs-human"],
			NeedsDecision:  labels["needs-decision"] || labels["2-discussing"],
			CIFailing:      prCIFailing(pr),
			FailingChecks:  append([]string(nil), pr.FailingChecks...),
			Conflict:       pr.Mergeable == github.MergeableNo,
			VerdictBlocked: item.verdict != nil && item.verdict.State == github.MergeVerdictBlocked,
			VerdictReason:  verdictReason(item.verdict),
			Lane:           item.info.Role,
			Author:         pr.Author,
			Stale:          item.info.Stale,
			AgeDays:        daysSince(pr.CreatedAt, now),
			IdleDays:       daysSince(issueActivityPR(pr), now),
		})
	}
	for _, item := range overviewIssueItems(status, cfg, overviewFilters{}, now) {
		issue := item.issue
		labels := labelSet(issue.Labels)
		q.Issues = append(q.Issues, hiveadvisor.IssueItem{
			Repo:          issue.Repo,
			Number:        issue.Number,
			URL:           overviewItemURL(issue.URL, issue.Repo, issue.Number, "issue", status.GitHubBaseURL),
			Band:          item.info.Band,
			Blocked:       labels["blocked"],
			NeedsDecision: labels["needs-decision"],
			Discussing:    labels["2-discussing"],
			AgeDays:       daysSince(issue.CreatedAt, now),
			IdleDays:      daysSince(issueActivity(issue), now),
		})
	}
	return q
}

func verdictReason(verdict *github.MergeVerdict) string {
	if verdict == nil {
		return ""
	}
	return verdict.Reason
}

// daysSince is whole days from t to now; 0 for a zero or sentinel time.
func daysSince(t, now time.Time) int {
	if t.IsZero() || !t.Before(now) {
		return 0
	}
	return int(now.Sub(t) / (hoursPerDay * time.Hour))
}

func countDisabledConfiguredAgents(agents []FrontendConfiguredAgent) int {
	count := 0
	for _, a := range agents {
		if !a.Enabled {
			count++
		}
	}
	return count
}

func countNoCadenceAgents(agents []FrontendAgent) int {
	count := 0
	for _, a := range agents {
		if a.Enabled && (a.NoCadence || strings.TrimSpace(a.Cadence) == "") {
			count++
		}
	}
	return count
}

func cloneHiveAdvisorResult(in *hiveadvisor.Result) *hiveadvisor.Result {
	if in == nil {
		return nil
	}
	out := *in
	out.Recommendations = cloneHiveAdvisorRecommendations(in.Recommendations)
	out.Epoch.Recommendations = cloneHiveAdvisorRecommendations(in.Epoch.Recommendations)
	return &out
}

func cloneHiveAdvisorRecommendations(in []hiveadvisor.Recommendation) []hiveadvisor.Recommendation {
	out := make([]hiveadvisor.Recommendation, len(in))
	copy(out, in)
	for i := range out {
		out[i].Signals = append([]hiveadvisor.Signal(nil), out[i].Signals...)
		out[i].Links = append([]hiveadvisor.Link(nil), out[i].Links...)
		out[i].Items = append([]hiveadvisor.Item(nil), out[i].Items...)
		out[i].Bands = append([]string(nil), out[i].Bands...)
	}
	return out
}

func (s *Server) ensureHiveAdviceEpochLoaded() {
	if s == nil {
		return
	}
	s.hiveAdviceMu.Lock()
	defer s.hiveAdviceMu.Unlock()
	if s.hiveAdviceLoaded {
		return
	}
	s.hiveAdviceLoaded = true
	data, err := os.ReadFile(hiveAdviceEpochPath)
	if err != nil {
		return
	}
	var epoch hiveadvisor.Epoch
	if err := json.Unmarshal(data, &epoch); err != nil || epoch.Start.IsZero() {
		return
	}
	s.hiveAdviceEpoch = &epoch
	s.hiveAdviceLast = &hiveadvisor.Result{
		Epoch:            epoch,
		Recommendations:  cloneHiveAdvisorRecommendations(epoch.Recommendations),
		NextReviewInDays: hiveAdviceDaysUntil(time.Now().UTC(), epoch.End),
		Frozen:           true,
	}
}

func (s *Server) persistHiveAdviceEpoch(epoch hiveadvisor.Epoch) {
	if s == nil || epoch.Start.IsZero() {
		return
	}
	data, err := json.MarshalIndent(epoch, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(hiveAdviceEpochPath, data, 0o600)
}

func hiveAdviceDaysUntil(now, end time.Time) int {
	if !now.Before(end) {
		return 0
	}
	return int((end.Sub(now) + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
}
