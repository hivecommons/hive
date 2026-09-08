package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #6287: the token-access audit log must not be writable by the agents it
// audits. These tests assert the INVARIANT on the durable log (owned by the
// hive process, no group/other bits at all, so no agent UID can open it for
// write, append or truncate) and the attribution rule on ingest (an event is
// attributed to the uid that OWNS the spool file, never to the uid it claims).
// They deliberately do not assert on log output: a warning is not a control.

func testTokenAccessPaths(t *testing.T) (spool, logPath string) {
	t.Helper()
	root := t.TempDir()
	spool = filepath.Join(root, "token-access-events")
	logPath = filepath.Join(root, "token-access.jsonl")
	prevSpool, prevLog := tokenAccessSpoolDirForTest, tokenAccessLogPathForTest
	tokenAccessSpoolDirForTest, tokenAccessLogPathForTest = spool, logPath
	t.Cleanup(func() { tokenAccessSpoolDirForTest, tokenAccessLogPathForTest = prevSpool, prevLog })
	return spool, logPath
}

func dropTokenAccessEvent(t *testing.T, spool, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(spool, name), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readLogLines(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not a JSON object: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// assertLogNotAgentWritable is the invariant: the log exists, is owned by the
// process running the hive, and carries no group or other permission bit.
// Every agent UID shares the hive's primary group, so a single group bit is
// an agent bit; 0600 is the only acceptable mode.
func assertLogNotAgentWritable(t *testing.T, logPath string) {
	t.Helper()
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	if got := fi.Mode().Perm(); got != tokenAccessLogMode {
		t.Fatalf("audit log mode = %04o, want %04o (agents must have no access at all)", got, tokenAccessLogMode)
	}
	if got := fi.Mode().Perm() & 0o077; got != 0 {
		t.Fatalf("audit log grants group/other bits %04o; an agent UID could write it", got)
	}
	if owner := fileOwnerUID(fi); owner >= 0 && owner != os.Getuid() {
		t.Fatalf("audit log owner uid = %d, want the hive process uid %d", owner, os.Getuid())
	}
}

// A log left behind by the previous design (dev:node 0664, group-writable by
// every agent) must be tightened at boot, not trusted.
func TestTokenAccessAudit_PrepareTightensLegacyGroupWritableLog(t *testing.T) {
	_, logPath := testTokenAccessPaths(t)
	if err := os.WriteFile(logPath, []byte(`{"op":"gh","uid":2001}`+"\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0o664); err != nil { // umask-proof
		t.Fatal(err)
	}
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	assertLogNotAgentWritable(t, logPath)
	// Tightening must not lose what was already recorded.
	if lines := readLogLines(t, logPath); len(lines) != 1 {
		t.Fatalf("existing entries lost on tighten: got %d lines", len(lines))
	}
}

// A fresh boot creates the log with the invariant already in force, and the
// spool as a drop-box agents can write into but not enumerate.
func TestTokenAccessAudit_PrepareCreatesHiveOwnedLogAndDropBox(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	assertLogNotAgentWritable(t, logPath)

	fi, err := os.Stat(spool)
	if err != nil {
		t.Fatalf("spool missing: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o730 {
		t.Fatalf("spool perm = %04o, want 0730 (group create+search, no list)", got)
	}
	if fi.Mode()&os.ModeSticky == 0 {
		t.Fatal("spool lacks the sticky bit; an agent could unlink a peer's queued event")
	}
	if runtime.GOOS == "linux" && fi.Mode()&os.ModeSetgid == 0 {
		t.Fatal("spool lacks setgid; dropped events would not inherit the hive-readable group")
	}
}

// Ingest attributes each event to the uid that owns the spool file. An event
// claiming to be some other agent lands under the writer's real uid with the
// claim preserved as evidence, and the log stays non-writable afterwards.
func TestTokenAccessAudit_IngestAttributesByFileOwnerNotClaim(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("file ownership is only reported on unix")
	}
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	me := os.Getuid()
	forgedUID := me + 1000
	dropTokenAccessEvent(t, spool, "1000000000000000001-11.json",
		`{"ts":"2026-09-08T00:00:00Z","agent":"scanner","uid":`+itoa(me)+`,"op":"gh","cmd":"gh pr list"}`)
	dropTokenAccessEvent(t, spool, "1000000000000000002-12.json",
		`{"ts":"2026-09-08T00:00:01Z","agent":"peer","uid":`+itoa(forgedUID)+`,"op":"gh","cmd":"gh pr merge 1"}`)

	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, time.Now()); n != 2 {
		t.Fatalf("ingested %d events, want 2", n)
	}
	assertLogNotAgentWritable(t, logPath)

	lines := readLogLines(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}
	if got := lines[0]["uid"]; got != float64(me) {
		t.Fatalf("honest event uid = %v, want %d", got, me)
	}
	if _, present := lines[0][tokenAccessClaimedUIDKey]; present {
		t.Fatal("honest event must not carry a claimed_uid")
	}
	if got := lines[1]["uid"]; got != float64(me) {
		t.Fatalf("forged event attributed to uid %v, want the real writer %d", got, me)
	}
	if got := lines[1][tokenAccessClaimedUIDKey]; got != float64(forgedUID) {
		t.Fatalf("forged event claimed_uid = %v, want %d", got, forgedUID)
	}
	if got := lines[1]["cmd"]; got != "gh pr merge 1" {
		t.Fatalf("event payload not preserved: cmd = %v", got)
	}

	// Consumed events leave the spool; nothing is left for the author to edit.
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("spool not drained: %d entries remain", len(entries))
	}
}

// Only well-formed, bounded JSON objects reach the log: the dashboard hands
// lines back verbatim as json.RawMessage, so garbage or an oversized blob
// must be rejected and removed, never appended. In-progress .tmp files are
// left alone until they go stale.
func TestTokenAccessAudit_IngestRejectsMalformedAndOversized(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	dropTokenAccessEvent(t, spool, "1-notjson.json", "not json at all\n")
	dropTokenAccessEvent(t, spool, "2-array.json", `[1,2,3]`)
	dropTokenAccessEvent(t, spool, "3-huge.json", `{"cmd":"`+strings.Repeat("x", tokenAccessMaxEventBytes)+`"}`)
	dropTokenAccessEvent(t, spool, "4-empty.json", "")
	dropTokenAccessEvent(t, spool, "5-good.json", `{"op":"git-credential","host":"github.com"}`)
	dropTokenAccessEvent(t, spool, "6-inflight.json"+tokenAccessSpoolTmpSuffix, `{"op":"gh"}`)

	now := time.Now()
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, now); n != 1 {
		t.Fatalf("ingested %d events, want exactly the one good event", n)
	}
	lines := readLogLines(t, logPath)
	if len(lines) != 1 || lines[0]["op"] != "git-credential" {
		t.Fatalf("log = %v, want only the good event", lines)
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "6-inflight.json"+tokenAccessSpoolTmpSuffix {
		t.Fatalf("spool after ingest = %v, want only the in-flight .tmp", entries)
	}
	// A .tmp older than the stale age is an abandoned write and gets swept.
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, now.Add(2*tokenAccessSpoolStaleTmpAge)); n != 0 {
		t.Fatalf("stale sweep appended %d events, want 0", n)
	}
	if entries, _ = os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("stale .tmp not swept: %v", entries)
	}
	assertLogNotAgentWritable(t, logPath)
}

// The boot entry point runs the ingest on a ticker and stops on ctx cancel.
func TestTokenAccessAudit_WatcherIngestsAndStops(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	prev := tokenAccessPollInterval
	tokenAccessPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { tokenAccessPollInterval = prev })

	ctx, cancel := context.WithCancel(context.Background())
	done := StartTokenAccessAuditWatcher(ctx, quietLogger())
	dropTokenAccessEvent(t, spool, "1-evt.json", `{"op":"gh","cmd":"gh issue list"}`)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), "gh issue list") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never ingested the event")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop on cancel")
	}
	assertLogNotAgentWritable(t, logPath)
}

// A spool that cannot be created disables the watcher instead of spinning.
func TestTokenAccessAudit_WatcherDisabledWhenSpoolUncreatable(t *testing.T) {
	testTokenAccessPaths(t)
	tokenAccessSpoolDirForTest = blockedRequestDir(t)
	done := StartTokenAccessAuditWatcher(context.Background(), quietLogger())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher should have disabled itself on an uncreatable spool")
	}
}
