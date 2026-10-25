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
	"time"

	"github.com/hivecommons/hive/crux/trajectory"

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

// Resumer is backend-native session resume: naming the conversation that
// authored a PR and typing a follow-up into it later (hivecommons/hive#9606).
// Errors are the crux.ErrResume* sentinels.
type Resumer interface {
	// ResumeHandle captures the agent's current resumable conversation.
	// cmd/hive: prfollowupwire.go (PR-opened hook).
	ResumeHandle(name string) (ResumeHandle, bool)
	// SendResumeKick types message into the session sessionID names, or
	// returns ErrResumeBusy / ErrResumeSessionGone / ErrResumeUnsupported.
	// cmd/hive: prfollowupwire.go.
	SendResumeKick(name, message, sessionID string) error
	// SessionID is the agent's live session id, if it has one. cmd/hive:
	// prfollowupwire.go.
	SessionID(name string) (string, bool)
	// SessionMissing reports an agent whose session is gone (dead pane).
	// cmd/hive: eval cycle resume-kick hold.
	SessionMissing(name string) bool
}

// Supervisor is what the governor asks the runtime while scheduling work:
// may it start an agent, what is each agent launched as, which agents have
// crashed, which credentials can be refreshed. These reads and actions sit
// between Lifecycle and the host's scheduler.
type Supervisor interface {
	// GovernorMayStart reports whether the runtime will accept a Start for
	// name right now (not blocked by a start-failure hold). cmd/hive.
	GovernorMayStart(name string) bool
	// MarkStartupLaunchQueued records that the host has queued the named
	// agents for its staggered boot launch. cmd/hive: boot.
	MarkStartupLaunchQueued(names []string)
	// CheckAndRestartCrashedAgents relaunches crashed agents and returns
	// their names. cmd/hive: run loop.
	CheckAndRestartCrashedAgents(ctx context.Context) []string
	// ConsentWedgedAgents lists agents stuck on a consent prompt. cmd/hive.
	ConsentWedgedAgents() []string
	// RefreshAgentTokens rewrites every agent's scoped GitHub token.
	// cmd/hive: watchers, GitHub client refresh.
	RefreshAgentTokens(ctx context.Context)
	// AgentCredentialRefreshable reports whether the host can refresh the
	// agent's credential. cmd/hive: heartbeat.
	AgentCredentialRefreshable(agentName string) (refreshable, known bool)
	// AgentCapabilities is the agent's GitHub capability bits. cmd/hive.
	AgentCapabilities(agentName string) (canOpenIssue, canOpenPR, canMerge, ok bool)
	// EffectiveBackend is the backend the agent runs on after overrides.
	// cmd/hive: rotation.
	EffectiveBackend(agentName string) (backend string, ok bool)
	// InvocationMetadata is what the agent was launched as. cmd/hive:
	// attribution trailers.
	InvocationMetadata(agentName string) (backend, model, effort string, ok bool)
	// RestartTelemetry is the agent's restart counters. cmd/hive: heartbeat.
	RestartTelemetry(name string) (total, last24h int, lastRestartAt time.Time, lastReason string, ok bool)
	// StartFailureState is the agent's start-failure record. cmd/hive.
	StartFailureState(name string) (StartFailureSnapshot, bool)
	// ProviderErrorBackoffRemaining reports an active provider-error backoff:
	// remaining time, provider, reason. cmd/hive: eval cycle.
	ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool)
	// PauseByCause is PauseBy with the machine-readable causation recorded.
	// cmd/hive: fleet breaker, escalations.
	PauseByCause(name, trigger, reason, by string, cause PauseCausation) error
	// ArchiveAllKickLogs rotates every agent's kick log. cmd/hive: mode
	// change.
	ArchiveAllKickLogs(reason string)
	// CountAgentsWithModel reports how many agents launched with an explicit
	// model, for the hourly spoke metrics. cmd/hive.
	CountAgentsWithModel() int
	// CrashOOMSuspected reports whether the agent's last crash looks like an
	// OOM kill, so a held resume kick is explained correctly. cmd/hive.
	CrashOOMSuspected(name string) bool
	// BackendAuthAvailable and AgentAuthAvailable report whether credentials
	// for a backend / an agent are present; known is false when the runtime
	// cannot tell. The dashboard's auth badges read them. cmd/hive.
	BackendAuthAvailable(backend string) (available, known bool)
	AgentAuthAvailable(agentName string) (available, known bool)
	// SendGitHubAPIBudgetNudge tells an agent its GitHub API budget is
	// nearly spent; a nudge, not a kick, so it never counts against the kick
	// limits. cmd/hive: egress proxy.
	SendGitHubAPIBudgetNudge(name, message string)
	// RecordAudit appends a host-side action against an agent to the
	// runtime's audit trail. cmd/hive.
	RecordAudit(action, agentName string, fields map[string]any)
	// AgentHasValidCredential and RefreshAgentTokenFor back the login-prompt
	// scanner: an agent sitting on a login prompt with a valid credential is
	// re-minted rather than paused. cmd/hive.
	AgentHasValidCredential(agentName string) bool
	RefreshAgentTokenFor(ctx context.Context, name string) error
	// TrajectoryAgents snapshots the running agents with intent and
	// transcript for the trajectory lane. cmd/hive.
	TrajectoryAgents() []trajectory.AgentView
}

// Restorer seeds the runtime with state the host persisted across a restart
// (pkg/snapshot). Each call replaces the record for one agent and is only
// meaningful before that agent runs.
type Restorer interface {
	SeedPauseState(name string, pausedAt time.Time, trigger, reason, by string)
	SeedRestartTelemetry(name string, count int, events []RestartEvent, lastReason string)
	SeedTurnLoss(name string, loss TurnLoss)
	SeedLastKick(name string, t time.Time)
	SeedKickHistory(name string, records []KickRecord)
	// RestoreBreaker re-engages the fleet breaker as it was persisted.
	RestoreBreaker(engaged bool, paused []string)
}

// Background are the runtime's long-running maintenance loops. Each blocks
// until ctx ends; the host runs them as goroutines after boot. In a sidecar
// they run inside the serving process and this interface is a no-op.
type Background interface {
	StartAgentTokenRefresh(ctx context.Context)
	StartCredentialWatchdog(ctx context.Context)
	StartCopilotSessionRefresh(ctx context.Context)
	StartTranscriptActivityRefresh(ctx context.Context)
}

// Authorizer is the per-request gate the host's relay watchers consult
// before acting on an agent's request (open a PR, open an issue, request a
// review, merge, push a branch): is the request from the agent it claims
// (fileUID), and does that agent's ACMM level permit the action. The five
// are method values handed to the relays at boot, so they are the one
// family a sidecar must answer synchronously on the request path.
type Authorizer interface {
	AuthorizePROpen(agentName string, fileUID int) error
	AuthorizePushBranch(agentName string, fileUID int) error
	AuthorizeIssueOpen(agentName string, fileUID int, kind string) error
	AuthorizeMerge(agentName string, fileUID int, repo string) error
	AuthorizeReviewRequest(agentName string, fileUID int) error
}

// Runtime is the full surface a consumer needs to run, observe and control
// agents. *agent.Manager implements it directly (asserted in crux/agent).
// Consumers that need less should depend on the narrower interfaces above.
//
// Resumer, Supervisor, Restorer, Background and Authorizer are the families cmd/hive
// uses beyond the dashboard's six (hivecommons/hive#14138, stage 1); a
// sidecar runtime serves each over the API before the default flips.
type Runtime interface {
	Lifecycle
	Observer
	Kicker
	Definitions
	Fleet
	Credentials
	Resumer
	Supervisor
	Restorer
	Background
	Authorizer
}
