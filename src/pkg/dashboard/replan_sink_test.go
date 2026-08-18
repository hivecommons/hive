package dashboard

import (
	"log/slog"
	"testing"
)

func TestReplanSink(t *testing.T) {
	srv := NewServer(0, slog.Default())
	// nil notifier → Notify is a safe no-op.
	sink := NewReplanSink(srv, nil)

	sink.Audit("planning", "replan", "epic e1 replanned", "architect")
	sink.Alert("plan-stall-e1", "warning", "e1 stalled")
	sink.Notify("Plan replan", "e1 replanned") // must not panic with nil notifier

	entries := srv.GetAudit().Recent(10)
	found := false
	for _, e := range entries {
		if e.Action == "replan" && e.Agent == "architect" {
			found = true
		}
	}
	if !found {
		t.Errorf("replan audit entry not recorded: %+v", entries)
	}

	srv.systemAlertsMu.RLock()
	defer srv.systemAlertsMu.RUnlock()
	var alerted bool
	for _, a := range srv.systemAlerts {
		if a.ID == "plan-stall-e1" && a.Severity == "warning" {
			alerted = true
		}
	}
	if !alerted {
		t.Errorf("replan alert not raised: %+v", srv.systemAlerts)
	}
}
