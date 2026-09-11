package agent

import "github.com/hivecommons/hive/pkg/agentaudit"

// The audit vocabulary — action names, the AuditSink interface, and the
// detail-string formatting — lives in the stdlib-only leaf pkg/agentaudit so
// policy-layer packages can emit audit events without importing this runtime
// package. The aliases below keep every existing consumer of agent.Audit* /
// agent.AuditSink / agent.FormatAuditDetail working unchanged.
const (
	AuditAgentStarted     = agentaudit.AuditAgentStarted
	AuditAgentStartFailed = agentaudit.AuditAgentStartFailed
	AuditAgentStopped     = agentaudit.AuditAgentStopped
	AuditAgentKicked      = agentaudit.AuditAgentKicked
	AuditAgentPaused      = agentaudit.AuditAgentPaused
	AuditAgentResumed     = agentaudit.AuditAgentResumed
	AuditAgentAdded       = agentaudit.AuditAgentAdded
	AuditAgentRemoved     = agentaudit.AuditAgentRemoved
	AuditAgentBackendSet  = agentaudit.AuditAgentBackendSet
	AuditAgentModelSet    = agentaudit.AuditAgentModelSet
	AuditToolApproval     = agentaudit.AuditToolApproval

	AuditCopilotTokenMissing = agentaudit.AuditCopilotTokenMissing
	AuditClaudeTokenMissing  = agentaudit.AuditClaudeTokenMissing
)

// auditActorSystem attributes an event to the hive process itself rather than
// to a person. Matches the dashboard's pseudo-user convention (see
// auditPseudoUsers there): "system" entries never count as user engagement.
// Lifecycle events originate in the agent manager, which has no HTTP request
// and therefore no authenticated user, so they are all system-attributed; the
// human-vs-governor distinction is carried in the detail's trigger= field.
const auditActorSystem = "system"

// AuditSink receives agent lifecycle events for durable, queryable recording.
// See agentaudit.AuditSink for the contract; the alias exists so the many
// existing consumers keep compiling unchanged.
type AuditSink = agentaudit.AuditSink

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
// agentaudit.Fields.
func auditFields(kv ...any) map[string]any {
	return agentaudit.Fields(kv...)
}

// FormatAuditDetail renders a fields map as the stable "k=v, k=v" detail
// string; see agentaudit.FormatAuditDetail.
func FormatAuditDetail(fields map[string]any) string {
	return agentaudit.FormatAuditDetail(fields)
}
