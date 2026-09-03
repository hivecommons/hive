package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
// Today that is /api/agents alone (T4, #5067 — the only typed read merged so
// far). The governor, tokens and events fetches (T6/T8/T10) each add one line
// here and one message type in pkg/tui/panes; the loop, the error policy and
// the tick scheduling do not change when they land.
//
// Deliberately NOT polled: /api/health. It exists and would succeed, but
// nothing in the frame renders it — the header's `ws:` field is SSE connection
// state, which is T13b's, not API reachability. Polling an endpoint whose
// result cannot be displayed would spend a request every 5s to learn nothing.
func (m model) poll() tea.Cmd {
	return tea.Batch(
		m.fetchAgents(),
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
