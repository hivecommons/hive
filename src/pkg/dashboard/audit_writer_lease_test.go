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

// A live non-holder that has not logged for longer than the TTL must keep its
// spill: the holder may only merge and remove spills of pods that stopped
// ticking, otherwise the non-holder's later entries land on an unlinked inode.
func TestAuditWriterLease_LiveSpillSurvivesHolderSweep(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")

	holder := &AuditLog{}
	holder.initWriterLease(logPath, "holder-pod")
	closeAuditWriterForTest(t, holder)
	spiller := &AuditLog{}
	spiller.initWriterLease(logPath, "spiller-pod")
	closeAuditWriterForTest(t, spiller)
	if !holder.owner || spiller.owner {
		t.Fatalf("owner: holder=%v spiller=%v, want holder only", holder.owner, spiller.owner)
	}

	spiller.Log("system", "hive_restart", "spilled-early", "")
	spill := auditSpillPath(logPath, "spiller-pod")
	stale := time.Now().Add(-2 * auditLeaseTTL)
	if err := os.Chtimes(spill, stale, stale); err != nil {
		t.Fatal(err)
	}

	spiller.leaseTick()
	if spiller.owner {
		t.Fatal("non-holder took a live lease")
	}
	holder.leaseTick()
	if _, err := os.Stat(spill); err != nil {
		t.Fatalf("live non-holder's spill was removed by the holder sweep: %v", err)
	}
	if strings.Contains(readAuditFileForTest(t, logPath), "spilled-early") {
		t.Fatal("live non-holder's spill was merged while it was still spilling")
	}

	spiller.Log("alice", "chat.dashboard.message", "spilled-late", "")

	holder.ReleaseWriterLease()
	spiller.leaseTick()
	if !spiller.owner {
		t.Fatal("non-holder did not acquire the released lease")
	}
	canonical := readAuditFileForTest(t, logPath)
	if !strings.Contains(canonical, "spilled-early") || !strings.Contains(canonical, "spilled-late") {
		t.Fatalf("canonical after takeover = %q, want both spilled entries", canonical)
	}
}

// If a spill file disappears under a non-holder, its next entry recreates the
// file instead of being written to the removed inode.
func TestAuditWriterLease_RemovedSpillIsReopened(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")

	holder := &AuditLog{}
	holder.initWriterLease(logPath, "holder-pod")
	closeAuditWriterForTest(t, holder)
	spiller := &AuditLog{}
	spiller.initWriterLease(logPath, "spiller-pod")
	closeAuditWriterForTest(t, spiller)

	spill := auditSpillPath(logPath, "spiller-pod")
	spiller.Log("system", "hive_restart", "before-remove", "")
	if err := os.Remove(spill); err != nil {
		t.Fatal(err)
	}
	spiller.Log("alice", "chat.dashboard.message", "after-remove", "")
	if !strings.Contains(readAuditFileForTest(t, spill), "after-remove") {
		t.Fatal("entry after spill removal was not written to a recreated spill file")
	}
}

// A pod that cannot write the lease must not become a second direct writer of
// audit.jsonl; it spills until a later tick acquires the lease.
func TestAuditWriterLease_LeaseWriteFailureSpills(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")
	// A non-empty directory at the lease path makes both the read and the
	// rename-into-place fail.
	if err := os.MkdirAll(filepath.Join(auditLeasePath(logPath), "block"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &AuditLog{}
	a.initWriterLease(logPath, "unlucky-pod")
	closeAuditWriterForTest(t, a)
	if a.owner {
		t.Fatal("pod became the writer without holding the lease")
	}
	a.Log("alice", "chat.dashboard.message", "while-unleased", "")
	if strings.Contains(readAuditFileForTest(t, logPath), "while-unleased") {
		t.Fatal("unleased pod appended to the canonical log")
	}
	if !strings.Contains(readAuditFileForTest(t, auditSpillPath(logPath, "unlucky-pod")), "while-unleased") {
		t.Fatal("unleased pod's entry not in its spill file")
	}

	if err := os.RemoveAll(auditLeasePath(logPath)); err != nil {
		t.Fatal(err)
	}
	a.leaseTick()
	if !a.owner {
		t.Fatal("pod did not acquire the lease once it became writable")
	}
	if !strings.Contains(readAuditFileForTest(t, logPath), "while-unleased") {
		t.Fatal("spilled entry not merged after the lease was acquired")
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
