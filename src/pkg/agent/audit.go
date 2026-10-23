package agent

import (
	"github.com/hivecommons/hive/pkg/crux/types"
)

// The audit vocabulary this runtime emits — the lifecycle action names, the
// AuditSink interface and the Fields helper — is DEFINED in the stdlib-only
// leaf pkg/crux/types, so policy-layer packages can emit audit events without
// importing this runtime package and the runtime does not import the
// governor-side pkg/agentaudit (which aliases the same names for the
// recorder). The governor-side vocabulary (lease stages, commit trailers,
// artifact links, FormatAuditDetail) is agentaudit's alone and is referenced
// there directly (hivecommons/hive#13536).
const (
	AuditAgentStarted     = types.AuditAgentStarted
	AuditAgentStartFailed = types.AuditAgentStartFailed
	AuditAgentStopped     = types.AuditAgentStopped
	AuditAgentKicked      = types.AuditAgentKicked
	AuditAgentPaused      = types.AuditAgentPaused
	AuditAgentResumed     = types.AuditAgentResumed
	AuditAgentAdded       = types.AuditAgentAdded
	AuditAgentRemoved     = types.AuditAgentRemoved
	AuditAgentBackendSet  = types.AuditAgentBackendSet
	AuditAgentModelSet    = types.AuditAgentModelSet
	AuditToolApproval     = types.AuditToolApproval

	AuditCopilotTokenMissing = types.AuditCopilotTokenMissing
	AuditClaudeTokenMissing  = types.AuditClaudeTokenMissing
)

// auditActorSystem attributes an event to the hive process itself rather than
// to a person. Matches the dashboard's pseudo-user convention (see
// auditPseudoUsers there): "system" entries never count as user engagement.
// Lifecycle events originate in the agent manager, which has no HTTP request
// and therefore no authenticated user, so they are all system-attributed; the
// human-vs-governor distinction is carried in the detail's trigger= field.
const auditActorSystem = "system"

// AuditSink receives agent lifecycle events for durable, queryable recording.
// See agentaudit.AuditSink for the contract; the alias exists because it is
// the parameter type of SetAuditSink.
type AuditSink = types.AuditSink

// SetAuditSink installs the durable audit sink. Safe to leave unset: a nil
// sink makes every audit call a no-op, which is what unit tests and any
// non-dashboard embedding of the manager get.
func (m *Manager) SetAuditSink(sink AuditSink) {
	m.auditSink.Store(&sink)
}

// audit records a lifecycle event to the durable audit store, if one is
// installed. It is deliberately a no-op when no sink is set rather than an
// error: audit recording must never be able to fail an agent operation.
//
// Stored/read via atomic.Pointer rather than under m.mu for the same reason as
// isGatewayBackend and bobAPIKeyResolver: this is called from the launch path
// (launchInTmux via Start), which ALREADY holds m.mu.Lock(). Re-locking a
// non-reentrant RWMutex on the same goroutine would deadlock startup before
// MarkReady and crash-loop every spoke.
func (m *Manager) audit(action, agentName string, fields map[string]any) {
	m.auditWithActor(auditActorSystem, action, agentName, fields)
}

// auditWithActor is audit with an explicit responsible user, for the paths
// where an operator's identity is known (dashboard-initiated switches).
func (m *Manager) auditWithActor(actor, action, agentName string, fields map[string]any) {
	p := m.auditSink.Load()
	if p == nil || *p == nil {
		return
	}
	(*p).Record(actor, action, agentName, fields)
}

// auditFields builds a fields map from alternating key/value pairs; see
// types.Fields.
func auditFields(kv ...any) map[string]any {
	return types.Fields(kv...)
}
