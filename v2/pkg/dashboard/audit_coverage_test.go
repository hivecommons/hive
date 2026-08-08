package dashboard

import (
	"testing"
	"time"
)

func TestAuditLog_LogAndRecent(t *testing.T) {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}

	a.Log("alice", "config_save", "key=region", "")
	a.Log("bob", "agent_restart", "agent=scanner", "scanner")
	a.Log("", "boot", "startup", "") // empty user becomes "system"

	entries := a.Recent(10)
	if len(entries) != 3 {
		t.Fatalf("Recent(10) = %d entries, want 3", len(entries))
	}
	// newest first
	if entries[0].User != "system" {
		t.Errorf("entries[0].User = %q, want system (newest)", entries[0].User)
	}
	if entries[2].User != "alice" {
		t.Errorf("entries[2].User = %q, want alice (oldest)", entries[2].User)
	}
}

func TestAuditLog_RecentCapped(t *testing.T) {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}
	for i := 0; i < 5; i++ {
		a.Log("user", "action", "", "")
	}
	entries := a.Recent(3)
	if len(entries) != 3 {
		t.Errorf("Recent(3) = %d entries, want 3", len(entries))
	}
}

func TestAuditLog_RecentZeroReturnsAll(t *testing.T) {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}
	a.Log("x", "a", "", "")
	a.Log("y", "b", "", "")
	entries := a.Recent(0)
	if len(entries) != 2 {
		t.Errorf("Recent(0) = %d entries, want 2 (all)", len(entries))
	}
}

func TestAuditLog_NoteUserAction_PseudoUsers(t *testing.T) {
	a := &AuditLog{lastAction: make(map[string]time.Time)}
	ts := "2026-01-15T10:00:00Z"

	// Pseudo-users should not be tracked.
	for _, u := range []string{"", "system", "local", "unknown"} {
		a.noteUserAction(u, ts)
	}
	if len(a.lastAction) != 0 {
		t.Errorf("pseudo-users tracked: %v", a.lastAction)
	}

	// Real user should be tracked.
	a.noteUserAction("alice", ts)
	if _, ok := a.lastAction["alice"]; !ok {
		t.Error("real user alice not tracked")
	}
}

func TestAuditLog_NoteUserAction_NeverMovesBackward(t *testing.T) {
	a := &AuditLog{lastAction: make(map[string]time.Time)}
	a.noteUserAction("alice", "2026-01-15T12:00:00Z")
	a.noteUserAction("alice", "2026-01-15T10:00:00Z") // earlier

	expected, _ := time.Parse(time.RFC3339, "2026-01-15T12:00:00Z")
	if got := a.lastAction["alice"]; !got.Equal(expected) {
		t.Errorf("lastAction moved backward: got %v, want %v", got, expected)
	}
}

func TestAuditLog_NoteUserAction_BoundsCheck(t *testing.T) {
	a := &AuditLog{lastAction: make(map[string]time.Time)}
	// Fill to capacity with known users.
	for i := 0; i < auditMaxTrackedActionUsers; i++ {
		a.noteUserAction("user"+string(rune('A'+i%26))+string(rune('0'+i/26)), "2026-01-15T10:00:00Z")
	}
	before := len(a.lastAction)
	// A new unknown user should be rejected (cap hit, not previously known).
	a.noteUserAction("brandnew_never_seen", "2026-01-15T11:00:00Z")
	if len(a.lastAction) > before {
		t.Errorf("exceeded cap: %d > %d", len(a.lastAction), before)
	}
}

func TestAuditLog_LastUserActions(t *testing.T) {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}
	a.Log("alice", "action1", "", "")
	a.Log("system", "boot", "", "") // pseudo-user, not tracked

	actions := a.LastUserActions()
	if _, ok := actions["alice"]; !ok {
		t.Error("alice missing from LastUserActions")
	}
	if _, ok := actions["system"]; ok {
		t.Error("system (pseudo-user) should not appear in LastUserActions")
	}
}

func TestAuditLog_RingRotation(t *testing.T) {
	a := &AuditLog{
		ring:       make([]AuditEntry, 0, auditRingCap),
		lastAction: make(map[string]time.Time),
	}
	// Fill beyond capacity to test rotation.
	for i := 0; i < auditRingCap+10; i++ {
		a.Log("user", "action", "", "")
	}
	if len(a.ring) > auditRingCap {
		t.Errorf("ring grew beyond cap: %d > %d", len(a.ring), auditRingCap)
	}
}

func TestAuditDetail(t *testing.T) {
	cases := []struct {
		kv   []string
		want string
	}{
		{nil, ""},
		{[]string{"key", "val"}, "key=val"},
		{[]string{"a", "1", "b", "2"}, "a=1, b=2"},
		{[]string{"a", "1", "odd"}, "a=1"}, // odd element ignored
	}
	for _, c := range cases {
		got := auditDetail(c.kv...)
		if got != c.want {
			t.Errorf("auditDetail(%v) = %q, want %q", c.kv, got, c.want)
		}
	}
}

func TestAuditLog_NilLastActionMap(t *testing.T) {
	// noteUserAction should handle a nil lastAction map (lazy init).
	a := &AuditLog{}
	a.noteUserAction("alice", "2026-01-15T10:00:00Z")
	if a.lastAction == nil {
		t.Fatal("lastAction should be initialized")
	}
	if _, ok := a.lastAction["alice"]; !ok {
		t.Error("alice not tracked after lazy init")
	}
}
