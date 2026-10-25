package crux

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// SpanStarter opens a trace span for a runtime operation. The host installs
// its tracer through Host.SetSpanStarter; nil means the OpenTelemetry global.
type SpanStarter func(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span)

// Host is how an in-process runtime receives the capabilities and callbacks
// Hive provides: credential minters, the audit sink, observers for kick
// outcomes and pause transitions, resolvers the launch path consults. It is
// deliberately NOT part of Runtime. A sidecar runtime cannot be handed a Go
// closure; stage 2 of hivecommons/hive#14138 records how each of these
// becomes a hive-side callback endpoint (the minters, the resolvers) or an
// API event (the observers), and until then Host is only satisfied by the
// in-process implementation (crux/runtime.HostOf reports which).
//
// Every method here is called by cmd/hive during boot or on a configuration
// reload, after construction; none is on the dashboard's path. Methods are
// grouped by what the sidecar will replace them with.
type Host interface {
	// Credential capabilities -> hive-side callback endpoint (stage 2).
	SetAppAuth(auth AppTokenMinter)
	SetAgentMint(issuer AgentMintIssuer)
	SetLinearCredentialResolver(fn func() LinearCredential)
	SetBobAPIKeyResolver(fn func() string)
	SetBobKeySourceResolver(fn func() string)
	SetSandboxPushMinter(minter pushbroker.TokenMinter)
	SetSandboxPRClient(client PRCreator)

	// Launch-time resolvers -> definition fields or a callback endpoint.
	SetBuildInfo(fn func() (version, commit string))
	SetGatewayBackendChecker(fn func(backend string) bool)
	SetExplainModeDefaultResolver(fn func() string)
	SetRepoAutoMergeEnabledResolver(fn func(repo string) bool)
	SetKickLogSourceResolver(fn func() string)
	SetInferenceCallbacks(setRoute func(agentName, backend, model string), clearRoute func(agentName string))
	SetSandboxConfig(cfg config.AgentSandboxConfig)
	SetSandboxMutationBoundary(boundary effects.Boundary)
	SetDeadSessionRecoveryOwner(ownedElsewhere bool)

	// Observers -> /v1/events (stage 3).
	SetKickOutcomeObserver(fn func(agentName string, outcome KickOutcome))
	SetPauseTransitionObserver(fn func(PauseTransitionEvent))
	SetRelaunchObserver(fn func(agentName string))
	SetPersistPauseCallback(fn func(name string, paused bool))
	SetRecordPromptCallback(fn func(agent, trigger, prompt string))
	SetSandboxAuditCallback(fn func(agent, action, detail string))
	SetAuditSink(sink AuditSink)
	SetConditionReporter(r ConditionReporter)
	SetSpanStarter(fn SpanStarter)
}
