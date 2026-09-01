package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kubestellar/hive/pkg/tui/client"
	"github.com/kubestellar/hive/pkg/tui/panes"
)

// pollInterval is how often the TUI re-reads the dashboard API.
//
// 5s is not a taste call: it is the cadence the dashboard already refreshes
// at. `dashboard/server.js:111` sets `REFRESH_MS = 5000` and drives its SSE
// push loop from it (`server.js:803`), so 5s is the freshest any consumer of
// this API ever sees — polling faster would re-fetch a snapshot the server has
// not rebuilt, spending request budget for no new information. It also matches
// the client's own 5s request timeout (pkg/tui/client/client.go), which was
// chosen on the same reasoning: a request that outlives its frame is not worth
// waiting for.
//
// T13b replaces this loop with the SSE stream and keeps it as the fallback;
// having picked the stream's own cadence means that switch changes where the
// data comes from without changing how often the frame moves. It is also the
// cadence the fallback returns to: while the stream is up the loop stretches to
// sseReconcileInterval (app.go), and a dropped stream puts it back here.
const pollInterval = 5 * time.Second

// tickMsg is the poll heartbeat. It carries no time: the tick is a "go fetch
// now" signal, not a clock, and nothing reads when it fired. tea.Tick supplies
// the instant to the callback, which discards it — T13b made this a struct for
// the generation below, and a field kept only because the callback is handed
// one would be a value no reader could rely on.
//
// GEN IS WHAT KEEPS THE LOOP SINGLE. tea.Tick fires once and the loop stays
// alive by re-arming from the handler, so "arm the new cadence" and "the old
// cadence is still armed" are the same instant: T13b changes the cadence when
// the SSE stream connects or drops, and arming a replacement chain without
// retiring the old one would leave two live chains ticking forever — one
// cadence change doubling the fetch rate for the rest of the process's life.
// Each chain therefore carries the model's tick generation, and a tick whose
// generation no longer matches is dropped instead of re-armed. That is what
// ends the superseded chain at its next fire rather than running it alongside
// the new one.
type tickMsg struct {
	gen uint64
}

// fetchErrMsg reports that one poll failed.
//
// It never reaches a pane. The app swallows it, which is the whole error
// policy: panes only ever see successful data, so the previous data survives a
// failed fetch by construction rather than by every pane remembering to hold
// onto it. The loop is unaffected — the next tick was already armed before the
// fetch was issued, so a dashboard that is down simply produces a stale frame
// that catches up when it returns.
//
// Nothing displays it yet, and that is a deliberate gap rather than an
// oversight: an error line is UI, and inventing one here would render into the
// frame T3 pinned and the header T13b owns. Carrying the source and the cause
// (rather than discarding them at the point of failure) is what lets that
// later task surface the real message instead of a generic "poll failed".
type fetchErrMsg struct {
	// source names the fetch that failed, so a frame with several polls in
	// flight can say which pane is stale rather than that something is.
	source string
	err    error
}

// Error makes fetchErrMsg an ordinary error value: the task that adds an
// error line can print it verbatim, and a test can assert on the text without
// this type needing accessors.
func (e fetchErrMsg) Error() string {
	return fmt.Sprintf("%s poll failed: %v", e.source, e.err)
}

// scheduleTick arms the next heartbeat.
//
// tea.Tick fires ONCE, so the loop is kept alive by re-arming from the tickMsg
// handler rather than by a repeating timer. That is not merely how bubbletea
// spells it — it is what stops ticks stacking: the next one is scheduled
// relative to the moment this one was handled, so a slow dashboard cannot
// queue up a backlog of pending fetches that all land at once when it
// recovers.
func (m model) scheduleTick() tea.Cmd {
	gen := m.tickGen
	return tea.Tick(m.interval, func(time.Time) tea.Msg {
		return tickMsg{gen: gen}
	})
}

// poll issues every fetch the client can currently make, as one batch.
//
// Seven reads today: /api/agents (T4, #5067), the three T29 wired for the
// Governor pane and the header — /api/status for live governor state,
// /api/config/governor for the evaluation cadence, and /api/hive-id for the
// hive's identity — the two T30 wires for the Tokens pane (/api/tokens for the
// counts and /api/cost for the estimated spend joined onto them), and T31's
// /api/audit for the Events pane. T31 was the one line this comment predicted
// the events fetch would cost: the loop, the error policy and the tick
// scheduling did not change when it landed.
//
// EACH FETCH FAILS ALONE. They are separate Cmds in one batch rather than one
// Cmd making four calls, and that is the failure-isolation property T29 is
// about: a dashboard that serves /api/status but forbids /api/config/governor
// (a common read-only-token shape) must still show a live governor mode, and a
// hive with no configured identity must not stop the Governor pane loading.
// Folding them together would make every value only as available as the least
// available endpoint, because one error return would discard three good
// results. Batched Cmds also run concurrently, so every read costs one round
// trip of wall time, not one each.
//
// /api/audit is the sharpest case for that isolation rather than an exception
// to it. It is the ONE read here that requires read-write or owner access, so
// the read-only token that reaches every other endpoint gets a 403 from it and
// nothing else. Folded in, that single authorization boundary would darken the
// whole frame; kept separate, it costs exactly one pane its refresh.
//
// Deliberately NOT polled: /api/health. It exists and would succeed, but
// nothing in the frame renders it — the header's `ws:` field is SSE connection
// state, which is T13b's, not API reachability. Polling an endpoint whose
// result cannot be displayed would spend a request every 5s to learn nothing.
func (m model) poll() tea.Cmd {
	return tea.Batch(
		m.fetchAgents(),
		m.fetchGovernor(),
		m.fetchGovernorInterval(),
		m.fetchHiveID(),
		m.fetchTokens(),
		m.fetchCosts(),
		m.fetchEvents(),
	)
}

// fetchAgents resolves to a panes.AgentsMsg on success and a fetchErrMsg on
// failure — never to a partial or zero-valued AgentsMsg, which a pane would
// be unable to tell from an empty fleet.
//
// The request is bounded by the client's own 5s timeout rather than by a
// context deadline set here. A second, shorter deadline would silently
// override the one pkg/tui/client documents and make the effective timeout
// depend on which caller you read.
func (m model) fetchAgents() tea.Cmd {
	return func() tea.Msg {
		agents, err := m.api.Agents(context.Background())
		if err != nil {
			return fetchErrMsg{source: "agents", err: err}
		}
		return panes.AgentsMsg{Agents: agents}
	}
}

// governorStatusMsg is a successful live-governor read from GET /api/status.
//
// It is an APP-LEVEL message, not panes.GovernorMsg, and the indirection is
// the fix for the bug T29 exists to close. The pane's message must carry both
// the live status and the configured evaluation interval, but those come from
// two endpoints that fail independently and answer at different times. Sending
// panes.GovernorMsg straight from this fetch would mean sending it with
// whatever interval this Cmd happened to know — zero — which is precisely how
// the pre-T29 SSE path left `next eval` permanently unknown. Instead the app
// caches this, joins it with the last successful interval, and emits one
// GovernorMsg that is always complete. The header reads the same cache for its
// `governor:` field.
type governorStatusMsg struct {
	status client.GovernorStatus
}

// governorIntervalMsg is a successful configuration read from
// GET /api/config/governor.
//
// A zero duration is a legitimate answer — the hive has no evaluation interval
// configured — and is retained as such rather than treated as a miss, because
// the pane renders zero as an honest dash. What must never reach the model is
// the zero produced by a FAILED read, which is why failure travels as
// fetchErrMsg and never as this type carrying a default value.
type governorIntervalMsg struct {
	interval time.Duration
}

// hiveIDMsg is a successful identity read from GET /api/hive-id.
//
// An empty id is a valid answer, kept for the same reason a zero interval is:
// a hive with no configured name renders `hive: —`, and that dash is a fact
// the server reported rather than a fetch that failed. The distinction is
// carried by the type — a failure is a fetchErrMsg — so the header can hold
// the last good identity through an outage instead of blanking on it.
type hiveIDMsg struct {
	id string
}

// fetchGovernor reads the governor's live state.
//
// Live state and configuration are separate fetches on purpose; client.Governor
// documents the same split from the other side. The consequence worth naming
// here is the failure one: this call is the only source of the header's
// governor mode, so it must not be able to fail because a DIFFERENT endpoint
// did.
func (m model) fetchGovernor() tea.Cmd {
	return func() tea.Msg {
		status, err := m.api.Governor(context.Background())
		if err != nil {
			return fetchErrMsg{source: "governor", err: err}
		}
		return governorStatusMsg{status: status}
	}
}

// fetchGovernorInterval reads the governor's configured evaluation cadence.
//
// This is configuration, and client.GovernorEvalInterval notes it is worth
// fetching once rather than every tick. It is nonetheless polled on the normal
// cadence, because the alternative — fetch once at startup — makes the value
// permanently unknown for any TUI that started while the dashboard was down or
// while its token lacked config read access, with no path to recovery short of
// restarting. Re-reading it costs one small request per tick and is what lets
// `next eval` start working the moment access is restored.
func (m model) fetchGovernorInterval() tea.Cmd {
	return func() tea.Msg {
		interval, err := m.api.GovernorEvalInterval(context.Background())
		if err != nil {
			return fetchErrMsg{source: "governor config", err: err}
		}
		return governorIntervalMsg{interval: interval}
	}
}

// fetchHiveID reads the hive's display identity for the header.
//
// It is polled rather than fetched once for the recovery reason above, and
// because identity is cheap: hiveIDResponse is a single string off a dedicated
// endpoint (T6b, #5412) rather than a slice of the large status document.
func (m model) fetchHiveID() tea.Cmd {
	return func() tea.Msg {
		id, err := m.api.HiveID(context.Background())
		if err != nil {
			return fetchErrMsg{source: "hive id", err: err}
		}
		return hiveIDMsg{id: id}
	}
}

// tokenUsageMsg is a successful token-count read from GET /api/tokens.
//
// It is app-level rather than panes.TokensMsg for the reason governorStatusMsg
// is: the pane's frame needs two endpoints, they fail independently, and
// sending the pane message straight from this fetch would send it with whatever
// cost this Cmd happened to know — none. The app caches this, joins it with the
// last cost read, and emits one complete TokensMsg.
//
// An empty usage document is a legitimate answer and is kept as one. A hive
// whose agents have burned no tokens reports zeroes, and the pane must render
// that as a loaded zero-usage frame rather than sitting on "waiting for data"
// forever; carrying success in the TYPE (a failure is a fetchErrMsg) is what
// makes that distinction survive the trip.
type tokenUsageMsg struct {
	usage client.TokenUsage
}

// costSummaryMsg is a successful estimated-cost read from GET /api/cost.
//
// Cost is the OPTIONAL half of the Tokens frame. It never gates delivery: a
// hive whose token endpoint answers and whose cost endpoint does not still gets
// fresh counts, with every cost column rendered as an em dash. That asymmetry
// is deliberate and is why this is a separate message from tokenUsageMsg rather
// than a field on it.
type costSummaryMsg struct {
	summary client.CostSummary
}

// costFetchSource is the fetchErrMsg source naming the /api/cost read.
//
// It is a named constant because it is the ONE fetch whose failure the app acts
// on rather than swallowing (it invalidates the cached estimate), so the
// producer and that consumer must agree on the spelling. A literal in both
// places would let a rename silently turn the invalidation off and leave the
// pane showing a stale dollar figure with no test failing.
const costFetchSource = "cost"

// fetchTokens reads the fleet's token counts.
//
// This is the PRIMARY data for the pane. Its failure is the only failure that
// holds the pane stale, which is what makes fetchCosts safe to lose.
func (m model) fetchTokens() tea.Cmd {
	return func() tea.Msg {
		usage, err := m.api.Tokens(context.Background())
		if err != nil {
			return fetchErrMsg{source: "tokens", err: err}
		}
		return tokenUsageMsg{usage: usage}
	}
}

// fetchCosts reads the dashboard's estimated spend.
//
// Separate from fetchTokens on purpose, and not merely for concurrency: cost is
// a strictly richer read (it prices every model in the summary) and is the one
// a restricted token or an older dashboard is likelier to refuse. Folding it
// into the token fetch would make the counts only as available as the estimate,
// which is precisely the "hold the whole pane stale behind an optional
// estimate" failure this task forbids.
func (m model) fetchCosts() tea.Cmd {
	return func() tea.Msg {
		summary, err := m.api.Costs(context.Background())
		if err != nil {
			return fetchErrMsg{source: costFetchSource, err: err}
		}
		return costSummaryMsg{summary: summary}
	}
}

// tokensMsg projects the model's cached token and cost reads into the one frame
// the Tokens pane consumes.
//
// THIS IS THE ONE PLACE A panes.TokensMsg IS BUILT, for the same reason
// governorMsg is the one place a GovernorMsg is: every rule below has to hold
// on every path, and a second construction site is a second place to get the
// priced/unpriced distinction wrong.
//
// The projection rules, each of which is a fact about the wire and not a
// preference:
//
//   - ROWS COME FROM ByAgentDetail, and their input/output counts stay the
//     /api/tokens values. The cost payload carries its own per-agent input and
//     output, but those are the estimator's view of the same buckets; showing
//     them would make the counts change depending on whether an unrelated
//     endpoint answered.
//
//   - COST ENRICHES A ROW, it never creates one. An agent priced by /api/cost
//     but absent from ByAgentDetail has no counts to show, and a row of dashes
//     with a dollar figure would invite the reading that it spent money without
//     tokens.
//
//   - CostKnown IS THE ENTRY'S SOURCE, never its value. client.CostAgentEntry
//     documents that an unpriced entry carries USD 0 on the wire as a
//     placeholder; rendering that as $0.00 would report a free agent, which is
//     the single most misleading thing this pane could say. Known() is the only
//     thing consulted.
//
//   - FLEET COUNTS COME FROM TotalInput/TotalOutput, not from summing the rows
//     above. AggregateSummary counts every scanned session including ones it
//     could not attribute to a configured agent, so the total is legitimately
//     larger than the visible rows. Re-summing would silently disagree with the
//     web dashboard for the same hive.
//
//   - FLEET COST IS KNOWN ONLY WHEN EVERY MODEL IS PRICED. CostSummary.TotalUSD
//     is a lower bound whenever UnpricedModels is non-empty, and presenting a
//     lower bound as the fleet's spend is the same lie as $0.00 on an unpriced
//     agent, one order of magnitude up.
//
// Map iteration order does not leak: the pane sorts what it is handed
// (panes.sortRows), and this builds rows from a map without imposing any order
// of its own, so the delivered frame is deterministic no matter which order Go
// walked ByAgentDetail in.
func (m model) tokensMsg() panes.TokensMsg {
	costs := m.agentCosts()

	rows := make([]panes.TokenRow, 0, len(m.tokenUsage.ByAgentDetail))
	for name, bucket := range m.tokenUsage.ByAgentDetail {
		if bucket == nil {
			// A null bucket carries no counts. The dashboard does not emit one
			// today, but the field is a pointer map and a row of zeroes
			// attributed to a real agent is worse than no row.
			continue
		}
		row := panes.TokenRow{
			Agent: name,
			TokenCounts: panes.TokenCounts{
				Input:  bucket.Input,
				Output: bucket.Output,
			},
		}
		if entry, ok := costs[name]; ok && entry.Known() {
			row.CostUSD = entry.USD
			row.CostKnown = true
		}
		rows = append(rows, row)
	}

	total := panes.TokenCounts{
		Input:  m.tokenUsage.TotalInput,
		Output: m.tokenUsage.TotalOutput,
	}
	if m.costLoaded && m.costSummary.AllPriced() {
		total.CostUSD = m.costSummary.TotalUSD
		total.CostKnown = true
	}

	return panes.TokensMsg{Agents: rows, Total: total}
}

// agentCosts indexes the cost summary by agent name for the join.
//
// The join key is the agent name exactly as each endpoint spells it, and that
// is a canonical key rather than a hopeful one: both payloads are derived from
// the SAME AggregateSummary.ByAgentDetail map on the server — /api/tokens
// returns its keys directly and /api/cost prices those same buckets
// (pkg/tokens/pricing.go EstimateFromSummary, flattened by
// pkg/dashboard/cost.go). There is no alias layer between them to normalize
// away, so anything cleverer than an exact match would be inventing a
// correspondence the server never made.
//
// It returns nil when no cost read has succeeded, which is what makes the
// cost-failure case fall out of the projection rather than needing a branch:
// every lookup misses and every row is unpriced.
func (m model) agentCosts() map[string]client.CostAgentEntry {
	if !m.costLoaded {
		return nil
	}
	byAgent := make(map[string]client.CostAgentEntry, len(m.costSummary.ByAgent))
	for _, entry := range m.costSummary.ByAgent {
		byAgent[entry.Name] = entry
	}
	return byAgent
}

// fetchEvents reads the operator activity feed for the Events pane.
//
// It resolves to panes.EventsMsg DIRECTLY rather than to an app-level message
// the way fetchGovernor and fetchTokens do, and the asymmetry is the point:
// those two exist because their pane's frame is a JOIN of two independently
// failing endpoints, so a message sent straight from one fetch would carry a
// zero for whatever the other knows. An EventsMsg is complete the moment
// /api/audit answers — there is nothing to join it with and nothing to cache —
// so routing it through the model would add a hop that could only lose or
// reorder data. This is fetchAgents' shape, for fetchAgents' reason.
//
// THE SLICE IS PASSED THROUGH UNTOUCHED. client.Events documents that
// /api/audit returns entries newest first, and panes.Events.replace copies what
// it is handed and re-anchors the viewport on the event that was at the top.
// Sorting, reversing, or appending to the previous snapshot here would each
// break that: the pane would re-anchor against a slice whose order the server
// never produced, and a scrolled-back operator would find the viewport jumping
// on every tick. Collection is this file's job and display is the pane's, and
// the boundary is exactly the slice.
//
// A FAILURE MUST NOT PRODUCE AN EMPTY MESSAGE. panes.Events cannot distinguish
// EventsMsg{Events: nil} from a hive with no recorded activity — it sets
// loaded and replaces the rows for both — so returning a zero-valued EventsMsg
// on error would blank the pane to "no events yet" and reset the operator's
// scroll position every time a fetch failed. Failure travels as fetchErrMsg,
// which the app swallows, so the previous rows and offset survive by
// construction. The successful empty list keeps its own meaning: it really does
// mark the pane loaded and render "no events yet", because a quiet hive is
// entitled to say so.
//
// 403 IS THE EXPECTED CASE, NOT A CRASH. /api/audit is the only poll read that
// demands read-write or owner access, so a perfectly healthy TUI driven by a
// read-only token is refused here on every tick. That returns the client's
// typed APIError down the same fetchErrMsg path as any other non-2xx, carrying
// its source so a later error-surface task can name this pane; nothing about it
// terminates the program or is special-cased here.
func (m model) fetchEvents() tea.Cmd {
	return func() tea.Msg {
		events, err := m.api.Events(context.Background())
		if err != nil {
			return fetchErrMsg{source: "events", err: err}
		}
		return panes.EventsMsg{Events: events}
	}
}
