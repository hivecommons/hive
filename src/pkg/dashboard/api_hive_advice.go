package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
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
		Now:      now,
		Signals:  buildHiveAdvisorSignals(status),
		Previous: previous,
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

func buildHiveAdvisorSignals(status *StatusPayload) hiveadvisor.Signals {
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
	}
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
