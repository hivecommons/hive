package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// agentSnap is one snapshot carrying per-agent $ and, when tok is set, the
// agents' token counters. The hive total is the agents' sum.
func agentSnap(t time.Time, usd map[string]float64, tok map[string]CostAgentSnap) CostHistoryEntry {
	e := CostHistoryEntry{Timestamp: t.UnixMilli(), Agents: usd, AgentTokens: tok}
	for _, v := range usd {
		e.USD += v
	}
	return e
}

func windowRow(t *testing.T, resp costAgentWindowResponse, name string) costAgentWindowRow {
	t.Helper()
	for _, r := range resp.Agents {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", name, resp.Agents)
	return costAgentWindowRow{}
}

// browserWindowSpend is agentWindowSpend (static-src/js/cost-trend.js) in Go:
// the positive step-to-step increases between in-window snapshots.
func browserWindowSpend(hist []CostHistoryEntry, name string, since, until int64) float64 {
	var spend float64
	var prev *float64
	for _, e := range hist {
		cur, ok := e.Agents[name]
		if !ok || e.Timestamp < since || e.Timestamp > until {
			continue
		}
		if prev != nil && cur > *prev {
			spend += cur - *prev
		}
		prev = &cur
	}
	return spend
}

// (a) Steady growth: the server's per-agent $ matches the browser's sum for
// the same window, tokens follow the same steps, and rows are highest spend
// first.
func TestCostAgentWindowSteadyGrowth(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	var hist []CostHistoryEntry
	for h := 0; h < 48; h++ {
		hist = append(hist, agentSnap(t0.Add(time.Duration(h)*time.Hour),
			map[string]float64{"scanner": float64(h), "reviewer": float64(3 * h)},
			map[string]CostAgentSnap{
				"scanner":  {Input: int64(h * 100), Output: int64(h * 10), CacheRead: int64(h * 1000)},
				"reviewer": {Input: int64(h * 300), Output: int64(h * 30)},
			}))
	}
	since, until := t0.Add(24*time.Hour).UnixMilli(), t0.Add(47*time.Hour).UnixMilli()
	resp := costAgentWindow(hist, since, until)
	if resp.Coverage != costCoverageFull || resp.DataSince != "" {
		t.Fatalf("coverage = %q data_since %q, want full", resp.Coverage, resp.DataSince)
	}
	if len(resp.Agents) != 2 || resp.Agents[0].Name != "reviewer" {
		t.Fatalf("rows = %+v, want reviewer (highest $) first", resp.Agents)
	}
	scanner := windowRow(t, resp, "scanner")
	if want := browserWindowSpend(hist, "scanner", since, until); scanner.USD != want || want != 23 {
		t.Errorf("scanner usd = %v, browser sum %v, want 23", scanner.USD, want)
	}
	if scanner.Input == nil || *scanner.Input != 2300 || *scanner.Output != 230 || *scanner.CacheRead != 23000 || *scanner.TotalTokens != 25530 {
		t.Errorf("scanner tokens = in %v out %v cr %v total %v", scanner.Input, scanner.Output, scanner.CacheRead, scanner.TotalTokens)
	}
	if scanner.Samples != 24 || scanner.Coverage != costCoverageFull {
		t.Errorf("scanner samples %d coverage %q", scanner.Samples, scanner.Coverage)
	}
	if resp.TotalUSD != 23+69 || resp.TotalTokens == nil || *resp.TotalTokens != 25530+7590 {
		t.Errorf("totals = $%v / %v tokens", resp.TotalUSD, resp.TotalTokens)
	}
}

// (b) A counter drop after a restart is not negative spend: growth on both
// sides of it counts, as in the browser.
func TestCostAgentWindowCounterDrop(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	usd := []float64{1, 2, 3, 0.5, 1.5}
	var hist []CostHistoryEntry
	for i, v := range usd {
		hist = append(hist, agentSnap(t0.Add(time.Duration(i)*time.Hour), map[string]float64{"scanner": v}, nil))
	}
	since, until := t0.UnixMilli(), t0.Add(5*time.Hour).UnixMilli()
	row := windowRow(t, costAgentWindow(hist, since, until), "scanner")
	if want := browserWindowSpend(hist, "scanner", since, until); row.USD != want || want != 3 {
		t.Errorf("usd = %v, browser sum %v, want 3 (1+1 before the drop, +1 after)", row.USD, want)
	}
}

// (c) A restore-shaped jump is dropped exactly as the daily rollup drops it
// from the hive total; a large climb to a new high is kept.
func TestCostAgentWindowDropsRestoreLikeDailyFold(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	// Ten ordinary $1 steps set the ceiling, a $5,000 climb to a new high is
	// a flush, the counter drops back, a $5,000 restore regains the ground,
	// then +$1.
	series := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 5009, 9, 5009, 5010}
	var hist []CostHistoryEntry
	for i, v := range series {
		hist = append(hist, agentSnap(day.Add(time.Duration(i)*time.Hour), map[string]float64{"scanner": v}, nil))
	}
	row := windowRow(t, costAgentWindow(hist, day.UnixMilli(), day.Add(23*time.Hour).UnixMilli()), "scanner")
	fold := costFoldDailyRollups(hist)["2026-09-03"]
	if row.USD != fold.USD || row.USD != 5010 {
		t.Errorf("window usd = %v, daily fold usd = %v, want both $5010 (9 + the $5000 flush + 1; the $5000 restore discarded)", row.USD, fold.USD)
	}
	// The same climb as the first step of the window is spend.
	hist = []CostHistoryEntry{
		agentSnap(day, map[string]float64{"scanner": 7}, nil),
		agentSnap(day.Add(time.Hour), map[string]float64{"scanner": 5007}, nil),
	}
	if row := windowRow(t, costAgentWindow(hist, day.UnixMilli(), day.Add(2*time.Hour).UnixMilli()), "scanner"); row.USD != 5000 {
		t.Errorf("usd = %v, want the $5000 climb to a new high kept", row.USD)
	}
}

// (d) A window that starts before the ring says so instead of reporting the
// missing span as zero.
func TestCostAgentWindowPartialCoverage(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	hist := []CostHistoryEntry{
		agentSnap(t0, map[string]float64{"scanner": 1}, nil),
		agentSnap(t0.Add(time.Hour), map[string]float64{"scanner": 2}, nil),
	}
	resp := costAgentWindow(hist, t0.Add(-30*24*time.Hour).UnixMilli(), t0.Add(2*time.Hour).UnixMilli())
	if resp.Coverage != costCoveragePartial || resp.DataSince != t0.Format(time.RFC3339) {
		t.Fatalf("coverage %q data_since %q, want partial from %s", resp.Coverage, resp.DataSince, t0.Format(time.RFC3339))
	}
	if row := windowRow(t, resp, "scanner"); row.Coverage != costCoveragePartial || row.USD != 1 {
		t.Errorf("row = %+v, want partial with $1", row)
	}
	if empty := costAgentWindow(nil, 0, 1); empty.Coverage != costCoverageNone || empty.Agents == nil || len(empty.Agents) != 0 {
		t.Errorf("no history = %+v, want coverage none and an empty agents list", empty)
	}
}

// (e) Snapshots written before per-agent tokens existed still count their
// dollars; the agent's tokens are unknown (null), not zero.
func TestCostAgentWindowOldSnapshotsHaveNoTokens(t *testing.T) {
	var old []CostHistoryEntry
	if err := json.Unmarshal([]byte(`[{"t":1763337600000,"usd":1,"agents":{"scanner":1}},{"t":1763341200000,"usd":3,"agents":{"scanner":3}}]`), &old); err != nil {
		t.Fatalf("old snapshots no longer decode: %v", err)
	}
	resp := costAgentWindow(old, old[0].Timestamp, old[1].Timestamp)
	row := windowRow(t, resp, "scanner")
	if row.USD != 2 || row.Input != nil || row.TotalTokens != nil || resp.TotalTokens != nil {
		t.Errorf("row = %+v total_tokens %v, want $2 with null tokens", row, resp.TotalTokens)
	}
	blob, _ := json.Marshal(row)
	if want := `"total_tokens":null`; !strings.Contains(string(blob), want) {
		t.Errorf("row JSON %s lacks %s", blob, want)
	}
}

// The sampler records each agent's token counters beside its dollars.
func TestSampleCostHistoryRecordsAgentTokens(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Tokens: tokenCollectorWithSummary(t, tokens.AggregateSummary{
		ByModelDetail: map[string]*tokens.AgentModelBucket{"claude-sonnet-4-5": {Input: 1_000_000, Output: 100_000}},
		ByAgentDetail: map[string]*tokens.AgentModelBucket{"scanner": {Input: 1_000_000, Output: 100_000}},
		ByAgent:       map[string]int64{"scanner": 1_100_000},
		ByModel:       map[string]int64{"claude-sonnet-4-5": 1_100_000},
		Sessions: []tokens.SessionSummary{{
			SessionID: "s1", Agent: "scanner", Model: "claude-sonnet-4-5",
			InputTokens: 1_000_000, OutputTokens: 100_000, TotalTokens: 1_100_000,
		}},
		SessionCount: 1,
	})}
	if !s.sampleCostHistory() {
		t.Fatal("no snapshot recorded")
	}
	hist := s.CostHistory()
	if got := hist[len(hist)-1].AgentTokens["scanner"]; got.Input != 1_000_000 || got.Output != 100_000 {
		t.Errorf("agent tokens = %+v, want 1M in / 100K out", got)
	}
}

// GET /api/cost/agents: the advisor endpoints' window parameters, refused
// with a reason when bad; no window is the last 24 hours.
func TestHandleCostAgents(t *testing.T) {
	s := newTestServer()
	now := time.Now().UTC()
	s.SeedCostHistory([]CostHistoryEntry{
		agentSnap(now.Add(-2*time.Hour), map[string]float64{"scanner": 1}, map[string]CostAgentSnap{"scanner": {Input: 10}}),
		agentSnap(now.Add(-time.Hour), map[string]float64{"scanner": 4}, map[string]CostAgentSnap{"scanner": {Input: 40}}),
	})
	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleCostAgents(rec, httptest.NewRequest(http.MethodGet, "/api/cost/agents"+q, nil))
		return rec
	}
	for _, bad := range []string{"?hours=0", "?hours=721", "?hours=x", "?since=yesterday", "?since=2026-11-18T00:00:00Z&hours=3", "?since=2026-11-18T00:00:00Z&until=2026-11-17T00:00:00Z"} {
		if rec := get(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
	rec := get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("default window = %d", rec.Code)
	}
	var resp costAgentWindowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	since, _ := time.Parse(time.RFC3339, resp.Since)
	until, _ := time.Parse(time.RFC3339, resp.Until)
	if d := until.Sub(since); d != costAgentWindowDefault || !resp.Estimated {
		t.Errorf("default window spans %v (estimated %v), want 24h", d, resp.Estimated)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].USD != 3 || resp.Agents[0].Input == nil || *resp.Agents[0].Input != 30 {
		t.Errorf("agents = %+v, want scanner $3 / 30 input", resp.Agents)
	}
	if rec := get("?hours=1"); rec.Code != http.StatusOK {
		t.Errorf("hours=1 = %d", rec.Code)
	}
}
