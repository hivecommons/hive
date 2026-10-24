// Package crux is the exported surface of Hive's agent runtime
// (hivecommons/hive#13536, src/docs/design/crux.md).
//
// crux/agent is the runtime's implementation: the manager, kick files, engine
// adapters and the tmux funnel. Consumers outside that tree (the dashboard,
// the hub, cmd/hive) are meant to program against the types in this package
// rather than against *agent.Manager, so that they do not follow the
// runtime's internals. The architecture ratchet in
// internal/testutil/crux_boundary_ratchet_test.go counts the agent.<Symbol>
// references each consumer still makes; this package is how that number goes
// down.
//
// This package does not import crux/agent: crux/agent imports it. Process is
// its own type, a snapshot the manager copies out of its internal agent
// record, and ProjectContext and RingBuffer are defined here and aliased by
// crux/agent, so *agent.Manager implements Runtime directly (agent.AsRuntime
// adapts a possibly-nil manager). Every other value type on this surface
// (process states, kick dispatch and outcome, kick logs, restart events,
// sub-agent models, start-failure classes, turn loss, the provider-status
// vocabulary, watchdog conditions) is defined here too and aliased by
// crux/agent and crux/watchdog. Each interface method is annotated with the
// consumer that needs it today; a method with no consumer does not belong
// here.
package crux

import (
	"context"

	"github.com/hivecommons/hive/crux/config"
)

// FatalNetworkErrorPatterns returns the pane substrings the runtime treats
// as a fatal startup network failure and restarts the agent on. Hosts that
// compose prompts echoed into the pane assert their text never contains one
// (pkg/review's prompt builders; #7553). The slice is a copy.
func FatalNetworkErrorPatterns() []string {
	return append([]string(nil), fatalNetworkErrorPatterns...)
}

// fatalNetworkErrorPatterns are substrings that indicate a transient TLS or
// network failure killed the agent at startup. These errors leave the Copilot
// chrome visible (❯, / commands) so the pane looks ready, but the agent is
// dead and will never recover without a restart. crux/agent's
// FatalNetworkErrorPatterns is initialised from this list.
var fatalNetworkErrorPatterns = []string{
	"invalid peer certificate",
	"BadSignature",
	"fetch failed",
}

// Lifecycle starts, stops, pauses and restarts agents.
type Lifecycle interface {
	// Start launches a configured agent. Dashboard: agent config and pack
	// handlers (api_agent_config.go, api_agents.go, api_packs.go).
	Start(ctx context.Context, name string) error
	// Stop ends an agent's process and marks it operator-stopped. Dashboard:
	// api_agent_config.go, api_packs.go.
	Stop(name string) error
	// Restart stops and relaunches an agent with its current overrides.
	// Dashboard: api_agent_config.go, api_agent_control.go.
	Restart(ctx context.Context, name string) error
	// RestartWithBootstrap relaunches an agent with a one-off bootstrap
	// prompt. Dashboard: inception watcher.
	RestartWithBootstrap(ctx context.Context, name, prompt string) error
	// Pause quiesces an agent with a machine trigger and a human reason.
	// Dashboard: api_packs.go (ACMM pack level), inception_handlers.go.
	Pause(name, trigger, reason string) error
	// PauseBy is Pause with the acting user recorded. Dashboard:
	// api_agent_control.go.
	PauseBy(name, trigger, reason, by string) error
	// Resume lifts a pause. Dashboard: api_agent_control.go, api_packs.go,
	// inception_handlers.go.
	Resume(ctx context.Context, name, trigger, reason string) error
	// IsPaused reports whether an agent is paused. Dashboard: api_packs.go.
	IsPaused(name string) bool
	// ResetRestartCount clears the crash-loop counter after an operator
	// restart. Dashboard: api_agent_control.go.
	ResetRestartCount(name string) error
}

// Observer reads agent state and output without changing it.
type Observer interface {
	// GetStatus returns a fresh snapshot of one agent, including pane-derived
	// observations. Dashboard: status builder and most agent handlers.
	GetStatus(name string) (*Process, error)
	// GetStatusFast returns the snapshot without re-reading the pane.
	// Dashboard: api_agent_config.go.
	GetStatusFast(name string) (*Process, error)
	// AllStatuses snapshots every agent. Dashboard: server.go health and
	// fleet views, api_watchdog_activity.go, contribute_leaderboard.go.
	AllStatuses() map[string]*Process
	// ResolveAgent maps a name or ID to the canonical agent name. Dashboard:
	// api.go (resolveAgentParam).
	ResolveAgent(nameOrID string) string
	// GetOutput returns the last lines of an agent's live pane. Dashboard:
	// api.go output handler.
	GetOutput(name string, lines int) ([]string, error)
	// GetBufferOutput returns the last lines of the agent's ring buffer.
	// Dashboard: api.go output handler, inception watcher.
	GetBufferOutput(name string, lines int) ([]string, error)
	// CaptureFullLog returns the whole scrollback of an agent's pane.
	// Dashboard: terminal_log.go, terminal_urls.go.
	CaptureFullLog(name string) (string, error)
	// FullLogHeader is the provenance header CaptureFullLog output is served
	// with. Dashboard: terminal_log.go.
	FullLogHeader(name string) string
}

// Kicker delivers prompts to running agents and exposes kick records.
type Kicker interface {
	// SendKick types a message into the agent's pane synchronously.
	// Dashboard: api_linear_agent.go, inception_handlers.go, swarm.go.
	SendKick(name string, message string) error
	// SendKickAsync queues a kick and returns whether delivery started; the
	// result is observed through KickDispatchState. Dashboard:
	// api_agent_control.go.
	SendKickAsync(name string, message string) (started bool, err error)
	// ValidateKick reports why a kick to name would be refused without
	// sending one. Dashboard: api_agent_control.go.
	ValidateKick(name string) error
	// KickDispatchState returns the latest asynchronous dispatch for name.
	// Dashboard: api_agent_control.go.
	KickDispatchState(name string) (KickDispatch, bool)
	// ListKickLogs lists archived kick logs for name. Dashboard: kick_logs.go.
	ListKickLogs(name string) ([]KickLogInfo, error)
	// ReadKickLog returns one archived kick log. Dashboard: kick_logs.go.
	ReadKickLog(name, id string) (string, error)
	// SetKickObserver registers a callback for kick lifecycle events.
	// Dashboard: api_linear_agent.go (Linear kick relay).
	SetKickObserver(fn func(agentName, event, detail string))
}

// Definitions changes which agents the runtime manages and how each is
// launched: configuration, model and backend overrides, CLI pins.
type Definitions interface {
	// AddAgent registers a new agent definition. Dashboard: api_governor.go,
	// api_packs.go.
	AddAgent(name string, cfg config.AgentConfig)
	// RemoveAgent forgets an agent definition. Dashboard: api_governor.go,
	// api_packs.go.
	RemoveAgent(name string)
	// UpdateConfig replaces an agent's configuration in place. Dashboard:
	// api_agent_config.go, api_agent_control.go, api_packs.go.
	UpdateConfig(name string, cfg config.AgentConfig) error
	// ReconcileAgents adds definitions present in configs and unknown to the
	// runtime, returning the names added. Dashboard: api_agent_config.go,
	// api_agents.go.
	ReconcileAgents(configs map[string]config.AgentConfig) []string
	// SetModelOverride / SetBackendOverride set per-agent launch overrides.
	// Dashboard: api_agent_config.go, api_agent_control.go.
	SetModelOverride(name, model string) error
	SetBackendOverride(name, backend string) error
	// PinCLI / PinModel / UnpinCLI / UnpinModel manage operator pins.
	// Dashboard: api_agent_control.go.
	PinCLI(name, version string) error
	PinModel(name, model string) error
	UnpinCLI(name string) error
	UnpinModel(name string) error
	// ClearModeOverrides drops mode overrides for the named agents.
	// Dashboard: api_packs.go.
	ClearModeOverrides(names []string)
	// SyncModeFiles rewrites agent mode files for an ACMM level. Dashboard:
	// api_agent_config.go, api_packs.go.
	SyncModeFiles(level int)
	// RefreshInferenceRoutes re-resolves routes for a gateway backend.
	// Dashboard: api_governor.go.
	RefreshInferenceRoutes(backend string)
}

// Fleet is runtime-wide control: the pause breaker and the ACMM level.
type Fleet interface {
	// BreakerState reports whether the fleet breaker is engaged and which
	// agents it paused. Dashboard: api.go, api_breaker.go.
	BreakerState() (engaged bool, paused []string)
	// EngageBreaker pauses every running agent. Dashboard: api_breaker.go.
	EngageBreaker() (paused []string)
	// ReleaseBreaker resumes the agents the breaker paused. Dashboard:
	// api_breaker.go.
	ReleaseBreaker(ctx context.Context) (resumed []string)
	// SetACMMLevel / GetACMMLevel manage the fleet's ACMM pack level.
	// Dashboard: api_packs.go, api_agent_config.go.
	SetACMMLevel(level int)
	GetACMMLevel() int
	// RelaunchBobAgentsAwaitingKey relaunches bob agents parked on a missing
	// API key. Dashboard: api_governor.go.
	RelaunchBobAgentsAwaitingKey(ctx context.Context) []string
	// SharedDevUID reports whether agents share the operator's uid.
	// Dashboard: health_uid_isolation.go.
	SharedDevUID() bool
}

// Credentials is the runtime's credential plumbing: the shared Copilot
// token, the Claude credential store, interactive login codes.
type Credentials interface {
	// SubmitLoginCode types a device-flow code into an agent waiting on a
	// login prompt. Dashboard: login_code.go.
	SubmitLoginCode(name, code string) error
	// CopilotToken / CopilotTokenSource / SetCopilotToken /
	// ActivateCopilotToken manage the shared Copilot CLI identity.
	// Dashboard: cli_models.go, copilot_auth.go.
	CopilotToken() string
	CopilotTokenSource() string
	SetCopilotToken(token string)
	ActivateCopilotToken(token string) error
	// ReloadClaudeToken re-reads the Claude credential store. Dashboard:
	// claude_auth.go.
	ReloadClaudeToken()
	// ClaudeCredentialCandidatePaths lists where the runtime looks for Claude
	// credentials. Dashboard: cli_models.go.
	ClaudeCredentialCandidatePaths() []string
}

// Runtime is the full surface a consumer needs to run, observe and control
// agents. *agent.Manager implements it directly (asserted in crux/agent).
// Consumers that need less should depend on the narrower interfaces above.
type Runtime interface {
	Lifecycle
	Observer
	Kicker
	Definitions
	Fleet
	Credentials
}
