package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

// metricsEnabled reports whether the /metrics Prometheus endpoint is turned on.
// Off by default (the endpoint exposes cost data unauthenticated); an operator
// opts in with HIVE_METRICS_ENABLED=1|true|yes so a scraper on the pod network
// can collect it.
func metricsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HIVE_METRICS_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// metricsToken returns the bearer token that guards /metrics. When set
// (HIVE_METRICS_TOKEN), the endpoint requires "Authorization: Bearer <token>";
// point Prometheus at it via bearer_token. The token is REQUIRED whenever
// metrics are enabled: with HIVE_METRICS_ENABLED set but no token configured,
// handleMetrics fails closed (403) instead of serving the cost/agent series to
// anyone on the pod network (#3785).
func metricsToken() string {
	return strings.TrimSpace(os.Getenv("HIVE_METRICS_TOKEN"))
}

// Prometheus text-exposition endpoint for hive's ESTIMATED LLM cost.
//
// This is deliberately dependency-free (no prometheus/client_golang): it writes
// the stable text exposition format directly, reusing estimatedCost() so the
// numbers are identical to the dashboard's Cost card and GET /api/cost.
//
// The whole surface is an ESTIMATE (token counts × list price) — the metric
// names carry `estimated` and the HELP text says so, so a downstream FinOps
// tool (e.g. OpenCost's FOCUS custom-cost plugin) never mistakes it for billed
// spend. Values are all-time cumulative counters; scrapers derive rates via
// increase()/rate() the usual way.
//
// Exposed series (labels: hive_id, plus model or agent):
//
//	hive_estimated_cost_usd{hive_id,model}          — per-model cumulative $
//	hive_estimated_cost_usd_by_agent{hive_id,agent} — per-agent cumulative $
//	hive_estimated_cost_usd_total{hive_id}          — grand total $
//	hive_model_input_tokens_total{hive_id,model}    — per-model input tokens
//	hive_model_output_tokens_total{hive_id,model}   — per-model output tokens
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Mandatory bearer auth (#3399, hardened in #3785): the cost/agent series
	// are business-sensitive, so /metrics FAILS CLOSED when metrics are enabled
	// without a token — enabling HIVE_METRICS_ENABLED alone no longer exposes
	// the data unauthenticated. Operators must also set HIVE_METRICS_TOKEN and
	// configure the scraper's bearer_token to match.
	want := metricsToken()
	if want == "" {
		http.Error(w, "metrics are enabled (HIVE_METRICS_ENABLED) but no HIVE_METRICS_TOKEN is configured; refusing to serve cost/agent metrics unauthenticated", http.StatusForbidden)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !secureCompare(got, want) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hive-metrics"`)
		http.Error(w, "metrics require a bearer token", http.StatusUnauthorized)
		return
	}

	est := s.estimatedCost()

	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}

	var b strings.Builder

	writeHeader := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	// Grand total.
	writeHeader("hive_estimated_cost_usd_total",
		"All-time cumulative ESTIMATED cost in USD (token counts x list price; not a bill).", "counter")
	fmt.Fprintf(&b, "hive_estimated_cost_usd_total{hive_id=%q} %g\n", hiveID, est.TotalUSD)

	// Per-model cost.
	writeHeader("hive_estimated_cost_usd",
		"All-time cumulative ESTIMATED cost in USD per model (token counts x list price; not a bill).", "counter")
	for _, m := range sortedByName(est.ByModel) {
		fmt.Fprintf(&b, "hive_estimated_cost_usd{hive_id=%q,model=%q,source=%q} %g\n",
			hiveID, m.Name, m.Source, m.USD)
	}

	// Per-agent cost.
	writeHeader("hive_estimated_cost_usd_by_agent",
		"All-time cumulative ESTIMATED cost in USD per agent (token counts x list price; not a bill).", "counter")
	for _, a := range sortedByName(est.ByAgent) {
		fmt.Fprintf(&b, "hive_estimated_cost_usd_by_agent{hive_id=%q,agent=%q} %g\n",
			hiveID, a.Name, a.USD)
	}

	// Per-model token counters (the price-independent basis of the estimate).
	writeHeader("hive_model_input_tokens_total",
		"All-time cumulative input tokens per model.", "counter")
	for _, m := range sortedByName(est.ByModel) {
		fmt.Fprintf(&b, "hive_model_input_tokens_total{hive_id=%q,model=%q} %d\n", hiveID, m.Name, m.Input)
	}
	writeHeader("hive_model_output_tokens_total",
		"All-time cumulative output tokens per model.", "counter")
	for _, m := range sortedByName(est.ByModel) {
		fmt.Fprintf(&b, "hive_model_output_tokens_total{hive_id=%q,model=%q} %d\n", hiveID, m.Name, m.Output)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// sortedByName returns the entries sorted by Name so the exposition output is
// stable across scrapes (Prometheus doesn't require order, but stable output
// makes diffs and tests deterministic).
func sortedByName(in []costModelEntry) []costModelEntry {
	out := make([]costModelEntry, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
