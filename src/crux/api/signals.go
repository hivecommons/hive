package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Turn signals (hivecommons/crux#114 S5): what a contained run, the
// gates around it, and its driver say at turn boundaries, kept with the
// run and readable after a missed event.
//
//	GET  /v1/runs/{id}/signals?since_seq=N&limit=M   signals after seq N,
//	                                                 oldest first, at most M
//	POST /v1/runs/{id}/signals                       the owner emits one
//	GET  /v1/agents/{name}/kick-outcome              the agent's saved
//	                                                 KickOutcome (operator)
//
// Both run routes admit the run's owner and the operator, like the run
// itself. Follow the log by passing the page's next_seq back as
// since_seq. Every durable signal is also published on /v1/events as
// EventRunSignal; the event is a notification, the record is the proof.

// RunSignalsPath is the route of one run's signal log.
func RunSignalsPath(id string) string { return RunPath(id) + "/signals" }

// RunSignalsQuery is RunSignalsPath with its paging parameters; zero
// values are omitted and take the server defaults.
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

// AgentKickOutcomePath is the route of one agent's saved kick outcome.
func AgentKickOutcomePath(name string) string {
	return PathAgents + "/" + url.PathEscape(name) + "/kick-outcome"
}

// Signal kinds: the closed set a record reports. A driver reads the kind
// as "where the turn is", never as a verdict on the work.
const (
	// SignalStarted: the runner handed the command to the sandbox.
	SignalStarted = "started"
	// SignalThinking: the agent is reasoning; no tool is running.
	SignalThinking = "thinking"
	// SignalTool: the agent is running a tool or command.
	SignalTool = "tool"
	// SignalBlocked: the turn cannot proceed and no person has been asked
	// (a gate expired, a limit was hit, a dependency is missing).
	SignalBlocked = "blocked"
	// SignalNeedsInput: the turn is waiting on a person (a gate opened).
	SignalNeedsInput = "needs-input"
	// SignalInput: the awaited input arrived (a gate was answered).
	SignalInput = "input"
	// SignalDone: the turn ended. Data carries the run's terminal status;
	// done is not success.
	SignalDone = "done"
)

// Signal sources: who wrote the record.
const (
	// SignalSourceRunner is the S2 runner's own lifecycle (started, done).
	SignalSourceRunner = "runner"
	// SignalSourceGate is the S3 gate service (needs-input, input, blocked).
	SignalSourceGate = "gate"
	// SignalSourceCaller is the run's owner through POST.
	SignalSourceCaller = "caller"
)

// CallerSignalKinds are the kinds POST /v1/runs/{id}/signals accepts;
// started and done are the runner's alone so a record cannot claim a
// turn ended that did not.
var CallerSignalKinds = []string{SignalThinking, SignalTool, SignalBlocked, SignalNeedsInput, SignalInput}

// EventRunSignal is the /v1/events type carrying a RunSignalEvent for every
// durable signal.
const EventRunSignal = "run.signal"

// SignalRequest is the body of POST /v1/runs/{id}/signals.
type SignalRequest struct {
	Kind string `json:"kind"`
	// Message is a short, human-readable note; it is scrubbed and bounded.
	Message string `json:"message,omitempty"`
	// Data is a driver-defined JSON payload, bounded once encoded.
	Data json.RawMessage `json:"data,omitempty"`
}

// Signal is one record of a run's signal log.
type Signal struct {
	Seq    int64     `json:"seq"`
	Kind   string    `json:"kind"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
	// By is the principal that emitted a caller signal; empty for the
	// runner and the gate service.
	By      string          `json:"by,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// SignalPage is GET /v1/runs/{id}/signals. NextSeq is the seq to pass as
// since_seq for the following page; More says one exists now.
type SignalPage struct {
	Run     string   `json:"run"`
	Signals []Signal `json:"signals"`
	NextSeq int64    `json:"next_seq"`
	More    bool     `json:"more,omitempty"`
}

// RunSignalEvent is the Data of an EventRunSignal event.
type RunSignalEvent struct {
	Run    string `json:"run"`
	Signal Signal `json:"signal"`
}

// AgentKickOutcome is GET /v1/agents/{name}/kick-outcome: the agent's last
// KickOutcome (live when the runtime has one, otherwise the saved record)
// and whether it settles the most recent kick. Outcome is absent when no
// turn of this agent has ever been classified.
type AgentKickOutcome struct {
	Agent    string       `json:"agent"`
	Outcome  *KickOutcome `json:"outcome,omitempty"`
	LastKick *time.Time   `json:"last_kick,omitempty"`
	// SettledForLastKick is KickOutcome.Settled(LastKick): the turn the
	// last kick started has ended and been classified.
	SettledForLastKick bool `json:"settled_for_last_kick"`
	// Saved is true when Outcome came from the persisted record rather
	// than the live runtime.
	Saved bool `json:"saved,omitempty"`
}

// RunSignals is GET /v1/runs/{id}/signals.
func (c *Client) RunSignals(ctx context.Context, id string, sinceSeq int64, limit int) (SignalPage, error) {
	var out SignalPage
	err := c.Do(ctx, http.MethodGet, RunSignalsQuery(id, sinceSeq, limit), nil, &out)
	return out, err
}

// EmitSignal is POST /v1/runs/{id}/signals.
func (c *Client) EmitSignal(ctx context.Context, id string, in SignalRequest) (Signal, error) {
	var out Signal
	err := c.Do(ctx, http.MethodPost, RunSignalsPath(id), in, &out)
	return out, err
}

// AgentKickOutcome is GET /v1/agents/{name}/kick-outcome.
func (c *Client) AgentKickOutcome(ctx context.Context, name string) (AgentKickOutcome, error) {
	var out AgentKickOutcome
	err := c.Do(ctx, http.MethodGet, AgentKickOutcomePath(name), nil, &out)
	return out, err
}
