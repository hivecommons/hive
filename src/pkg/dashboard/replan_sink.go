package dashboard

import "github.com/hivecommons/hive/pkg/notify"

// ReplanSink adapts the dashboard Server (+ notifier) to planning.Sink, so the
// Phase 3 stall-replan lane records audit entries, raises system alerts, and
// pushes notifications through the same channels the trajectory and budget
// alerts use. The notifier may be nil (notifications are then skipped).
type ReplanSink struct {
	srv      *Server
	notifier *notify.Notifier
}

// NewReplanSink builds the adapter.
func NewReplanSink(srv *Server, notifier *notify.Notifier) *ReplanSink {
	return &ReplanSink{srv: srv, notifier: notifier}
}

// Audit records a replan decision in the audit log.
func (r *ReplanSink) Audit(actor, action, detail, agent string) {
	r.srv.AuditLog(actor, action, detail, agent)
}

// Alert raises (or updates) a dashboard system alert.
func (r *ReplanSink) Alert(id, severity, message string) {
	r.srv.AddSystemAlert(id, severity, message)
}

// Notify sends a high-priority notification when a notifier is configured.
func (r *ReplanSink) Notify(title, message string) {
	if r.notifier != nil {
		r.notifier.Send(title, message, notify.PriorityHigh)
	}
}
