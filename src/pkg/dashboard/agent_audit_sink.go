package dashboard

import (
	"github.com/hivecommons/hive/pkg/agent"
)

// agentAuditSink adapts the dashboard's AuditLog to agent.AuditSink so the
// agent manager can record lifecycle events (start, stop, launch failure,
// backend/model change) into the durable, queryable audit store.
//
// The indirection exists because the import direction forbids the obvious
// call: pkg/dashboard already imports pkg/agent, so pkg/agent cannot import
// pkg/dashboard back. cmd/hive owns both objects and injects this adapter.
type agentAuditSink struct {
	log *AuditLog
}

// Record implements agent.AuditSink. Fields are rendered with the shared
// formatter in pkg/agent so the detail column has one canonical shape
// regardless of which side produced the entry.
func (s *agentAuditSink) Record(actor, action, agentName string, fields map[string]any) {
	if s == nil || s.log == nil {
		return
	}
	s.log.Log(actor, action, agent.FormatAuditDetail(fields), agentName)
}

// AgentAuditSink returns an agent.AuditSink backed by this server's audit log,
// for injection into the agent manager at wiring time (see cmd/hive/main.go).
func (s *Server) AgentAuditSink() agent.AuditSink {
	return &agentAuditSink{log: s.audit}
}
