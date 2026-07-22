package dashboard

import (
	"path/filepath"
	"strconv"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestAuditLogOnceSurvivesDisplayRingEvictionAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writer := &lumberjack.Logger{Filename: path, MaxSize: 5, MaxBackups: 3, MaxAge: 90}
	audit := &AuditLog{
		writer:  writer,
		ring:    make([]AuditEntry, 0, auditRingCap),
		seenIDs: make(map[string]struct{}),
	}
	const firstID = "visual-work-completion:first"
	if appended, err := audit.LogOnce(firstID, "governor", "visual_work", "first", ""); err != nil || !appended {
		t.Fatalf("first audit append = %v, %v", appended, err)
	}
	for index := 0; index < auditRingCap+25; index++ {
		id := "visual-work-completion:later-" + strconv.Itoa(index)
		if appended, err := audit.LogOnce(id, "governor", "visual_work", "later", ""); err != nil || !appended {
			t.Fatalf("later audit append %d = %v, %v", index, appended, err)
		}
	}
	if len(audit.ring) != auditRingCap {
		t.Fatalf("display ring size = %d, want %d", len(audit.ring), auditRingCap)
	}
	if appended, err := audit.LogOnce(firstID, "governor", "visual_work", "replay", ""); err != nil || appended {
		t.Fatalf("evicted audit replay = %v, %v", appended, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap), seenIDs: make(map[string]struct{})}
	if err := reloaded.loadAuditFile(path, true); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ring) != auditRingCap+26 {
		t.Fatalf("reloaded untrimmed audit entries = %d", len(reloaded.ring))
	}
	// loadFromDisk applies this display-only trim after indexing every ID.
	reloaded.ring = reloaded.ring[len(reloaded.ring)-auditRingCap:]
	if appended, err := reloaded.LogOnce(firstID, "governor", "visual_work", "restart replay", ""); err != nil || appended {
		t.Fatalf("reloaded audit replay = %v, %v", appended, err)
	}
	if appended, err := reloaded.LogOnce("visual-work-completion:unseen", "governor", "visual_work", "memory only", ""); err == nil || appended {
		t.Fatalf("memory-only durable audit = %v, %v", appended, err)
	}
}
