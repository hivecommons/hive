// Package tui implements Hive's full-screen terminal dashboard — the
// keyboard-driven operator view over the same dashboard API the web UI and
// hivectl already consume (kubestellar/hive#4907).
//
// This package is the TUI's own root. It deliberately holds no Hive logic: the
// TUI is a second CLIENT of the dashboard API, not a second implementation of
// the fleet model, so everything it displays arrives over the documented HTTP
// contract in dashboard/openapi.json.
//
// T3 (#5004): the frame is a header bar, a 2×2 grid of the four panes from
// pkg/tui/panes, and a footer keybinding strip, per the layout sketch in
// src/docs/design/tui.md §3.
//
// T12 (#5061): the frame is live. A tick every pollInterval issues the client
// fetches that exist and delivers each result to the panes as that pane's own
// message type; see poll.go for the loop, its cadence and its error policy.
// The SSE feed (T13) and the per-pane content (T5/T7/T9/T11) build on it.
//
// T24 (#5138): the frame is size-aware. The grid already re-derived itself from
// the last tea.WindowSizeMsg on every render, so it shares the space at any
// size for free; what T24 adds is the FLOOR. Below minWidth x minHeight the
// grid is not shrunk, it is replaced by a single centred message, per the
// design doc's note on the sketch (src/docs/design/tui.md §3).
// T25 (#5139): every color the frame draws comes from theme.go, as a
// light/dark pair. No call site names a color of its own.
//
// T13b (#5215): the frame is PUSHED to. The TUI subscribes to the dashboard's
// SSE stream (T13a) at startup and translates each event into the same pane
// messages the poll produces, so a pane moves the moment the server publishes
// rather than on the next tick. The poll does not go away — it becomes the
// fallback and the reconciler: healthy stream, 60s reconcile; dropped stream,
// back to the 5s poll and a header that says so. See the SSE section at the
// bottom of this file.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
	"github.com/hivecommons/hive/pkg/tui/theme"
)

// splash is drawn only before the first tea.WindowSizeMsg arrives. It names
// the binding that gets an operator back out, because a full-screen program
// that does not say how to exit is a trap — especially over SSH.
const splash = "Hive TUI (q to quit)"

// paneCount is the grid's four cells. Focus arithmetic uses it so adding a
// pane later cannot silently desynchronize tab cycling from the pane table.
const paneCount = 4

// minWidth and minHeight are the smallest terminal the grid is drawn in.
//
// The numbers come from what the frame needs to be READABLE rather than from
// what it needs to avoid panicking — the cell() clamp already makes any size
// render without crashing. Below this, every cell's interior is a couple of
// columns wide after two borders and a halved terminal, which is a frame that
// draws but says nothing. Showing an operator a stack of empty boxes is worse
// than telling them the window is too small, so this is the floor.
const (
	minWidth  = 60
	minHeight = 20
)

// tooSmallText is the whole below-minimum frame's content. It is derived from
// the constants rather than spelled out, so the numbers an operator is told to
// resize to cannot drift away from the numbers actually enforced.
var tooSmallText = fmt.Sprintf("terminal too small (need at least %dx%d)", minWidth, minHeight)

// Border styles for the grid cells. The focused pane gets a THICK border, not
// only a color change: test and CI environments render through termenv's
// Ascii profile where colors are stripped, so a color-only highlight would be
// invisible exactly where the golden file pins the frame. The thick border
// survives any profile; the color is a refinement on real terminals.
//
// The colors come from theme (T25) rather than being written here: a literal
// at the call site is a value chosen against ONE background, and the focus
// border in particular was invisible on a light terminal. See theme.go.
var (
	unfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(theme.Border)
	focusedBorder = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(theme.BorderFocus)
	headerStyle     = lipgloss.NewStyle().Bold(true)
	footerStyle     = lipgloss.NewStyle().Faint(true)
	confirmBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(theme.BorderFocus).
			Padding(0, 1)
	confirmErrorStyle = lipgloss.NewStyle().Bold(true)
)

// headerFormat is the header bar, with the SSE connection state as its only
// live field after T13b. The other two still carry placeholders, and each one
// is a fact about what is not fetched yet rather than an oversight:
//
//   - `hive:` — the hive's name is on GET /api/status (StatusPayload.HiveID),
//     which no merged client method reads. T6's /api/status client decodes the
//     governor slice; until a client call exposes the id, inventing one here
//     would be a guess rendered as a fact.
//   - `governor:` — same endpoint, T6/T7's to fill.
//
// A dash is "not known", which is true; any value polled data does not support
// would be false.
const headerFormat = "hive: —   governor: —   ws: %s"

// The two `ws:` values, and why there are only two.
//
// "connected" means an event has been RECEIVED on the stream, not that a
// socket was opened: client.StreamEvents hands back its channels before the
// request is even dialled, so a successful connect is not observable through
// its contract — the first event is. Everything else is "not connected", and
// that deliberately covers three situations the operator does not need told
// apart: before the first event, while a reconnect is backing off, and after a
// drop. All three mean the same thing about what is on screen — the numbers
// are coming from the 5s poll, not from the stream — which is the only
// distinction the header exists to draw.
const (
	wsConnected    = "connected"
	wsNotConnected = "not connected"
)

// footerText lists only the bindings that EXIST. The sketch's full strip
// (m model, A acmm, …) documents keys whose tasks have not landed;
// showing them now would advertise actions that silently do nothing. Each
// action task appends its own binding when it wires the key.
const footerText = "tab focus  p pause/resume  K kick  a attach  ? help  q quit"

// confirmState is the pause/resume dialog. It remains present while the HTTP
// command is in flight so every other key stays behind the modal, and it also
// holds a failed call's message so an API error becomes UI rather than a
// process-ending error.
type confirmState struct {
	agent    string
	pause    bool
	pending  bool
	actionID uint64
	err      string
}

// agentActionMsg is the asynchronous result of confirming the dialog. The
// original target travels with it so a late response cannot close or rewrite
// a newer modal the operator opened after dismissing the in-flight one.
type agentActionMsg struct {
	actionID uint64
	agent    string
	pause    bool
	result   client.AgentActionResult
	err      error
}

// kickResultMsg is the asynchronous result of asking the dashboard to queue a
// kick. The target travels with the result because an empty Agent in a malformed
// success response must not erase the operator's target from the status text.
type kickResultMsg struct {
	agent  string
	result client.KickResult
	err    error
}

// model is the root bubbletea model.
//
// It is a VALUE type with value receivers, which is the bubbletea convention
// rather than an accident: Update returns the next model instead of mutating
// the current one, so a message handler can never leave a half-updated frame
// visible if it returns early.
type model struct {
	// width and height track the terminal size reported by tea.WindowSizeMsg.
	// They are zero until the first message arrives — bubbletea sends one at
	// startup, but View can be called before it lands, so View must tolerate
	// zero rather than assume it has been sized.
	width  int
	height int

	// panes holds the grid's cells in reading order: 0 Agents (top-left),
	// 1 Governor (top-right), 2 Tokens (bottom-left), 3 Events
	// (bottom-right) — the numbering the design sketch's [1]..[4] badges use,
	// zero-based.
	panes [paneCount]panes.Pane

	// focus indexes the focused pane. Exactly one pane is always focused;
	// there is no "nothing focused" state to handle everywhere else.
	focus int

	// api is the dashboard client every poll goes through. client.New cannot
	// fail — a bad HIVE_DASHBOARD_URL surfaces as a request error on the first
	// tick rather than as a constructor error the TUI has no frame to render
	// yet — so the model always has one and poll never has to nil-check it.
	api *client.Client

	// helpVisible is whether the help overlay is up. While it is, the overlay
	// swallows EVERY key — including q — so a reader dismissing it cannot
	// accidentally quit the program instead (see Update).
	helpVisible bool

	// confirm is non-nil while a pause/resume dialog is open. Like help, it
	// owns every key while visible; unlike help, only y, n and esc act on it.
	confirm *confirmState

	// actionSeq identifies each confirmed HTTP call. Agent and verb alone are
	// not enough: an operator can dismiss an in-flight request and open the
	// same action again, and the first response must not close the new modal.
	actionSeq uint64

	// attachPending covers both the short tmux session preflight and the time
	// spent inside the attached session. It prevents repeated `a` presses from
	// queuing multiple terminal-suspending commands before the first preflight
	// completes.
	attachPending bool

	// kickPending is the selected agent whose kick request is currently in
	// flight. Only one local request is allowed at a time, so repeated K presses
	// cannot enqueue duplicate commands before the dashboard answers.
	kickPending string

	// footerStatus is the latest action result rendered in place of the normal
	// binding strip. Whichever asynchronous action answers last is the status
	// the operator sees. Kick success describes only queueing or deduplication.
	footerStatus string

	// interval is this model's poll cadence, defaulting to pollInterval.
	//
	// It is a field rather than a bare constant read for two reasons. Tests
	// drive a whole tick — fetch, delivery, and the re-arm — without waiting
	// five real seconds for it. And T13b needs exactly this knob: once the SSE
	// stream is connected the poll becomes a fallback and should slow down,
	// not keep hammering an endpoint the stream has already superseded.
	interval time.Duration

	// tickGen retires superseded poll chains. Changing interval only affects
	// the NEXT re-arm, so switching cadence means arming a fresh chain while
	// the old one is still pending; bumping this makes the pending tick a
	// no-op instead of a second, permanent loop. See tickMsg in poll.go.
	tickGen uint64

	// agents is the last fleet roster a poll returned.
	//
	// The stream carries live per-agent state but not the /api/agents contract
	// panes.Agents draws its rows from, so an SSE update joins its states onto
	// this list rather than rebuilding the roster from a second source that
	// would disagree with the polled one about who is in the fleet. This is
	// the join panes.AgentsMsg.States was defined for.
	agents []client.Agent

	// sse is the live stream, or nil while there is none. It is a pointer
	// because it owns a cancel func and two channels — things a value-typed
	// model copies by reference on purpose, so cancelling reaches the one
	// goroutine that exists rather than a copy of it.
	sse *sseStream

	// sseGen identifies the current stream attempt. Every SSE message carries
	// the generation it was produced for, so a late event or error from a
	// stream that has already been replaced is dropped instead of degrading
	// (or resurrecting) the connection state of its successor.
	sseGen uint64

	// sseConnected is whether an event has arrived on the current stream. It
	// is the header's `ws:` field and the reason the poll is stretched; see
	// wsConnected for why receipt, not dial, is the signal.
	sseConnected bool

	// sseBackoff is the delay before the NEXT reconnect attempt, zero before
	// the first failure. It doubles per consecutive failure and is reset by
	// any received event.
	sseBackoff time.Duration
}

// newModel returns the root model in its initial state. Unexported because the
// program is entered through Run; the tests use it directly to drive the model
// without a terminal.
func newModel() model {
	return model{
		panes: [paneCount]panes.Pane{
			panes.NewAgents(),
			panes.NewGovernor(),
			panes.NewTokens(),
			panes.NewEvents(),
		},
		api:      client.New(),
		interval: pollInterval,
	}
}

// New returns the TUI's root model for embedding in another bubbletea program
// or driving under teatest. The panes' golden test lives next to the panes
// (pkg/tui/panes/testdata, per the design doc's testing convention) and this
// is its entry point; hivectl's own entry stays Run.
func New() tea.Model {
	return newModel()
}

// Init implements tea.Model.
//
// It gathers the panes' initial commands and starts the poll loop: one fetch
// immediately, and the first tick armed for pollInterval later. The immediate
// fetch is what keeps startup honest — without it every pane would show
// "waiting for data" for a full interval while a perfectly reachable dashboard
// sat there answering, and an operator would read that as the TUI being broken.
//
// The SSE subscription starts here too, and starts ALONGSIDE the poll rather
// than instead of it: the stream's first event may be seconds away (or never
// arrive, on a dashboard that is down), and the first frame must not wait on
// it. The poll stretches only once the stream has proved itself.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.poll(), m.scheduleTick(), m.connectSSE()}
	for _, p := range m.panes {
		if c := p.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if msg.gen != m.tickGen {
			// A tick from a chain the cadence change retired. Dropping it
			// without re-arming is what ends that chain; see tickMsg.
			return m, nil
		}
		// Re-arm BEFORE the fetches are issued, not after they resolve: the
		// loop's cadence must not depend on how long a fetch takes, and a
		// dashboard that never answers must not be able to stop the clock.
		return m, tea.Batch(m.scheduleTick(), m.poll())
	case fetchErrMsg:
		// Swallowed on purpose — see fetchErrMsg's doc comment. Returning here
		// rather than falling through to the broadcast below is the mechanism:
		// the panes never see the error, so they never have to decide whether
		// to clear their data, and the previous frame simply persists.
		return m, nil
	case sseOpenMsg:
		return m.handleSSEOpen(msg)
	case sseEventMsg:
		return m.handleSSEEvent(msg)
	case sseDroppedMsg:
		return m.handleSSEDropped(msg)
	case sseReconnectMsg:
		if msg.gen != m.sseGen {
			return m, nil
		}
		return m, m.connectSSE()
	case panes.AgentsMsg:
		// Remember the roster on its way to the pane. A poll-sourced snapshot
		// carries the fleet; an SSE-sourced one carries the same slice back
		// with live states joined onto it, so re-caching is a no-op there.
		if msg.Agents != nil {
			m.agents = msg.Agents
		}
		next, cmd := m.broadcast(msg)
		return next, cmd
	case agentActionMsg:
		return m.handleAgentAction(msg)
	case kickResultMsg:
		return m.handleKickResult(msg)
	case attachReadyMsg:
		if msg.err != nil {
			m.attachPending = false
			m.footerStatus = "Attach failed: " + msg.err.Error()
			return m, nil
		}
		return m, tea.ExecProcess(msg.cmd, func(err error) tea.Msg {
			return attachDoneMsg{err: err}
		})
	case attachDoneMsg:
		m.attachPending = false
		if msg.err != nil {
			m.footerStatus = "Attach failed: " + msg.err.Error()
		} else {
			m.footerStatus = ""
		}
		return m, m.poll()
	case tea.KeyMsg:
		if m.confirm != nil {
			return m.updateConfirm(msg)
		}
		// The help overlay is modal and dismisses on ANY key, so it is handled
		// before the global bindings rather than as one of them. Order is the
		// whole mechanism: falling through would let "q" quit the program while
		// the reader believed they were closing a dialog, which is the one
		// misfire a help screen must not have. It also makes "?" a toggle for
		// free — the second press lands here.
		if m.helpVisible {
			m.helpVisible = false
			return m, nil
		}
		// KeyMsg.String() normalizes both plain runes ("q") and control
		// combinations ("ctrl+c", "shift+tab") into one comparable form, so
		// the global bindings can be listed together rather than split
		// across a type switch on key type.
		switch msg.String() {
		case "?":
			m.helpVisible = true
			return m, nil
		case "q", "ctrl+c":
			return m.stopSSE(), tea.Quit
		case "tab":
			m.focus = (m.focus + 1) % paneCount
			return m, nil
		case "shift+tab":
			// +paneCount-1 rather than -1: keeps the operand positive, so
			// the modulo never sees a negative number to round wrongly.
			m.focus = (m.focus + paneCount - 1) % paneCount
			return m, nil
		case "p":
			if m.focus != 0 {
				return m, nil
			}
			agents, ok := m.panes[0].(panes.Agents)
			if !ok {
				return m, nil
			}
			name, paused, ok := agents.SelectedAgent()
			if !ok {
				return m, nil
			}
			m.confirm = &confirmState{agent: name, pause: !paused}
			return m, nil
		case "K":
			if m.focus != 0 || m.kickPending != "" {
				return m, nil
			}
			agents, ok := m.panes[0].(panes.Agents)
			if !ok {
				return m, nil
			}
			name, _, ok := agents.SelectedAgent()
			if !ok {
				return m, nil
			}
			m.kickPending = name
			m.footerStatus = ""
			return m, m.kickAgent(name)
		case "a":
			if m.focus != 0 || m.attachPending {
				return m, nil
			}
			agents, ok := m.panes[0].(panes.Agents)
			if !ok {
				return m, nil
			}
			name, _, ok := agents.SelectedAgent()
			if !ok {
				return m, nil
			}
			m.attachPending = true
			m.footerStatus = ""
			return m, prepareAttach(name)
		}
		// Any other key belongs to the focused pane. The T3 stubs ignore
		// everything, but routing through this seam now is what lets a pane
		// task add j/k selection without touching the app's key handling.
		var cmd tea.Cmd
		m.panes[m.focus], cmd = m.panes[m.focus].Update(msg)
		return m, cmd
	}
	// Non-key messages go to every pane: a poll result or SSE event (T12,
	// T13b) is not addressed to whichever pane happens to be focused.
	next, cmd := m.broadcast(msg)
	return next, cmd
}

// broadcast delivers one message to every pane and returns the updated model
// with their commands batched.
//
// It returns the concrete model rather than tea.Model so the SSE path can
// deliver SEVERAL messages out of one event — an agents snapshot and a
// governor snapshot from the same status payload — by threading the result of
// one broadcast into the next, instead of discarding the panes each one
// updated.
func (m model) broadcast(msg tea.Msg) (model, tea.Cmd) {
	var cmds []tea.Cmd
	for i, p := range m.panes {
		next, c := p.Update(msg)
		m.panes[i] = next
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

// View implements tea.Model.
func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		// Not sized yet. Return the bare line rather than laying out into a
		// zero-sized box, which would render as an empty frame — a blank
		// screen for however long it takes the first WindowSizeMsg to
		// arrive.
		return splash
	}

	if m.width < minWidth || m.height < minHeight {
		return m.tooSmallView()
	}

	// One line each for header and footer; the grid gets the rest, split
	// into two rows and two columns. The right column and bottom row absorb
	// the odd remainder so the frame always fills the terminal exactly.
	gridH := m.height - 2
	topH := gridH / 2
	botH := gridH - topH
	leftW := m.width / 2
	rightW := m.width - leftW

	cell := func(i, outerW, outerH int) string {
		style := unfocusedBorder
		if i == m.focus {
			style = focusedBorder
		}
		// The border consumes one row/column on every side; the pane
		// renders only the interior. The clamp stays after T24 even though
		// the minimum-size guard now keeps the grid out of the sizes that
		// need it: it is a defence against a later layout change reserving
		// more chrome, not a duplicate of the guard.
		innerW := max(0, outerW-2)
		innerH := max(0, outerH-2)
		return style.Render(m.panes[i].View(innerW, innerH))
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top,
		cell(0, leftW, topH), cell(1, rightW, topH))
	bottom := lipgloss.JoinHorizontal(lipgloss.Top,
		cell(2, leftW, botH), cell(3, rightW, botH))

	header := headerStyle.Width(m.width).Render(m.headerText())
	footerTextForFrame := footerText
	if m.footerStatus != "" {
		footerTextForFrame = m.footerStatus
	}
	footer := footerStyle.Width(m.width).MaxWidth(m.width).Render(footerTextForFrame)

	frame := lipgloss.JoinVertical(lipgloss.Left, header, top, bottom, footer)
	if m.confirm != nil {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.confirmView())
	}
	if m.helpVisible {
		// Place, not Join: the overlay sits ON the frame rather than taking
		// rows from it, so the grid keeps the exact geometry it had and the
		// frame is still the terminal's size when the overlay is dismissed.
		// panes.Help() sizes itself to its content; centring it is this
		// layer's job, the same split pane.go draws between content and the
		// chrome around it.
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panes.Help())
	}
	return frame
}

// updateConfirm consumes every key while the pause/resume dialog is open.
// Unknown keys — including q and tab — intentionally do nothing rather than
// leaking through to the frame underneath.
func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n", "esc":
		m.confirm = nil
		return m, nil
	case "y":
		if m.confirm.pending {
			return m, nil
		}
		// The model is a value type, but confirm is a pointer so nil can mean
		// "closed". Copy its value before editing to preserve Update's rule that
		// the input model is never mutated through shared pointer state.
		confirm := *m.confirm
		m.confirm = &confirm
		m.confirm.pending = true
		m.confirm.err = ""
		m.actionSeq++
		m.confirm.actionID = m.actionSeq
		target := *m.confirm
		return m, m.agentAction(target)
	default:
		return m, nil
	}
}

func (m model) agentAction(target confirmState) tea.Cmd {
	return func() tea.Msg {
		var result client.AgentActionResult
		var err error
		if target.pause {
			result, err = m.api.PauseAgent(context.Background(), target.agent)
		} else {
			result, err = m.api.ResumeAgent(context.Background(), target.agent)
		}
		return agentActionMsg{
			actionID: target.actionID,
			agent:    target.agent,
			pause:    target.pause,
			result:   result,
			err:      err,
		}
	}
}

func (m model) handleAgentAction(msg agentActionMsg) (tea.Model, tea.Cmd) {
	matchesOpenModal := m.confirm != nil &&
		m.confirm.actionID == msg.actionID &&
		m.confirm.agent == msg.agent && m.confirm.pause == msg.pause
	if msg.err != nil {
		if matchesOpenModal {
			confirm := *m.confirm
			m.confirm = &confirm
			m.confirm.pending = false
			verb := "Pause"
			if !msg.pause {
				verb = "Resume"
			}
			if client.IsForbidden(msg.err) {
				m.confirm.err = verb + " failed: owner access required"
			} else {
				m.confirm.err = fmt.Sprintf("%s failed: %v", verb, msg.err)
			}
		}
		return m, nil
	}

	// State, not Status, is the operation's authoritative post-call value.
	// This also handles Changed=false correctly: a no-op response still fixes
	// a stale row before the refresh arrives.
	name := msg.result.Agent
	if name == "" {
		name = msg.agent
	}
	if agents, ok := m.panes[0].(panes.Agents); ok {
		m.panes[0] = agents.SetAgentPaused(name, msg.result.Paused())
	}
	if matchesOpenModal {
		m.confirm = nil
	}
	return m, m.poll()
}

func (m model) kickAgent(agent string) tea.Cmd {
	return func() tea.Msg {
		result, err := m.api.KickAgent(context.Background(), agent, "")
		return kickResultMsg{agent: agent, result: result, err: err}
	}
}

func (m model) handleKickResult(msg kickResultMsg) (tea.Model, tea.Cmd) {
	// A result for anything other than the one local request still pending is
	// stale. Ignoring it prevents an old response from clearing or rewriting a
	// newer action if messages are replayed by an embedding program.
	if m.kickPending != msg.agent {
		return m, nil
	}
	m.kickPending = ""

	if msg.err != nil {
		if client.IsForbidden(msg.err) {
			m.footerStatus = "Kick failed: owner access required"
		} else {
			m.footerStatus = fmt.Sprintf("Kick failed: %v", msg.err)
		}
		return m, nil
	}

	agent := msg.result.Agent
	if agent == "" {
		agent = msg.agent
	}
	switch msg.result.Status {
	case "queued":
		m.footerStatus = "kick queued for " + agent
	case "in-flight":
		m.footerStatus = "kick already in flight for " + agent + " (request deduplicated)"
	default:
		m.footerStatus = fmt.Sprintf("Kick returned status %q for %s", msg.result.Status, agent)
	}
	return m, nil
}

func (m model) confirmView() string {
	verb := "Pause"
	if !m.confirm.pause {
		verb = "Resume"
	}
	body := fmt.Sprintf("%s agent %s?", verb, m.confirm.agent)
	switch {
	case m.confirm.err != "":
		body += "\n\n" + confirmErrorStyle.Render(m.confirm.err) + "\n\nPress y to retry or n/esc to cancel"
	case m.confirm.pending:
		body += "\n\nWorking…"
	default:
		body += "\n\ny confirm  n/esc cancel"
	}

	// At the minimum supported frame width this leaves one cell of breathing
	// room on each side after the border and padding. Width also makes long API
	// errors wrap inside the modal instead of growing the terminal frame.
	contentWidth := min(52, max(1, m.width-6))
	return confirmBoxStyle.Width(contentWidth).Render(body)
}

// headerText renders the header bar for this model's connection state.
func (m model) headerText() string {
	ws := wsNotConnected
	if m.sseConnected {
		ws = wsConnected
	}
	return fmt.Sprintf(headerFormat, ws)
}

// tooSmallView renders the below-minimum frame: the message alone, centred in
// the terminal, and nothing else.
//
// The message is wrapped to the terminal's width and the result is clipped to
// its exact width and height, so the frame fits ANY size — including one
// narrower than the message itself, which lipgloss.Place alone would happily
// overflow. That matters because this is precisely the path a too-narrow
// terminal takes: a minimum-size message that itself wraps past the right edge
// is the same broken frame it exists to avoid.
func (m model) tooSmallView() string {
	msg := lipgloss.NewStyle().
		Width(m.width).
		Align(lipgloss.Center).
		Render(tooSmallText)
	placed := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(placed)
}

// ── SSE (T13b) ───────────────────────────────────────────────────────────────

// sseReconcileInterval is the poll cadence while the stream is healthy.
//
// The poll is not switched off when the stream connects; it is demoted to a
// RECONCILER, and two things make that worth a request a minute. The stream
// carries live state but not the /api/agents roster panes.Agents draws its
// rows from, so something still has to notice an agent being added or removed.
// And a stream that is up is not proof the frame is current: a dropped event,
// or a server that stopped publishing while holding the connection open, looks
// exactly like a quiet hive. 60s is twelve times the push cadence — slow
// enough that the stream is plainly doing the work, often enough that a
// silently stale frame cannot outlive a minute.
const sseReconcileInterval = 60 * time.Second

// The reconnect backoff. It starts at a second so a momentary blip costs
// almost nothing, and stops doubling at 30s so a dashboard that is down for an
// hour is still retried steadily rather than at an interval that has grown
// past usefulness. The fallback poll runs at pollInterval throughout, so the
// backoff never decides how fresh the frame is — only how soon push resumes.
const (
	sseBackoffMin = 1 * time.Second
	sseBackoffMax = 30 * time.Second
)

// errSSEClosed is a stream that ended without an error of its own: the server
// closed cleanly. For the frame it means what an error means — nothing is
// being pushed any more — so it travels the same path rather than being a
// second, silent case.
var errSSEClosed = errors.New("sse stream closed")

// sseStream is one live subscription: the channels client.StreamEvents
// returned, the cancel that ends the request behind them, and the generation
// they belong to.
type sseStream struct {
	events <-chan client.SSEEvent
	errs   <-chan error
	cancel context.CancelFunc
	gen    uint64
}

// The SSE messages, each carrying the generation of the stream that produced
// it.
//
// GENERATIONS ARE THE WHOLE CONCURRENCY STORY. A dropped stream is replaced
// while its Cmds may still be in flight: without the guard, a late error from
// the connection we already gave up on would degrade the fresh one, a late
// event would report it healthy, and a backoff timer from an abandoned attempt
// would open a second stream nobody re-arms a reader for. Every handler
// therefore compares against model.sseGen first and drops anything older.
type (
	// sseOpenMsg hands a newly subscribed stream back to the model.
	// Subscribing happens in a Cmd rather than in Update because it starts a
	// goroutine and creates a cancel func, and Update stays pure.
	sseOpenMsg struct {
		gen    uint64
		stream *sseStream
	}

	// sseEventMsg is one completely framed event off the stream.
	sseEventMsg struct {
		gen   uint64
		event client.SSEEvent
	}

	// sseDroppedMsg is the stream ending, by error or by clean close.
	sseDroppedMsg struct {
		gen uint64
		err error
	}

	// sseReconnectMsg is the backoff timer firing.
	sseReconnectMsg struct {
		gen uint64
	}
)

// connectSSE subscribes to the dashboard's event stream.
//
// The context is deliberately not derived from anything request-scoped: this
// stream lives until it drops or the program exits. Its cancel travels on the
// returned stream so the model can end the request on a drop, on a reconnect,
// and on quit.
func (m model) connectSSE() tea.Cmd {
	api, gen := m.api, m.sseGen
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		events, errs := api.StreamEvents(ctx)
		return sseOpenMsg{gen: gen, stream: &sseStream{
			events: events,
			errs:   errs,
			cancel: cancel,
			gen:    gen,
		}}
	}
}

// waitSSE is ONE receive from the stream; Update re-arms it for the next.
//
// That is the bubbletea channel pump: a Cmd runs on its own goroutine and ends
// by producing a message, so a long-lived channel is drained one Cmd per value
// rather than by a loop, which would have no way to deliver what it read.
func waitSSE(s *sseStream) tea.Cmd {
	return func() tea.Msg {
		select {
		case event, ok := <-s.events:
			if !ok {
				return sseDroppedMsg{gen: s.gen, err: s.terminalErr()}
			}
			return sseEventMsg{gen: s.gen, event: event}
		case err, ok := <-s.errs:
			if !ok {
				return sseDroppedMsg{gen: s.gen, err: errSSEClosed}
			}
			return sseDroppedMsg{gen: s.gen, err: err}
		}
	}
}

// terminalErr takes the stream's failure without blocking, falling back to
// errSSEClosed when it simply ended.
//
// It exists because the select above can observe either channel first. The
// producer buffers its error and only then closes both (errs first, events
// second, its defers running in reverse), so a closed events channel means any
// error is already sitting in errs — and receiving from a closed buffered
// channel still yields what it holds. Reporting a bare close without looking
// would throw away the one description of what went wrong.
func (s *sseStream) terminalErr() error {
	select {
	case err, ok := <-s.errs:
		if ok && err != nil {
			return err
		}
	default:
	}
	return errSSEClosed
}

// cancelSSE ends the current stream's request, if there is one.
func (m model) cancelSSE() {
	if m.sse != nil {
		m.sse.cancel()
	}
}

// stopSSE ends the stream for good, on the way out of the program.
//
// Cancelling is what stops the goroutine StreamEvents owns: it is blocked on a
// read that nothing else can end, which is harmless for `hivectl tui` — the
// process is exiting — and is a leak per program under teatest, where the test
// binary outlives everything it drives.
//
// Retiring the generation is the other half, and it is not bookkeeping. The
// cancellation closes the stream's channels, so the pump produces one last
// drop; without the bump that drop is indistinguishable from a real one and
// the model would degrade the header and schedule a reconnect while the
// program is already quitting.
func (m model) stopSSE() model {
	m.cancelSSE()
	m.sse = nil
	m.sseGen++
	return m
}

func (m model) handleSSEOpen(msg sseOpenMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.sseGen {
		// A subscription abandoned before it reported back. Cancelling here is
		// what stops it holding a request open with nobody reading it.
		msg.stream.cancel()
		return m, nil
	}
	m.sse = msg.stream
	return m, waitSSE(msg.stream)
}

func (m model) handleSSEEvent(msg sseEventMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.sseGen || m.sse == nil {
		return m, nil
	}
	// A received event is the only proof the stream is up (see wsConnected),
	// so it is what resets the backoff and stretches the poll. No new tick
	// chain is armed for the stretch: the pending tick re-arms itself from
	// m.interval, so the new cadence takes effect within one pollInterval
	// without a second chain ever existing.
	m.sseConnected = true
	m.sseBackoff = 0
	m.interval = sseReconcileInterval

	cmds := []tea.Cmd{waitSSE(m.sse)}
	for _, paneMsg := range m.paneMsgs(msg.event) {
		var cmd tea.Cmd
		m, cmd = m.broadcast(paneMsg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) handleSSEDropped(msg sseDroppedMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.sseGen {
		return m, nil
	}
	m.cancelSSE()
	m.sse = nil
	m.sseConnected = false

	if m.sseBackoff == 0 {
		m.sseBackoff = sseBackoffMin
	} else {
		m.sseBackoff = min(2*m.sseBackoff, sseBackoffMax)
	}
	m.sseGen++
	gen, delay := m.sseGen, m.sseBackoff
	cmds := []tea.Cmd{tea.Tick(delay, func(time.Time) tea.Msg {
		return sseReconnectMsg{gen: gen}
	})}

	// Only the first drop after a healthy stream restores the poll, because it
	// is the only one with a stretched cadence to undo. Retiring the 60s chain
	// and arming a 5s one on EVERY failed reconnect as well would issue a
	// fetch per backoff step — which is how a dashboard that is down ends up
	// receiving more requests than one that is up.
	if m.interval != pollInterval {
		m.interval = pollInterval
		m.tickGen++
		// Fetch now as well as re-arming: the pending tick belonged to the 60s
		// chain this retires, so without an immediate poll the fallback's
		// first data would be a whole pollInterval away — spent showing a
		// frame the stream has already stopped updating.
		cmds = append(cmds, m.scheduleTick(), m.poll())
	}
	return m, tea.Batch(cmds...)
}

// sseStatusAgent is the subset of the dashboard's status agent entry that this
// frame renders. Fields are transcribed from dashboard.FrontendAgent
// (src/pkg/dashboard/server.go), which is what both the full status payload
// and the lighter `agent-status` payload carry under `agents`.
//
// It is deliberately narrow. The wire object has some sixty fields; decoding
// the four that decide a status glyph keeps this file from becoming a second,
// drifting copy of the dashboard's model — the same reason client.Agent
// documents itself as exactly the /api/agents contract and nothing more.
type sseStatusAgent struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Paused    bool   `json:"paused"`
	State     string `json:"state"`
	LastError string `json:"lastError,omitempty"`
}

// sseAgentStateRunning is the one agent.ProcessState value that means the
// process is up ("running"; the only other is "stopped").
const sseAgentStateRunning = "running"

// sseStatusPayload is the envelope both stream event types share. The full
// status payload has a great deal more in it, and the JSON decoder discards
// every key not named here without retaining it.
type sseStatusPayload struct {
	Timestamp string           `json:"timestamp"`
	Agents    []sseStatusAgent `json:"agents"`
}

// paneMsgs translates one stream event into the pane messages it supports.
//
// The panes are not taught about SSE: an event becomes the SAME message type
// the poll delivers, so a pane cannot tell (and never has to handle) where its
// snapshot came from. That is what makes the stream a source rather than a
// second rendering path.
//
// Two event types arrive on this stream (see client/sse.go):
//
//   - `agent-status`, the dashboard's fast agent-only push, carries agents
//     alone — so it produces an agents delivery and nothing else.
//   - the default message event, the full status snapshot, carries the
//     governor slice as well; client.GovernorStatus decodes directly from that
//     document, which is why the governor delivery costs one more Decode of
//     the same bytes rather than a second endpoint.
//
// WHAT IS DELIBERATELY NOT FILLED IN. GovernorMsg.EvalInterval is left zero:
// it is configuration from /api/config/governor, not live state, and this task
// adds no fetches. The pane already renders an unknown interval as a dash, so
// the result is an honest partial frame instead of no governor frame at all —
// nothing else feeds that pane today. AgentState.LastActivity is left zero for
// the same kind of reason: the payload's per-agent timestamp (`lastKick`) is a
// pre-formatted server-local display string, so a real instant cannot be
// recovered from it, and panes.Agents renders zero as "—".
func (m model) paneMsgs(event client.SSEEvent) []tea.Msg {
	var payload sseStatusPayload
	if err := event.Decode(&payload); err != nil {
		// A frame we cannot read is dropped exactly as a failed fetch is: the
		// panes keep what they were last told rather than being handed a zero
		// value they could not tell from an empty hive.
		return nil
	}

	var msgs []tea.Msg
	// The roster comes from the poll, the states from the stream. Before the
	// first poll returns there is no roster to join onto, and sending the
	// states alone would blank the pane — so this waits, which costs at most
	// the one in-flight fetch Init issued.
	if states := sseAgentStates(payload.Agents); states != nil && len(m.agents) > 0 {
		msgs = append(msgs, panes.AgentsMsg{
			Agents:     m.agents,
			States:     states,
			ObservedAt: sseObservedAt(payload.Timestamp),
		})
	}

	if event.Type != client.SSEEventTypeAgentStatus {
		var status client.GovernorStatus
		// Active is the payload's own "this document has a governor section"
		// bit — buildGovernor hardcodes it true — so this both skips a
		// governor-less snapshot and avoids overwriting a good frame with an
		// all-dashes one.
		if err := event.Decode(&status); err == nil && status.Active {
			msgs = append(msgs, panes.GovernorMsg{Status: status})
		}
	}
	return msgs
}

// sseAgentStates keys the live states by agent name, the key panes.AgentsMsg
// joins on. It returns nil for a payload with nothing usable, which is the
// signal to leave the pane's existing states alone rather than clear them.
func sseAgentStates(agents []sseStatusAgent) map[string]panes.AgentState {
	if len(agents) == 0 {
		return nil
	}
	states := make(map[string]panes.AgentState, len(agents))
	for _, agent := range agents {
		if agent.Name == "" {
			continue
		}
		states[agent.Name] = panes.AgentState{Status: agent.status()}
	}
	if len(states) == 0 {
		return nil
	}
	return states
}

// status maps the wire's several state fields onto the pane's three.
//
// The order is the priority order an operator reads them in: a recorded error
// is the thing to say about an agent even while it is nominally running, and a
// paused agent is paused whatever its process is doing. A stopped-but-not
// -errored agent lands on paused, which is also what the poll-only path shows
// for a disabled one (panes.Agents.agentLine) — the pane has no fourth status
// and adding one is a pane change, which this task does not make.
func (a sseStatusAgent) status() panes.AgentStatus {
	switch {
	case a.LastError != "":
		return panes.AgentStatusError
	case a.Paused || !a.Enabled:
		return panes.AgentStatusPaused
	case a.State == sseAgentStateRunning:
		return panes.AgentStatusRunning
	default:
		return panes.AgentStatusPaused
	}
}

// sseObservedAt reads the payload's own publish time, which is RFC 3339 (
// BuildFrontendStatus formats it with time.RFC3339). Using the server's
// timestamp rather than the receiving clock is what keeps the relative
// activity labels honest about the snapshot they describe; an unreadable one
// returns zero, which panes.Agents substitutes its own clock for.
func sseObservedAt(timestamp string) time.Time {
	observed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return time.Time{}
	}
	return observed
}

// Run starts the TUI on this process's own terminal and blocks until the
// operator quits. It returns whatever error bubbletea reports, including the
// failure to open a terminal when the program is run without a TTY.
func Run() error {
	return run(os.Stdin, os.Stdout)
}

// run is Run with its terminal injected.
//
// The split exists so tests can drive the REAL program — the same
// tea.NewProgram call with the same options — over pipes instead of a TTY.
// That matters beyond coverage: teatest builds its own program internally, so a
// teatest-only suite never executes this constructor and would not notice
// WithAltScreen being dropped. Alt-screen is not cosmetic — it is what restores
// the operator's scrollback on exit, so `hivectl tui` leaves the terminal the
// way it found it.
func run(in io.Reader, out io.Writer) error {
	_, err := tea.NewProgram(
		newModel(),
		tea.WithAltScreen(),
		tea.WithInput(in),
		tea.WithOutput(out),
	).Run()
	return err
}
