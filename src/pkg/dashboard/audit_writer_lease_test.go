package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readAuditFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func closeAuditWriterForTest(t *testing.T, a *AuditLog) {
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.writer != nil {
			_ = a.writer.Close()
		}
	})
}

// #13046: during a maxSurge rollout the incoming pod must not append to the
// shared audit.jsonl while the outgoing pod still holds it; once the old pod
// releases, the new one takes over and merges what it spilled, and the old
// pod's post-release entries are merged once its spill goes quiet.
func TestAuditWriterLease_SurgeHandoff(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")

	oldPod := &AuditLog{}
	oldPod.initWriterLease(logPath, "old-pod")
	closeAuditWriterForTest(t, oldPod)
	newPod := &AuditLog{}
	newPod.initWriterLease(logPath, "new-pod")
	closeAuditWriterForTest(t, newPod)

	if !oldPod.owner || newPod.owner {
		t.Fatalf("owner: old=%v new=%v, want old only", oldPod.owner, newPod.owner)
	}

	oldPod.Log("alice", "chat.dashboard.message", "from-old", "")
	newPod.Log("system", "hive_restart", "from-new", "")

	canonical := readAuditFileForTest(t, logPath)
	if !strings.Contains(canonical, "from-old") || strings.Contains(canonical, "from-new") {
		t.Fatalf("canonical before handoff = %q, want only the old pod's entry", canonical)
	}
	newSpill := auditSpillPath(logPath, "new-pod")
	if !strings.Contains(readAuditFileForTest(t, newSpill), "from-new") {
		t.Fatalf("new pod's entry not in its spill file %s", newSpill)
	}

	oldPod.ReleaseWriterLease()
	if _, err := os.Stat(auditLeasePath(logPath)); !os.IsNotExist(err) {
		t.Fatalf("lease file still present after release: %v", err)
	}
	oldPod.Log("alice", "chat.dashboard.message", "old-after-release", "")
	oldSpill := auditSpillPath(logPath, "old-pod")
	if !strings.Contains(readAuditFileForTest(t, oldSpill), "old-after-release") {
		t.Fatalf("released pod did not switch to its spill file")
	}

	newPod.leaseTick()
	if !newPod.owner {
		t.Fatal("new pod did not acquire the released lease")
	}
	canonical = readAuditFileForTest(t, logPath)
	if !strings.Contains(canonical, "from-old") || !strings.Contains(canonical, "from-new") {
		t.Fatalf("canonical after takeover = %q, want both entries", canonical)
	}
	if _, err := os.Stat(newSpill); !os.IsNotExist(err) {
		t.Fatalf("own spill not removed after merge: %v", err)
	}
	if strings.Contains(canonical, "old-after-release") {
		t.Fatal("a still-fresh foreign spill was merged")
	}

	stale := time.Now().Add(-2 * auditLeaseTTL)
	if err := os.Chtimes(oldSpill, stale, stale); err != nil {
		t.Fatal(err)
	}
	newPod.leaseTick()
	if !strings.Contains(readAuditFileForTest(t, logPath), "old-after-release") {
		t.Fatal("quiet foreign spill was not merged by the lease holder")
	}
	if _, err := os.Stat(oldSpill); !os.IsNotExist(err) {
		t.Fatalf("foreign spill not removed after merge: %v", err)
	}

	newPod.Log("bob", "config_github", "after-takeover", "")
	if !strings.Contains(readAuditFileForTest(t, logPath), "after-takeover") {
		t.Fatal("new holder does not write the canonical log")
	}
}

// A lease left by a pod that died without releasing it expires, and the
// startup merge folds in that pod's orphaned spill.
func TestAuditWriterLease_StaleLeaseAndOrphanSpillAtStartup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")
	if err := writeAuditLease(auditLeasePath(logPath), "dead-pod", time.Now().Add(-2*auditLeaseTTL)); err != nil {
		t.Fatal(err)
	}
	orphan := auditSpillPath(logPath, "dead-pod")
	line := `{"ts":"2026-10-19T19:31:30Z","user":"alice","action":"chat.dashboard.message","detail":"orphaned"}` + "\n"
	if err := os.WriteFile(orphan, []byte(line+"not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * auditLeaseTTL)
	if err := os.Chtimes(orphan, stale, stale); err != nil {
		t.Fatal(err)
	}

	a := &AuditLog{}
	a.initWriterLease(logPath, "fresh-pod")
	closeAuditWriterForTest(t, a)

	if !a.owner {
		t.Fatal("stale lease was not taken over")
	}
	if l, ok := readAuditLease(auditLeasePath(logPath)); !ok || l.Holder != "fresh-pod" {
		t.Fatalf("lease = %+v ok=%v, want holder fresh-pod", l, ok)
	}
	canonical := readAuditFileForTest(t, logPath)
	if canonical != line {
		t.Fatalf("canonical = %q, want only the valid orphaned entry", canonical)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan spill not removed: %v", err)
	}
}

// A bare AuditLog (no /data, tests) keeps the lease inert.
func TestAuditWriterLease_BareLogIsInert(t *testing.T) {
	a := &AuditLog{}
	a.leaseTick()
	a.ReleaseWriterLease()
	a.Log("alice", "x", "y", "")
	if len(a.Recent(1)) != 1 {
		t.Fatal("bare audit log stopped recording to the ring")
	}
}
