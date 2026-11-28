package dashboard

import (
	"testing"
	"time"
)

// TestLastUserActionsDoesNotWaitOnAuditWriteLock is the #20300 regression:
// the heartbeat's user_last_actions collector ran past the 10s collect budget
// because LastUserActions queued on AuditLog.mu, which an audit write holds
// across its on-disk I/O. Holding mu here stands in for a write stuck on a
// slow /data volume; the read must still return promptly.
func TestLastUserActionsDoesNotWaitOnAuditWriteLock(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	a.Log("alice", "config_save", "", "")

	a.mu.Lock()
	defer a.mu.Unlock()

	done := make(chan map[string]string, 1)
	go func() { done <- a.LastUserActions() }()
	select {
	case got := <-done:
		if got["alice"] == "" {
			t.Fatalf("LastUserActions = %v, want an entry for alice", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LastUserActions blocked on AuditLog.mu (held by an in-flight audit write)")
	}
}
