package extwork

// AuditRecorder is the shape of the agent audit sink (pkg/agent.AuditSink)
// restated here so this package need not import it: actor is "system" for
// hive-originated events, action is the audit action, agentName is the
// subject, and fields carry structured detail.
type AuditRecorder interface {
	Record(actor, action, agentName string, fields map[string]any)
}

// auditActorSystem attributes progress events to the hive process itself.
const auditActorSystem = "system"

// AuditProgressSink writes progress events to an AuditRecorder as system
// actions keyed by assignment id, which is the lease's task id, so every
// transport-level fact lands on the lease's audit trail.
type AuditProgressSink struct {
	recorder AuditRecorder
}

// NewAuditProgressSink wraps a recorder; a nil recorder yields a no-op sink.
func NewAuditProgressSink(recorder AuditRecorder) *AuditProgressSink {
	return &AuditProgressSink{recorder: recorder}
}

// Record implements ProgressSink.
func (a *AuditProgressSink) Record(ev ProgressEvent) {
	if a == nil || a.recorder == nil {
		return
	}
	fields := map[string]any{"execution_key": string(ev.ExecutionKey)}
	if ev.State != "" {
		fields["state"] = string(ev.State)
	}
	for k, v := range ev.Fields {
		fields[k] = v
	}
	a.recorder.Record(auditActorSystem, ev.Action, ev.AssignmentID, fields)
}
