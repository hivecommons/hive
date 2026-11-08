package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Turn signals (hivecommons/crux#114 S5): where a contained run's turn is
// and how it ended, kept with the run so a driver that missed the live
// event learns from the saved record.
//
//	GET  /v1/runs/{id}/signals?since_seq=N&limit=M   signals after seq N,
//	                                                  oldest first, at most M
//	POST /v1/runs/{id}/signals                       the owner reports where
//	                                                  the turn is
//	GET  /v1/agents/{name}/kick-outcome              the saved outcome of the
//	                                                  agent's last kicked turn
//
// The run routes admit the run's owner and the operator, like the run
// itself; the agent route is an operator route like every /v1/agents one.
// Every durable signal is also published as EventRunSignal.

// RunSignalsPath is the route of one run's signals.
func RunSignalsPath(id string) string { return RunPath(id) + "/signals" }

// RunSignalsQuery is RunSignalsPath with its paging parameters; zero values
// are omitted and take the server defaults.
func RunSignalsQuery(id string, sinceSeq int64, limit int) string {
	q := url.Values{}
	if sinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(sinceSeq, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := RunSignalsPath(id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
}

// AgentKickOutcomePath is the route of an agent's saved kick outcome.
func AgentKickOutcomePath(name string) string { return AgentPath(name) + "/kick-outcome" }

// Signal kinds, the closed set every signal reports. Started, done and step
// (SignalStep, steps.go) are the runner's; the rest may come from the run's owner, and needs-input,
// input and blocked also from a gate opened for the run.
const (
	// SignalStarted: the sandbox was launched.
	SignalStarted = "started"
	// SignalThinking: the turn is reasoning, no tool running.
	SignalThinking = "thinking"
	// SignalTool: the turn is running a tool.
	SignalTool = "tool"
	// SignalBlocked: the turn cannot continue (a gate expired with no
	// answer, or the owner says so).
	SignalBlocked = "blocked"
	// SignalNeedsInput: the turn waits on a person (a gate opened for it).
	SignalNeedsInput = "needs-input"
	// SignalInput: the person answered.
	SignalInput = "input"
	// SignalDone: the run reached a terminal status; Data is SignalDoneData.
	// It is always the run's last signal.
	SignalDone = "done"
)

// Signal sources: who appended the signal.
const (
	SignalSourceRunner = "runner"
	SignalSourceGate   = "gate"
	SignalSourceCaller = "caller"
)

// CodeSignalsClosed is a signal posted to a run whose done signal is
// already recorded (409).
const CodeSignalsClosed = "signals_closed"

// EventRunSignal carries a RunSignalEvent on /v1/events.
const EventRunSignal = "run.signal"

// RunSignal is one turn-boundary signal of a run. Message is scrubbed of
// credential-shaped strings; Data is a bounded, scrubbed JSON value whose
// shape depends on the kind (SignalDoneData for done, SignalGateData for a
// gate's signals, the caller's own payload otherwise).
type RunSignal struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Source  string          `json:"source"`
	At      time.Time       `json:"at"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// SignalDoneData is the data of a done signal: the run's terminal status
// and, when the sandbox reported one, its exit code.
type SignalDoneData struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// SignalGateData is the data of a signal a gate appended: the gate, and
// for input the recorded answer. The gate's context and note never ride
// along.
type SignalGateData struct {
	Gate       string `json:"gate"`
	Answer     string `json:"answer,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
}

// RunSignalRequest is the body of POST /v1/runs/{id}/signals. Kind is one
// of thinking, tool, blocked, needs-input or input.
type RunSignalRequest struct {
	Kind    string          `json:"kind"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RunSignalPage is GET /v1/runs/{id}/signals. NextSeq is the seq to pass
// as since_seq for the following page; More says one exists now. A caller
// following a live run keeps polling with NextSeq until it sees done.
type RunSignalPage struct {
	Run     string      `json:"run"`
	Signals []RunSignal `json:"signals"`
	NextSeq int64       `json:"next_seq"`
	More    bool        `json:"more,omitempty"`
}

// RunSignalEvent is the Data of EventRunSignal: the signal and its run.
type RunSignalEvent struct {
	Run string `json:"run"`
	RunSignal
}

// AgentKickOutcome is GET /v1/agents/{name}/kick-outcome. Outcome is the
// saved outcome of the agent's most recent kicked turn, absent when none
// was ever recorded. SettledForLastKick is crux.KickOutcome.Settled against
// LastKick, the runtime's delivery time of its latest kick: true only when
// the saved outcome belongs to that kick. LastKick is absent when the
// runtime holds no kick for the agent (it was never kicked, or crux serve
// restarted since); the outcome's kickAt then says which kick it describes.
type AgentKickOutcome struct {
	Agent              string       `json:"agent"`
	Outcome            *KickOutcome `json:"outcome,omitempty"`
	LastKick           *time.Time   `json:"last_kick,omitempty"`
	SettledForLastKick bool         `json:"settled_for_last_kick"`
}

// RunSignals is GET /v1/runs/{id}/signals.
func (c *Client) RunSignals(ctx context.Context, id string, sinceSeq int64, limit int) (RunSignalPage, error) {
	var out RunSignalPage
	err := c.do(ctx, http.MethodGet, RunSignalsQuery(id, sinceSeq, limit), nil, &out)
	return out, err
}

// EmitRunSignal is POST /v1/runs/{id}/signals.
func (c *Client) EmitRunSignal(ctx context.Context, id string, in RunSignalRequest) (RunSignal, error) {
	var out RunSignal
	err := c.do(ctx, http.MethodPost, RunSignalsPath(id), in, &out)
	return out, err
}

// AgentKickOutcome is GET /v1/agents/{name}/kick-outcome.
func (c *Client) AgentKickOutcome(ctx context.Context, name string) (AgentKickOutcome, error) {
	var out AgentKickOutcome
	err := c.do(ctx, http.MethodGet, AgentKickOutcomePath(name), nil, &out)
	return out, err
}
