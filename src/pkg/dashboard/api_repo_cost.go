package dashboard

// The /api/repo-cost handler. The interval join and its caching collector
// moved to pkg/dashboard/collect (kubestellar/hive#5565 slice 2); the HTTP
// surface stays here with the rest of the dashboard's handlers.

import (
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/tokens"
)

// handleRepoCost serves GET /api/repo-cost from the cached RepoCostCollector
// snapshot (see collect/repo_cost_collector.go) rather than recomputing the
// interval join per request. The join reads every rotated/compressed audit
// backup (up to 64MB decompressed each) and walks every retained Claude usage
// event in the window — recomputing that on every 60s dashboard poll, per
// open tab, was the defect fixed by #4943.
//
// When the collector has not produced a snapshot yet (fresh boot, before its
// first ticker fire), this reports ready=false with an EMPTY by_repo/zero
// totals and no CollectedAt — never a fabricated $0.00. A cost payload with
// ready=true and an empty by_repo is indistinguishable from "this hive spent
// nothing", which is exactly the misreporting the #4836 epic exists to
// prevent, so the not-ready state must stay visibly different from that.
func (s *Server) handleRepoCost(w http.ResponseWriter, r *http.Request) {
	if s.deps != nil && s.deps.RepoCost != nil {
		snap, ready := s.deps.RepoCost.Snapshot()
		if !ready {
			jsonResponse(w, collect.RepoCostResponse{
				Phase:              "phase_3_interval_join",
				ByRepo:             []collect.RepoCostEntry{},
				PriceTableDate:     tokens.PriceTableDate(),
				Disclaimer:         costEstimateDisclaimer,
				Limitations:        collect.RepoCostLimitations(),
				Unattributed:       collect.RepoCostEntry{Repo: collect.RepoCostBucketUnattributed, Source: "estimated"},
				BackendUnsupported: collect.RepoCostEntry{Repo: collect.RepoCostBucketBackendUnsupported, Source: "estimated"},
			})
			return
		}
		jsonResponse(w, snap)
		return
	}

	// No collector wired (e.g. a minimal test Server): fall back to computing
	// inline so the endpoint still degrades to a correct answer rather than
	// 503ing. Production always wires RepoCost (see cmd/hive/main.go).
	now := time.Now()
	var summary *tokens.AggregateSummary
	if s.deps != nil && s.deps.Tokens != nil {
		summary = s.deps.Tokens.Summary()
	}
	var entries []AuditEntry
	if s.audit != nil {
		entries = s.audit.OutputActionsSince(now.Add(-collect.RepoCostWindow), collect.ActivityOutputActions, s.auditPathForActivity())
	}
	jsonResponse(w, collect.ComputeRepoCost(summary, entries, now))
}

// auditPathForActivity returns the audit file path the activity collector was
// configured with, so the cost join reads exactly the same log. "" means the
// production default (see OutputActionsSince).
func (s *Server) auditPathForActivity() string {
	if s.deps != nil && s.deps.Activity != nil {
		return s.deps.Activity.AuditPath()
	}
	return ""
}
