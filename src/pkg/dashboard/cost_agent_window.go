package dashboard

import (
	"math"
	"net/http"
	"sort"
	"time"
)

// Per-agent tokens and estimated $ over a chosen window (#18968): the Cost
// card's "usage by sandbox" table follows the timeframe selector with these
// rows, the agent cards' spend chips read the same numbers, and the admin MCP
// spend_read tool reads them with hours or since/until.
//
// The rows come from the 5-minute cost snapshots (CostHistoryEntry), so a
// window reaches back only as far as the snapshot ring (~30 days). Each
// agent's dollars are the positive step-to-step increases of its cumulative
// counter between snapshots inside the window, as agentWindowSpend has always
// summed them in the browser, minus the restore-shaped jumps the daily rollup
// discards (costCorrectionGuard). Tokens follow the same steps.

// CostAgentSnap is one agent's cumulative token counters at a history
// snapshot, from the same estimatedCost().ByAgent row as its dollars.
type CostAgentSnap struct {
	Input       int64 `json:"i"`
	Output      int64 `json:"o"`
	CacheRead   int64 `json:"cr,omitempty"`
	CacheCreate int64 `json:"cc,omitempty"`
}

// costAgentWindowDefault is the window GET /api/cost/agents answers when the
// caller names none: the last 24 hours.
const costAgentWindowDefault = 24 * time.Hour

// Coverage of a window by the snapshot ring.
const (
	costCoverageFull    = "full"
	costCoveragePartial = "partial"
	costCoverageNone    = "none"
)

// costAgentWindowNote is the response's standing caveat.
const costAgentWindowNote = "Estimated from token counts × list price, not billed spend. Each agent's figures are the increases of its cumulative counters between the 5-minute cost snapshots inside the window; the snapshot ring keeps about 30 days."

// costAgentWindowRow is one agent's spend over the window. The token fields
// are nil when no pair of snapshots in the window carries the agent's token
// counters (snapshots from before per-agent tokens were recorded): unknown,
// not zero.
type costAgentWindowRow struct {
	Name        string  `json:"name"`
	USD         float64 `json:"usd"`
	Input       *int64  `json:"input"`
	Output      *int64  `json:"output"`
	CacheRead   *int64  `json:"cache_read"`
	CacheCreate *int64  `json:"cache_create"`
	TotalTokens *int64  `json:"total_tokens"`
	FirstSample string  `json:"first_sample"`
	LastSample  string  `json:"last_sample"`
	Samples     int     `json:"samples"`
	Coverage    string  `json:"coverage"`
	tokens      *[4]int64
	firstMs     int64
	lastMs      int64
}

// costAgentWindowResponse is the GET /api/cost/agents payload.
type costAgentWindowResponse struct {
	// Since and Until bound the window asked for (RFC3339).
	Since string `json:"since"`
	Until string `json:"until"`
	// DataSince is the oldest snapshot in the ring when it is later than
	// Since: the window reaches back past what is recorded, and the figures
	// cover DataSince..Until only. Empty when the ring covers the window.
	DataSince string `json:"data_since,omitempty"`
	// Coverage is full, partial (the window starts before the ring) or none
	// (no snapshots at all).
	Coverage    string               `json:"coverage"`
	TotalUSD    float64              `json:"total_usd"`
	TotalTokens *int64               `json:"total_tokens"`
	Agents      []costAgentWindowRow `json:"agents"`
	Estimated   bool                 `json:"estimated"`
	Note        string               `json:"note"`
}

// costAgentWindow aggregates the snapshot ring (timestamp order) into one row
// per agent seen in [since, until] (unix millis, inclusive, as the browser's
// window is). Rows are sorted by USD, then total tokens, then name.
func costAgentWindow(hist []CostHistoryEntry, since, until int64) costAgentWindowResponse {
	resp := costAgentWindowResponse{
		Since:     time.UnixMilli(since).UTC().Format(time.RFC3339),
		Until:     time.UnixMilli(until).UTC().Format(time.RFC3339),
		Coverage:  costCoverageFull,
		Agents:    []costAgentWindowRow{},
		Estimated: true,
		Note:      costAgentWindowNote,
	}
	if len(hist) == 0 {
		resp.Coverage = costCoverageNone
		return resp
	}
	if oldest := hist[0].Timestamp; oldest > since {
		resp.Coverage = costCoveragePartial
		resp.DataSince = time.UnixMilli(oldest).UTC().Format(time.RFC3339)
	}

	// An agent whose usage is unpriced has token counters but no $ entry
	// (sampleCostHistory records Agents only for USD > 0): name it from
	// either map so its tokens still show (#18971 review).
	names := map[string]bool{}
	for _, e := range hist {
		for name := range e.Agents {
			names[name] = true
		}
		for name := range e.AgentTokens {
			names[name] = true
		}
	}
	var totalTokens int64
	haveTokens := false
	for name := range names {
		row, ok := costAgentWindowFor(hist, name, since, until)
		if !ok {
			continue
		}
		resp.TotalUSD += row.USD
		if row.TotalTokens != nil {
			totalTokens += *row.TotalTokens
			haveTokens = true
		}
		resp.Agents = append(resp.Agents, row)
	}
	if haveTokens {
		resp.TotalTokens = &totalTokens
	}
	sort.SliceStable(resp.Agents, func(i, j int) bool {
		a, b := resp.Agents[i], resp.Agents[j]
		if a.USD != b.USD {
			return a.USD > b.USD
		}
		if at, bt := costTokensOrMinus(a.TotalTokens), costTokensOrMinus(b.TotalTokens); at != bt {
			return at > bt
		}
		return a.Name < b.Name
	})
	return resp
}

func costTokensOrMinus(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// costAgentWindowFor is one agent's row; ok is false when none of the
// agent's snapshots falls inside the window. The correction thresholds are
// derived from the agent's whole series and its peak is tracked from the
// start of the ring, exactly as the daily fold does for the total, so a
// restore inside the window is judged against history before it.
func costAgentWindowFor(hist []CostHistoryEntry, name string, since, until int64) (costAgentWindowRow, bool) {
	type sample struct {
		pt     costSeriesPoint
		tokens *CostAgentSnap
	}
	var series []sample
	pts := make([]costSeriesPoint, 0, len(hist))
	// A snapshot with the agent's tokens but no $ entry recorded its spend
	// as $0 (unpriced); the counter is cumulative, so it carries the last $
	// seen rather than reading as a drop to zero and a later restore.
	lastUSD := 0.0
	for i := range hist {
		usd, priced := hist[i].Agents[name]
		snap, counted := hist[i].AgentTokens[name]
		if !priced && !counted {
			continue
		}
		if priced {
			lastUSD = usd
		}
		s := sample{pt: costSeriesPoint{T: hist[i].Timestamp, USD: lastUSD}}
		if counted {
			s.tokens = &snap
		}
		series = append(series, s)
		pts = append(pts, s.pt)
	}
	if len(series) == 0 {
		return costAgentWindowRow{}, false
	}
	guard := newCostCorrectionGuard(pts)
	row := costAgentWindowRow{Name: name, Coverage: costCoverageFull}
	if series[0].pt.T > since {
		row.Coverage = costCoveragePartial
	}
	peak := math.Inf(-1)
	inWindow := func(t int64) bool { return t >= since && t <= until }
	for i, cur := range series {
		if inWindow(cur.pt.T) {
			if row.Samples == 0 {
				row.firstMs = cur.pt.T
			}
			row.Samples++
			row.lastMs = cur.pt.T
		}
		if i > 0 && inWindow(cur.pt.T) && inWindow(series[i-1].pt.T) {
			prev := series[i-1]
			if !guard.isCorrection(prev.pt, cur.pt, peak) {
				if d := cur.pt.USD - prev.pt.USD; d > 0 {
					row.USD += d
				}
				if prev.tokens != nil && cur.tokens != nil {
					if row.tokens == nil {
						row.tokens = &[4]int64{}
					}
					for k, d := range [4]int64{
						cur.tokens.Input - prev.tokens.Input,
						cur.tokens.Output - prev.tokens.Output,
						cur.tokens.CacheRead - prev.tokens.CacheRead,
						cur.tokens.CacheCreate - prev.tokens.CacheCreate,
					} {
						if d > 0 {
							row.tokens[k] += d
						}
					}
				}
			}
		}
		if cur.pt.USD > peak {
			peak = cur.pt.USD
		}
	}
	if row.Samples == 0 {
		return costAgentWindowRow{}, false
	}
	row.FirstSample = time.UnixMilli(row.firstMs).UTC().Format(time.RFC3339)
	row.LastSample = time.UnixMilli(row.lastMs).UTC().Format(time.RFC3339)
	if row.tokens != nil {
		t := *row.tokens
		in, out, cr, cc, total := t[0], t[1], t[2], t[3], t[0]+t[1]+t[2]+t[3]
		row.Input, row.Output, row.CacheRead, row.CacheCreate, row.TotalTokens = &in, &out, &cr, &cc, &total
	}
	return row, true
}

// handleCostAgents serves GET /api/cost/agents?since=&until= (RFC3339) or
// ?hours=N (1..720): each agent's tokens and estimated $ over the window,
// highest spend first. The window parameters are the advisor endpoints'
// (advisorWindow), which the Cost section's timeframe selector already sends
// for advisor spend. No window means the last 24 hours. A window that starts
// before the snapshot ring says so (coverage partial, data_since) instead of
// reporting the missing span as zero. Same access as GET /api/cost.
func (s *Server) handleCostAgents(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	since, until, msg := advisorWindow(r, now)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	if until.IsZero() {
		until = now
	}
	if since.IsZero() {
		since = until.Add(-costAgentWindowDefault)
	}
	jsonResponse(w, costAgentWindow(s.CostHistory(), since.UnixMilli(), until.UnixMilli()))
}
