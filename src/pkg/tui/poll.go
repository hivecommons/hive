package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
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
//
// T32 splits the loop in two, and this is the cadence of both — for opposite
// reasons. The ACTIVITY loop runs at it unconditionally, because no stream
// event can refresh what it reads and 5s is the freshest the server rebuilds
// at. The RECONCILIATION loop runs at it only while the stream is down, and
// stretches to sseReconcileInterval once the stream proves itself.
const pollInterval = 5 * time.Second

// The two poll heartbeats. Neither carries a time: a tick is a "go fetch now"
// signal, not a clock, and nothing reads when it fired. tea.Tick supplies the
// instant to the callback, which discards it — T13b made this a struct for the
// generation below, and a field kept only because the callback is handed one
// would be a value no reader could rely on.
//
// THERE ARE TWO CHAINS BECAUSE THERE ARE TWO KINDS OF DATA, not because there
// are two intervals. The stream carries live agent and governor state, so the
// reconciliation loop can afford to slow to a minute once it is healthy — it
// is only there to catch a roster change or a silently stalled stream. Nothing
// on the stream carries token counts, estimated cost, or audit rows, so the
// activity loop's endpoints are the ONLY source those three panes have. Attach
// them to the stretching timer, as they were before T32, and a healthy `ws:`
// indicator would paradoxically make half the screen twelve times staler —
// the frame looking its most alive at the moment it stopped being.
//
// GEN IS WHAT KEEPS EACH LOOP SINGLE. tea.Tick fires once and a loop stays
// alive by re-arming from its handler, so "arm the new cadence" and "the old
// cadence is still armed" are the same instant: T13b changes the reconcile
// cadence when the stream connects or drops, and arming a replacement chain
// without retiring the old one would leave two live chains ticking forever —
// one cadence change doubling the fetch rate for the rest of the process's
// life. Each chain therefore carries its own generation, and a tick whose
// generation no longer matches is dropped instead of re-armed. That is what
// ends the superseded chain at its next fire rather than running it alongside
// the new one.
//
// THE GENERATIONS ARE SEPARATE COUNTERS, and that separation is load-bearing
// rather than symmetry for its own sake. A shared counter would mean the bump
// that retires the stretched reconcile chain on an SSE drop also retires the
// pending activity tick — which nothing re-arms, because the drop path arms a
// reconcile chain and only a reconcile chain. The Tokens and Events panes
// would freeze permanently at the first stream drop, which is the exact
// failure this task exists to prevent, arrived at from the other direction.
type (
	// reconcileTickMsg drives the loop whose cadence the stream changes.
	reconcileTickMsg struct {
		gen uint64
	}

	// activityTickMsg drives the fixed loop. Its generation is never bumped
	// today — the cadence has nothing to respond to — and the guard is kept
	// anyway because it is what makes that true STRUCTURALLY: the chain cannot
	// be retired by a reconcile-side bump, only by a deliberate one here.
	activityTickMsg struct {
		gen uint64
	}
)

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

// scheduleReconcileTick arms the next reconciliation heartbeat.
//
// tea.Tick fires ONCE, so a loop is kept alive by re-arming from its own tick
// handler rather than by a repeating timer. That is not merely how bubbletea
// spells it — it is what stops ticks stacking: the next one is scheduled
// relative to the moment this one was handled, so a slow dashboard cannot
// queue up a backlog of pending fetches that all land at once when it
// recovers.
func (m model) scheduleReconcileTick() tea.Cmd {
	gen := m.reconcileGen
	return tea.Tick(m.reconcileInterval, func(time.Time) tea.Msg {
		return reconcileTickMsg{gen: gen}
	})
}

// scheduleActivityTick arms the next activity heartbeat.
//
// Same shape, deliberately separate function rather than one parameterised by
// which class it is arming. The pair a caller must get right is (interval,
// generation), and every caller here picks it by naming the loop instead of by
// passing two arguments that could be mismatched — arming the activity chain
// with the reconcile generation would stamp it for retirement by the next
// cadence change, and nothing about the call site would look wrong.
func (m model) scheduleActivityTick() tea.Cmd {
	gen := m.activityGen
	return tea.Tick(m.activityInterval, func(time.Time) tea.Msg {
		return activityTickMsg{gen: gen}
	})
}

// poll issues every fetch the client can currently make, as one batch.
//
// It is the ONE-SHOT full refresh, not a loop: nothing here arms a tick. It is
// what Init uses to fill the whole frame before either chain's first interval
// elapses, and what an action handler uses after a write — a pause, a model
// apply, an ACMM apply, or returning from an attached tmux session — where
// every class of data is stale at once. A write moves the roster AND appends
// the operator's own action to the audit log, so refreshing one class and
// waiting out the other's cadence would show the effect of an action before
// the record of it.
//
// Seven reads today, split between the two loops below: see pollReconcile for
// the four the stream reconciles against and pollActivity for the three it
// cannot carry.
//
// EACH FETCH FAILS ALONE. They are separate Cmds in one batch rather than one
// Cmd making seven calls, and that is the failure-isolation property T29 is
// about: a dashboard that serves /api/status but forbids /api/config/governor
// (a common read-only-token shape) must still show a live governor mode, and a
// hive with no configured identity must not stop the Governor pane loading.
// Folding them together would make every value only as available as the least
// available endpoint, because one error return would discard three good
// results. Batched Cmds also run concurrently, so seven reads cost one round
// trip of wall time, not seven — and that is why splitting the batch in two
// costs nothing: two concurrent batches are still one round trip.
//
// Deliberately NOT polled: /api/health. It exists and would succeed, but
// nothing in the frame renders it — the header's `ws:` field is SSE connection
// state, which is T13b's, not API reachability. Polling an endpoint whose
// result cannot be displayed would spend a request every 5s to learn nothing.
func (m model) poll() tea.Cmd {
	return tea.Batch(m.pollReconcile(), m.pollActivity())
}

// pollReconcile reads the state the SSE stream also carries.
//
// Four reads: /api/agents (T4, #5067) and the three T29 wired for the Governor
// pane and the header — /api/status for live governor state,
// /api/config/governor for the evaluation cadence, and /api/hive-id for the
// hive's identity.
//
// This is the batch whose cadence the stream changes, and it is the batch that
// can afford to: every value here either arrives on the stream (agent states,
// governor status) or changes on human timescales (identity, the configured
// eval interval). At 60s it is doing the job sseReconcileInterval describes —
// noticing a roster change the stream does not announce, and bounding how long
// a silently stalled stream can show a stale frame.
//
// /api/config/governor and /api/hive-id are in the reconcile class rather than
// the activity one even though no stream event carries them, because the class
// is about how fast the DATA moves, not about which endpoint feeds it. A hive
// whose name or evaluation cadence changed a minute ago is not a stale frame;
// a token count that is a minute old, on a fleet burning tokens continuously,
// is.
func (m model) pollReconcile() tea.Cmd {
	return tea.Batch(
		m.fetchAgents(),
		m.fetchGovernor(),
		m.fetchGovernorInterval(),
		m.fetchHiveID(),
	)
}

// pollActivity reads what the SSE stream cannot carry.
//
// Three reads: /api/tokens for the counts and /api/cost for the estimated
// spend joined onto them (T30), and /api/audit for the Events pane (T31).
// /api/events is deliberately absent: it is the long-lived SSE status stream,
// not the poll-shaped activity feed, and its payload contains none of these.
//
// This batch's cadence NEVER changes. There is no stream event that refreshes
// a token count or appends an audit row, so slowing it while the stream is
// healthy would not be trading polling for pushing — it would just be polling
// less. Before T32 these three shared the reconciliation timer, which meant a
// connected stream stretched them to 60s: the Tokens and Events panes were at
// their stalest precisely when the header said the frame was live.
func (m model) pollActivity() tea.Cmd {
	return tea.Batch(
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

// fetchEvents reads the newest-first operator activity snapshot.
//
// Client.Events owns the /api/audit path and the wire shape. This command does
// not sort, append to, or copy the returned slice: a successful snapshot is
// handed straight to the pane, which owns replacement and scroll anchoring. A
// failed read becomes fetchErrMsg instead of an empty EventsMsg, so 403s and
// outages cannot erase the last good rows or reset the operator's position.
func (m model) fetchEvents() tea.Cmd {
	return func() tea.Msg {
		events, err := m.api.Events(context.Background())
		if err != nil {
			return fetchErrMsg{source: "events", err: err}
		}
		return panes.EventsMsg{Events: events}
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
