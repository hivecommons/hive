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
// The watchdog fleet family (hivecommons/hive#14138, stage 3) serves the
// watchdog.Fleet the self-healing reconciler runs against, so a host can
// observe agents and publish or clear their conditions across the process
// boundary:
//
//	GET    /v1/watchdog                          Fleet.AgentNames
//	GET    /v1/agents/{name}/watchdog            Fleet.Observe / IsPaused / LastProduction
//	PUT    /v1/agents/{name}/watchdog/conditions Fleet.SetConditions
//	DELETE /v1/agents/{name}/watchdog/conditions Fleet.SetConditions (empty set)
//
// {name} is a name or ID; the server resolves it with Observer.ResolveAgent.
// Routes is the same table as data; the parity tests hold it to the server,
// the client, crux.Runtime and the CLI's verb table.
package api

import (
	"encoding/json"
	"net/http"
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
	// PathWatchdog lists the agents the watchdog fleet reconciles.
	PathWatchdog = "/" + Version + "/watchdog"
)

// AgentPath is the route of one agent.
func AgentPath(name string) string { return PathAgents + "/" + name }

// LogsPath is the route of one agent's logs.
func LogsPath(name string) string { return AgentPath(name) + "/logs" }

// KickPath is the route that kicks one agent.
func KickPath(name string) string { return AgentPath(name) + "/kick" }

// StopPath is the route that stops one agent.
func StopPath(name string) string { return AgentPath(name) + "/stop" }

// WatchdogPath is the route that observes one agent for the watchdog.
func WatchdogPath(name string) string { return AgentPath(name) + "/watchdog" }

// WatchdogConditionsPath is the route that sets (PUT) or clears (DELETE) one
// agent's watchdog conditions.
func WatchdogConditionsPath(name string) string { return WatchdogPath(name) + "/conditions" }

// Route is one endpoint of the API as data: the HTTP method and ServeMux
// path pattern, the `crux` verb it serves (empty when no verb maps onto
// it), the *Client methods that call it, and the runtime methods the server
// answers it with. Runtime names crux.Runtime methods; Fleet names
// watchdog.Fleet methods, the watchdog family that is not on crux.Runtime.
type Route struct {
	Method  string
	Pattern string
	Verb    string
	Client  []string
	Runtime []string
	Fleet   []string
}

// Routes is every endpoint the server registers. It is the API's half of
// the parity contract in hivecommons/hive#14138: crux/serve asserts each is
// registered and that every crux.Runtime and watchdog.Fleet method is routed
// here or named in its unrouted allowlist; crux/cli asserts every runtime
// verb has a route. A new endpoint family adds its rows here and removes its
// methods from those allowlists.
var Routes = []Route{
	{Method: http.MethodGet, Pattern: PathHealth, Client: []string{"Health", "WaitReady"}, Runtime: []string{"AllStatuses"}},
	{Method: http.MethodGet, Pattern: PathAgents, Verb: "ps", Client: []string{"List"}, Runtime: []string{"AllStatuses"}},
	{Method: http.MethodPost, Pattern: PathAgents, Verb: "run", Client: []string{"Run"}, Runtime: []string{"ResolveAgent", "GetStatus", "AddAgent", "Start"}},
	{Method: http.MethodGet, Pattern: AgentPath("{name}"), Client: []string{"Get"}, Runtime: []string{"ResolveAgent", "GetStatus"}},
	{Method: http.MethodGet, Pattern: LogsPath("{name}"), Verb: "logs", Client: []string{"Logs", "FollowLogs"}, Runtime: []string{"ResolveAgent", "GetOutput", "GetBufferOutput", "CaptureFullLog", "FullLogHeader"}},
	{Method: http.MethodPost, Pattern: KickPath("{name}"), Verb: "kick", Client: []string{"Kick"}, Runtime: []string{"ResolveAgent", "SendKick", "GetStatus"}},
	{Method: http.MethodPost, Pattern: StopPath("{name}"), Verb: "stop", Client: []string{"Stop"}, Runtime: []string{"ResolveAgent", "Stop", "GetStatus"}},
	{Method: http.MethodGet, Pattern: PathEvents, Client: []string{"Events"}, Runtime: []string{"AllStatuses", "SetKickObserver"}},
	{Method: http.MethodGet, Pattern: PathWatchdog, Client: []string{"WatchdogAgents"}, Fleet: []string{"AgentNames"}},
	{Method: http.MethodGet, Pattern: WatchdogPath("{name}"), Client: []string{"WatchdogObserve"}, Runtime: []string{"ResolveAgent"}, Fleet: []string{"Observe", "IsPaused", "LastProduction"}},
	{Method: http.MethodPut, Pattern: WatchdogConditionsPath("{name}"), Client: []string{"SetWatchdogConditions"}, Runtime: []string{"ResolveAgent", "GetStatus"}, Fleet: []string{"SetConditions"}},
	{Method: http.MethodDelete, Pattern: WatchdogConditionsPath("{name}"), Client: []string{"ClearWatchdogConditions"}, Runtime: []string{"ResolveAgent", "GetStatus"}, Fleet: []string{"SetConditions"}},
}

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

// WatchdogFleet is the body of GET /v1/watchdog: the agents the watchdog
// reconciles (those the runtime believes are running), sorted by name.
type WatchdogFleet struct {
	Agents []string `json:"agents"`
}

// WatchdogObservation is the body of GET /v1/agents/{name}/watchdog: the
// wire form of watchdog.Observation plus the pause state and production
// evidence the reconciler reads alongside it. A nil time is one the runtime
// does not know (no launch timestamp, no pane change seen, no readable
// production evidence), never a zero value standing in for "healthy".
type WatchdogObservation struct {
	Agent            string `json:"agent"`
	Backend          string `json:"backend"`
	SessionExists    bool   `json:"session_exists"`
	Pane             string `json:"pane,omitempty"`
	HasCLIMarker     bool   `json:"has_cli_marker"`
	ShowsLoginPrompt bool   `json:"shows_login_prompt"`

	LastChange *time.Time `json:"last_change,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`

	AuthAvailable      bool   `json:"auth_available"`
	AuthKnown          bool   `json:"auth_known"`
	CredentialProven   bool   `json:"credential_proven"`
	ProviderErrorClass string `json:"provider_error_class,omitempty"`
	ProviderErrorLine  string `json:"provider_error_line,omitempty"`

	Paused         bool       `json:"paused"`
	LastProduction *time.Time `json:"last_production,omitempty"`
}

// WatchdogCondition is one observed-health axis, the wire form of
// crux.Condition (same JSON shape). Type is Ready, Authenticated or
// Producing; Status is True, False or Unknown.
type WatchdogCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// WatchdogConditions is the body of PUT /v1/agents/{name}/watchdog/conditions
// and the response of both the PUT and the DELETE: the agent's condition set
// after the call. Agent is ignored on the request; the path names the agent.
type WatchdogConditions struct {
	Agent      string              `json:"agent,omitempty"`
	Conditions []WatchdogCondition `json:"conditions"`
}

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
	// CodeNotConfigured is a route whose family the server was built
	// without (a watchdog route on a runtime that has no watchdog fleet).
	CodeNotConfigured = "not_configured"
)
