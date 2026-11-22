package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auditchain"
)

func writeSealedAuditLog(t *testing.T, rows ...string) string {
	t.Helper()
	var head auditchain.Link
	var buf bytes.Buffer
	for _, r := range rows {
		line, next, err := auditchain.Seal([]byte(r), head)
		if err != nil {
			t.Fatal(err)
		}
		head = next
		buf.Write(line)
		buf.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuditVerifySubcommand(t *testing.T) {
	path := writeSealedAuditLog(t,
		`{"ts":"2026-01-01T00:00:00Z","user":"system","action":"audit_chain_genesis"}`,
		`{"ts":"2026-01-01T00:00:01Z","user":"alice","action":"config_governor_save","detail":"level=4"}`)

	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{auditVerifySubcommand, path}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d stdout=%s stderr=%s", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "OK: chain intact") {
		t.Fatalf("missing OK line: %s", stdout.String())
	}

	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte("level=4"), []byte("level=9"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := runAuditVerify([]string{"--json", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("tampered log: exit %d, want 1; %s", code, stdout.String())
	}
	var out struct {
		OK       bool                 `json:"ok"`
		Problems []auditchain.Problem `json:"problems"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("--json output: %v\n%s", err, stdout.String())
	}
	if out.OK || len(out.Problems) != 1 {
		t.Fatalf("want one problem, got %+v", out)
	}

	if code := runAuditVerify([]string{filepath.Join(t.TempDir(), "missing.jsonl")}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing log: exit %d, want 2", code)
	}
}
