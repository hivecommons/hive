package dashboard

import "github.com/hivecommons/hive/pkg/notify"

// TrajectorySink adapts the dashboard Server (+ notifier) to
// trajectory.Sink, so the review lane can record audit entries, raise system
// alerts, and push notifications through the same channels the rest of hive
// uses. The notifier may be nil (notifications are then skipped).
type TrajectorySink struct {
	srv      *Server
	notifier *notify.Notifier
}

// NewTrajectorySink builds the adapter.
func NewTrajectorySink(srv *Server, notifier *notify.Notifier) *TrajectorySink {
	return &TrajectorySink{srv: srv, notifier: notifier}
}

// Audit records a trajectory decision in the audit log under the "trajectory"
// actor.
func (t *TrajectorySink) Audit(agent, action, detail string) {
	t.srv.AuditLog("trajectory", action, detail, agent)
}

// Alert raises (or updates) a dashboard system alert.
func (t *TrajectorySink) Alert(id, severity, message string) {
	t.srv.AddSystemAlert(id, severity, message)
}

// Notify sends a high-priority notification when a notifier is configured.
func (t *TrajectorySink) Notify(title, message string) {
	if t.notifier != nil {
		t.notifier.Send(title, message, notify.PriorityHigh)
	}
}
