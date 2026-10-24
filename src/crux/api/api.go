// Package api is the wire contract of `crux serve` (Crux Stage 3,
// hivecommons/hive#13536; src/docs/design/crux.md "Stage 3"): the request
// and response types, the routes, the event vocabulary and the address forms
// a client and server agree on. It imports only the standard library, so a
// harness that drives the runtime over the API depends on this package and
// nothing of the runtime itself.
//
// The API is HTTP+JSON under /v1. The server listens on a unix socket by
// default and optionally on TCP; TCP always requires a bearer token
// (Authorization: Bearer <token>), the socket requires one only when the
// server was given one. Streams (log follow, runtime events) are
// text/event-stream, one JSON object per `data:` line, `event:` naming the
// type.
//
// Each endpoint mirrors one `crux` CLI verb and the crux.Runtime methods that
// verb maps onto:
//
//	GET  /v1/health                        server is up; agent count
//	GET  /v1/agents                        ps        Observer.AllStatuses
//	POST /v1/agents                        run       Definitions.AddAgent + Lifecycle.Start
//	GET  /v1/agents/{name}                 get       Observer.GetStatus
//	GET  /v1/agents/{name}/logs            logs      Observer.GetOutput / GetBufferOutput / CaptureFullLog
//	GET  /v1/agents/{name}/logs?follow=1   logs -f   Observer.GetBufferOutput, streamed as SSE
//	POST /v1/agents/{name}/kick            kick      Kicker.SendKick
//	POST /v1/agents/{name}/stop            stop      Lifecycle.Stop
//	GET  /v1/events                        —         runtime events as SSE
//
// {name} is a name or ID; the server resolves it with Observer.ResolveAgent.
package api

import (
	"encoding/json"
	"time"
)

// Version is the API version every route is prefixed with.
const Version = "v1"

// Routes. Agent routes take the agent's name or ID in place of {name}; use
// AgentPath and friends to build them.
const (
	PathHealth = "/" + Version + "/health"
	PathAgents = "/" + Version + "/agents"
	PathEvents = "/" + Version + "/events"
)

// AgentPath is the route of one agent.
func AgentPath(name string) string { return PathAgents + "/" + name }

// LogsPath is the route of one agent's logs.
func LogsPath(name string) string { return AgentPath(name) + "/logs" }

// KickPath is the route that kicks one agent.
func KickPath(name string) string { return AgentPath(name) + "/kick" }

// StopPath is the route that stops one agent.
func StopPath(name string) string { return AgentPath(name) + "/stop" }

// Log sources, the `source` query parameter of the logs route. They are the
// three readers `crux logs` has: the live pane (default), the ring buffer
// (--buffer) and the whole scrollback (--full).
const (
	LogSourcePane   = "pane"
	LogSourceBuffer = "buffer"
	LogSourceFull   = "full"
)

// Query parameters of the logs route.
const (
	QuerySource = "source"
	QueryLines  = "n"
	QueryFollow = "follow"
)

// DefaultLogLines is how many lines the logs route returns when `n` is
// absent; it matches `crux logs`.
const DefaultLogLines = 100

// Media types.
const (
	ContentTypeJSON  = "application/json"
	ContentTypeEvent = "text/event-stream"
)

// Health is the body of GET /v1/health.
type Health struct {
	// Version is the API version the server speaks.
	Version string `json:"version"`
	// Agents is how many definitions the runtime holds.
	Agents int `json:"agents"`
}

// Process is the wire projection of one agent: what `crux ps --json` prints,
// plus the fields a harness needs to decide what to do next (kick refusals,
// pause provenance, the busy condition). It is a subset of crux.Process by
// design: the full snapshot carries engine-internal state that is neither
// stable nor meaningful off-process.
type Process struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	State   string `json:"state"`
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	PID     int    `json:"pid,omitempty"`

	Paused        bool   `json:"paused"`
	PausedReason  string `json:"paused_reason,omitempty"`
	PausedTrigger string `json:"paused_trigger,omitempty"`
	PausedBy      string `json:"paused_by,omitempty"`

	StartedAt    *time.Time `json:"started_at,omitempty"`
	LastKick     *time.Time `json:"last_kick,omitempty"`
	RestartCount int        `json:"restart_count"`

	LastRestartReason  string `json:"last_restart_reason,omitempty"`
	KickRefused        bool   `json:"kick_refused,omitempty"`
	KickRefusalReason  string `json:"kick_refusal_reason,omitempty"`
	KicksUndeliverable int    `json:"kicks_undeliverable,omitempty"`
	BusyCondition      string `json:"busy_condition,omitempty"`
	StartFailureClass  string `json:"start_failure_class,omitempty"`
}

// ProcessList is the body of GET /v1/agents, sorted by name.
type ProcessList struct {
	Processes []Process `json:"processes"`
}

// RunRequest is the body of POST /v1/agents. Name is required. Definition,
// when present, is an agent definition document in the runtime's own
// configuration shape (the JSON form of crux/config.AgentConfig, which is
// the per-agent overlay hive reads from agents_dir) and is registered with
// Definitions.AddAgent before Lifecycle.Start; Name must then match
// ^[a-z0-9][a-z0-9_-]*$ (at most 64 characters) and not already be defined
// on the server (409 CodeConflict otherwise: the runtime does not replace a
// definition). The server fills the defaults a start depends on (id and
// role from Name, replicas 1, enabled unless the document says otherwise);
// everything else is the caller's. Without a definition, Name (or an agent
// ID) must already be defined on the server. Parsing the portable
// AgentDefinition format is the caller's job, as it is the host's job for
// the CLI.
type RunRequest struct {
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition,omitempty"`
}

// KickRequest is the body of POST /v1/agents/{name}/kick.
type KickRequest struct {
	Message string `json:"message"`
}

// AgentResponse is the body of a successful run, kick or stop: the resolved
// agent name and its snapshot after the action.
type AgentResponse struct {
	Agent   string   `json:"agent"`
	Process *Process `json:"process,omitempty"`
}

// Logs is the body of GET /v1/agents/{name}/logs without follow. Lines is
// set for the pane and buffer sources; Header and Content for full.
type Logs struct {
	Agent   string   `json:"agent"`
	Source  string   `json:"source"`
	Header  string   `json:"header,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Content string   `json:"content,omitempty"`
}

// LogChunk is one `data:` record of a followed log stream: the lines
// appended to the ring buffer since the previous chunk.
type LogChunk struct {
	Agent string   `json:"agent"`
	Lines []string `json:"lines"`
}

// Event is one `data:` record of GET /v1/events. Type is one of the Event*
// constants; Agent is empty for runtime-wide events. State carries the new
// state for EventState; Detail is free text for the kick relay.
type Event struct {
	Time   time.Time `json:"time"`
	Type   string    `json:"type"`
	Agent  string    `json:"agent,omitempty"`
	State  string    `json:"state,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Event types. The lifecycle ones are emitted by the server for actions it
// performed; EventState is emitted when the server observes an agent's state
// change for any reason (crash, restart, pause); EventKick relays the
// runtime's kick observer (Kicker.SetKickObserver) with the observer's event
// name in Detail.
const (
	EventStarted = "agent.started"
	EventStopped = "agent.stopped"
	EventKicked  = "agent.kicked"
	EventState   = "agent.state"
	EventKick    = "agent.kick"
	// EventLog is the type of every record on a followed log stream.
	EventLog = "agent.log"
)

// Error is the body of every non-2xx response.
type Error struct {
	// Error is the message.
	Error string `json:"error"`
	// Code is a stable machine-readable kind; see the Code* constants.
	Code string `json:"code,omitempty"`
}

// Error codes.
const (
	CodeUnauthorized = "unauthorized"
	CodeBadRequest   = "bad_request"
	CodeNotFound     = "not_found"
	// CodeConflict is a run with a definition for a name the server already
	// holds; the runtime does not replace definitions.
	CodeConflict = "conflict"
	CodeRuntime  = "runtime_error"
)
