package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/tokens"
)

// Live per-run line (hivecommons/hive#14576).
//
// The token collector already keeps a per-agent cumulative token/cost figure
// (the same one GET /api/cost reports under estimated.by_agent). A running
// kick's own spend is the difference between that cumulative figure now and
// the figure when the kick started, so the run meter snapshots the baseline
// the first time it sees an agent busy for a given kick time and reports the
// delta on every status build after that. Nothing new is metered.

// FrontendAgentRun is the live per-run figure on a busy agent card and in the
// transcript pane header. It is present only while the agent is working.
type FrontendAgentRun struct {
	// Since is when the kick was delivered (the agent's LastKick), RFC3339;
	// BusySince when no kick time is known.
	Since string `json:"since"`
	// ElapsedS is seconds since Since at build time.
	ElapsedS int64 `json:"elapsedS"`
	// CostUSD and Tokens are this run's estimated spend and token count
	// (input + output + cache) since the kick started. Priced is false when
	// the collector has no per-agent figure for this agent yet, in which
	// case both are zero and the UI renders them as unknown.
	CostUSD float64 `json:"costUSD"`
	Tokens  int64   `json:"tokens"`
	Priced  bool    `json:"priced"`
	// Tests is the last test-runner outcome visible in the pane: "pass",
	// "fail" or "" when no result line is visible.
	Tests string `json:"tests,omitempty"`
	// FilesChanged is the last `N files changed` git summary visible in the
	// pane; -1 when none is visible.
	FilesChanged int `json:"filesChanged"`
}

// FrontendFleetRun is the governor-strip fleet figure: how many agents are
// mid-kick and the estimated spend accrued in the current clock hour, plus
// the wider windows the tile rotates through (hivecommons/hive#15614).
type FrontendFleetRun struct {
	Running int     `json:"running"`
	HourUSD float64 `json:"hourUSD"`
	// HourPriced is false until the meter has observed a cumulative total
	// in the current hour, i.e. when HourUSD would be a guess.
	HourPriced bool `json:"hourPriced"`
	// DayUSD / WeekUSD / MonthUSD are the estimated $ spent since the start
	// of the current UTC calendar day, ISO week (Monday 00:00 UTC) and
	// calendar month (1st 00:00 UTC): the daily cost ledger summed over the
	// window plus the live rise since its last snapshot (fleetCostWindows).
	// They are attached by the Server, not the run meter, and are only
	// meaningful when WindowsPriced is true.
	DayUSD   float64 `json:"dayUSD"`
	WeekUSD  float64 `json:"weekUSD"`
	MonthUSD float64 `json:"monthUSD"`
	// WindowsPriced is false when there is no cost ledger (no snapshot and
	// no daily rollup) to sum, i.e. when the wider windows would be a guess.
	WindowsPriced bool `json:"windowsPriced"`
}

type runBaseline struct {
	since  time.Time
	usd    float64
	tokens int64
}

// runMeter remembers per-agent kick baselines and the hourly fleet baseline.
// It is a package-level singleton, like the other status-build providers in
// this file's neighbours, because the status builders are free functions.
type runMeter struct {
	mu        sync.Mutex
	baselines map[string]runBaseline
	// last is the most recent per-agent cumulative figure seen, so builds
	// without a collector (BuildAgentOnlyStatus) still report a delta.
	last      map[string]tokens.ModelCost
	hourStart time.Time
	hourBase  float64
	hourTotal float64
	hourSeen  bool
}

func newRunMeter() *runMeter {
	return &runMeter{baselines: map[string]runBaseline{}, last: map[string]tokens.ModelCost{}}
}

var defaultRunMeter = newRunMeter()

// paneLinesForRun bounds how much pane text the test/files parsers read.
const paneLinesForRun = 400

// observeCollector refreshes the meter's view of per-agent cumulative cost
// from the token collector. A nil collector or summary leaves the last
// observation in place.
func (m *runMeter) observeCollector(tc *tokens.Collector, now time.Time) {
	if tc == nil {
		return
	}
	summary := tc.Summary()
	if summary == nil {
		return
	}
	m.observeEstimate(tokens.EstimateFromSummary(summary), now)
}

func (m *runMeter) observeEstimate(est tokens.EstimatedCost, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, mc := range est.ByAgent {
		m.last[name] = mc
	}
	hour := now.Truncate(time.Hour)
	if !m.hourSeen || !hour.Equal(m.hourStart) {
		m.hourStart = hour
		m.hourBase = est.TotalUSD
		m.hourSeen = true
	}
	// A cumulative total that fell (pod roll, session-window shift) would
	// otherwise read as negative spend for the rest of the hour.
	if est.TotalUSD < m.hourBase {
		m.hourBase = est.TotalUSD
	}
	m.hourTotal = est.TotalUSD
}

// run returns the live figure for one agent, resetting its baseline when the
// kick time changed. A zero since means the agent is not running and
// the baseline is dropped.
func (m *runMeter) run(name string, since time.Time, now time.Time) *FrontendAgentRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if since.IsZero() {
		delete(m.baselines, name)
		return nil
	}
	cur, priced := m.last[name]
	curTokens := modelCostTokens(cur)
	b, ok := m.baselines[name]
	if !ok || !b.since.Equal(since) {
		b = runBaseline{since: since, usd: cur.USD, tokens: curTokens}
		m.baselines[name] = b
	}
	r := &FrontendAgentRun{
		Since:        since.UTC().Format(time.RFC3339),
		ElapsedS:     int64(now.Sub(since).Seconds()),
		Priced:       priced,
		FilesChanged: -1,
	}
	if r.ElapsedS < 0 {
		r.ElapsedS = 0
	}
	if priced {
		// The cumulative counter can fall across a pod roll; never report a
		// negative run.
		if cur.USD >= b.usd {
			r.CostUSD = cur.USD - b.usd
		}
		if curTokens >= b.tokens {
			r.Tokens = curTokens - b.tokens
		}
	}
	return r
}

func (m *runMeter) fleet(running int) FrontendFleetRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := FrontendFleetRun{Running: running, HourPriced: m.hourSeen}
	if m.hourSeen && m.hourTotal >= m.hourBase {
		f.HourUSD = m.hourTotal - m.hourBase
	}
	return f
}

// latestTotal is the most recent cumulative estimated total the meter has
// observed, and whether it has observed one at all.
func (m *runMeter) latestTotal() (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hourTotal, m.hourSeen
}

func modelCostTokens(mc tokens.ModelCost) int64 {
	return mc.Input + mc.Output + mc.CacheRead + mc.CacheCreate
}

// applyRunMeter attaches the live per-run figure to every busy agent and
// returns the fleet figure. It is called from both status builders.
func applyRunMeter(m *runMeter, agents []FrontendAgent, statuses map[string]*crux.Process, tc *tokens.Collector, now time.Time) FrontendFleetRun {
	m.observeCollector(tc, now)
	running := 0
	for i := range agents {
		a := &agents[i]
		proc := statuses[a.Name]
		if a.Busy != "working" || proc == nil {
			m.run(a.Name, time.Time{}, now)
			continue
		}
		// The kick's delivery time is the run's start: busy == "working" is
		// defined as "that kick's turn has not settled". BusySince is only
		// stamped by the busy-pane watcher, so it is the fallback, not the
		// primary.
		var since time.Time
		if proc.LastKick != nil {
			since = *proc.LastKick
		}
		if since.IsZero() {
			since = proc.BusySince
		}
		if since.IsZero() {
			continue
		}
		running++
		r := m.run(a.Name, since, now)
		lines := proc.PaneLines(paneLinesForRun)
		r.Tests = parseTestSignal(lines)
		r.FilesChanged = parseFilesChanged(lines)
		a.Run = r
	}
	return m.fleet(running)
}

var (
	// go test: "ok  \tpkg\t0.1s" / "PASS" vs "FAIL\tpkg" / "--- FAIL".
	reGoTestOK   = regexp.MustCompile(`^(?:ok\s+\S+|PASS)\b`)
	reGoTestFail = regexp.MustCompile(`^(?:FAIL(?:\s+\S+)?|--- FAIL:)\b`)
	// pytest summary: "=== 12 passed in 1.2s ===" / "=== 1 failed, 11 passed ===".
	rePytestLine = regexp.MustCompile(`^=+ .*\b(\d+ passed|\d+ failed|\d+ error)`)
	// jest/vitest/mocha: "Tests: 1 failed, 3 passed" / "Tests  3 passed".
	reJSTests = regexp.MustCompile(`^Tests:?\s+(.*)$`)
	// git: "3 files changed, 10 insertions(+)" / "1 file changed".
	reFilesChanged = regexp.MustCompile(`\b(\d+) files? changed\b`)
)

// parseTestSignal returns "pass", "fail" or "" from the LAST test-runner
// result line visible in lines. Only whole-run summary lines count, so a
// single `--- FAIL` inside an otherwise green run still reads as fail (it
// is the later line that decides) and a transcript with no runner output
// reads as unknown.
func parseTestSignal(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(stripANSI(lines[i]))
		if l == "" {
			continue
		}
		switch {
		case reGoTestFail.MatchString(l):
			return "fail"
		case reGoTestOK.MatchString(l):
			return "pass"
		case rePytestLine.MatchString(l):
			if strings.Contains(l, " failed") || strings.Contains(l, " error") {
				return "fail"
			}
			return "pass"
		}
		if m := reJSTests.FindStringSubmatch(l); m != nil {
			if strings.Contains(m[1], "failed") {
				return "fail"
			}
			if strings.Contains(m[1], "passed") {
				return "pass"
			}
		}
	}
	return ""
}

// parseFilesChanged returns the N of the last `N files changed` git summary
// line visible in lines, or -1 when none is.
func parseFilesChanged(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if m := reFilesChanged.FindStringSubmatch(stripANSI(lines[i])); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return -1
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return reANSI.ReplaceAllString(s, "")
}
