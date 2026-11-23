package dashboard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

func chainedTestLog(t *testing.T, path string) *AuditLog {
	t.Helper()
	a := &AuditLog{writer: &lumberjack.Logger{Filename: path}}
	t.Cleanup(func() { _ = a.writer.Close() })
	return a
}

func readAuditRows(t *testing.T, path string) []AuditEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditEntry
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuditLogWritesVerifiableChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := chainedTestLog(t, path)
	a.Log("alice", "config_governor_save", "level=4", "")
	a.LogRecord("system", "agent_write_refused", "repo=o/r, number=7", "coder", "o/r", 7)
	a.Log("bob", "agent_restart", "", "coder")

	rows := readAuditRows(t, path)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if rows[0].Seq != 1 || rows[0].PrevHash != "" {
		t.Fatalf("first row should start the chain: %+v", rows[0])
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Seq != int64(i+1) || rows[i].PrevHash != rows[i-1].RowHash {
			t.Fatalf("row %d does not link to row %d: %+v", i+1, i, rows[i])
		}
	}

	report, err := VerifyAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() || report.Chained != 3 {
		t.Fatalf("fresh chain did not verify: %+v", report)
	}
	if head := a.ChainHead(); head != report.Head || head.Hash != rows[2].RowHash {
		t.Fatalf("in-memory head %+v disagrees with file head %+v", head, report.Head)
	}
	// The ring carries the same chain fields as the file.
	if got := a.Recent(1)[0]; got.RowHash != rows[2].RowHash {
		t.Fatalf("ring row_hash %q, file %q", got.RowHash, rows[2].RowHash)
	}
}

func TestAuditLogChainDetectsEditedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := chainedTestLog(t, path)
	a.Log("alice", "config_governor_save", "level=4", "")
	a.Log("alice", "config_governor_save", "level=5", "")
	_ = a.writer.Close()

	data, _ := os.ReadFile(path)
	tampered := bytes.Replace(data, []byte("level=4"), []byte("level=1"), 1)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() || !strings.Contains(report.Problems[0].Reason, "row_hash mismatch") {
		t.Fatalf("edited row not detected: %+v", report)
	}
}

func TestAuditLogChainResumesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	first := chainedTestLog(t, path)
	first.Log("alice", "a", "", "")
	first.Log("alice", "b", "", "")
	head := first.ChainHead()
	_ = first.writer.Close()

	second := chainedTestLog(t, path)
	second.startChain(path)
	if got := second.ChainHead(); got != head {
		t.Fatalf("restart loaded head %+v, want %+v", got, head)
	}
	second.Log("alice", "c", "", "")

	rows := readAuditRows(t, path)
	if len(rows) != 3 {
		t.Fatalf("restart must not write a genesis row onto an existing chain; rows: %+v", rows)
	}
	if rows[2].Seq != 3 || rows[2].PrevHash != head.Hash {
		t.Fatalf("row after restart does not continue the chain: %+v", rows[2])
	}
	if report, _ := VerifyAuditLog(path); !report.OK() {
		t.Fatalf("chain across a restart did not verify: %v", report.Problems)
	}
}

func TestAuditLogChainGenesisAfterLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	legacy := `{"ts":"2026-01-01T00:00:00Z","user":"alice","action":"config_governor_save","detail":"level=4"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	a := chainedTestLog(t, path)
	a.startChain(path)
	a.Log("alice", "config_governor_save", "level=5", "")

	rows := readAuditRows(t, path)
	if len(rows) != 3 || rows[1].Action != AuditActionChainGenesis || rows[1].Seq != 1 || rows[1].PrevHash != "" {
		t.Fatalf("want legacy row, genesis row, chained row; got %+v", rows)
	}
	report, err := VerifyAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() || report.Legacy != 1 || report.Chained != 2 || report.Head.Seq != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestAuditLogChainDetectsRowDroppedAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	a := chainedTestLog(t, path)
	for _, action := range []string{"a", "b", "c", "d"} {
		a.Log("alice", action, "", "")
	}
	_ = a.writer.Close()
	data, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(bytes.TrimSpace(data), []byte("\n"))
	// Simulate a rotation: the oldest two rows in a backup, the rest current.
	backup := filepath.Join(dir, "audit-2026-01-01T00-00-00.000.jsonl")
	if err := os.WriteFile(backup, bytes.Join(lines[:2], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.Join(lines[2:], nil), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if report, _ := VerifyAuditLog(path); !report.OK() || report.Chained != 4 {
		t.Fatalf("chain split across a rotation did not verify: %+v", report)
	}
	// Now drop the last row of the backup.
	if err := os.WriteFile(backup, lines[0], 0o600); err != nil {
		t.Fatal(err)
	}
	report, _ := VerifyAuditLog(path)
	if report.OK() {
		t.Fatal("row removed from a rotated backup went undetected")
	}
}

func TestVerifyAuditLogMissing(t *testing.T) {
	if _, err := VerifyAuditLog(filepath.Join(t.TempDir(), "audit.jsonl")); err != ErrNoAuditLog {
		t.Fatalf("want ErrNoAuditLog, got %v", err)
	}
}

func TestAuditLogWithoutDiskIsUnchained(t *testing.T) {
	a := &AuditLog{}
	a.Log("alice", "a", "", "")
	if got := a.Recent(1)[0]; got.Seq != 0 || got.RowHash != "" {
		t.Fatalf("ring-only log should not chain: %+v", got)
	}
	if !a.ChainHead().IsZero() {
		t.Fatalf("ring-only log has a chain head: %+v", a.ChainHead())
	}
}
