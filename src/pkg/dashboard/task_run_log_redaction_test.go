package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #7334: the task-run log bypassed the redaction boundary the live fleet view
// enforces, while being strictly MORE exposed than the fleet view — public,
// unauthenticated, durable past disconnect, and readable per-username over a
// window up to a year. These tests pin the boundary at the write edge so the
// secret is never at rest, not merely absent from one handler's output.

// writeOneTaskRun points the log at a temp file, appends one record, and
// returns the record as it was actually persisted.
func writeOneTaskRun(t *testing.T, rec TaskRunRecord) TaskRunRecord {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "task_runs.jsonl")
	orig := taskRunLogPath
	taskRunLogPath = path
	t.Cleanup(func() { taskRunLogPath = orig })

	hub := &ContributeWSHub{}
	hub.appendTaskRun(rec)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("no record written")
	}
	var got TaskRunRecord
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return got
}

func TestAppendTaskRunRedactsReasonAtRest(t *testing.T) {
	// The motivating case: a CLI printing a credential into an auth error,
	// which the relay then hands back verbatim as its task_failed reason.
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123"
	got := writeOneTaskRun(t, TaskRunRecord{
		TaskID:   "task-1",
		Username: "alice",
		Outcome:  outcomeFailed,
		Reason:   "auth failed using token " + secret,
	})

	if strings.Contains(got.Reason, secret) {
		t.Fatalf("token survived into the log at rest: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "***") {
		t.Errorf("want a redaction marker, got %q", got.Reason)
	}
	// Redaction must not eat the operator-useful context around the secret.
	if !strings.Contains(got.Reason, "auth failed using token") {
		t.Errorf("surrounding context lost: %q", got.Reason)
	}
}

func TestAppendTaskRunRedactsVerdictReasonAtRest(t *testing.T) {
	// VerdictReason reached the record with nothing but a TrimSpace, so it was
	// the less obvious half of #7334 — same exposure, no bound at all.
	secret := "sk-abcdefghijklmnop"
	got := writeOneTaskRun(t, TaskRunRecord{
		TaskID:        "task-1",
		Username:      "alice",
		Outcome:       outcomeCompleted,
		VerdictReason: "model rejected key " + secret,
	})

	if strings.Contains(got.VerdictReason, secret) {
		t.Fatalf("api key survived into the log at rest: %q", got.VerdictReason)
	}
	if !strings.Contains(got.VerdictReason, "REDACTED") {
		t.Errorf("want a redaction marker, got %q", got.VerdictReason)
	}
}

func TestAppendTaskRunBoundsClientSuppliedText(t *testing.T) {
	// A registered relay must not be able to bloat records that every
	// anonymous reader then downloads. 500 records * unbounded strings was
	// the actual exposure; the response cap alone did not bound the payload.
	long := strings.Repeat("A", maxFailureReasonLen*3)
	got := writeOneTaskRun(t, TaskRunRecord{
		TaskID:        "task-1",
		Username:      "alice",
		Outcome:       outcomeFailed,
		Reason:        long,
		VerdictReason: long,
	})

	for name, field := range map[string]string{"reason": got.Reason, "verdict_reason": got.VerdictReason} {
		if len([]rune(field)) > maxFailureReasonLen+len([]rune("… (truncated)")) {
			t.Errorf("%s not bounded: %d runes", name, len([]rune(field)))
		}
		if !strings.HasSuffix(field, "(truncated)") {
			t.Errorf("%s should be marked truncated, got tail %q", name, field[len(field)-20:])
		}
	}
}

func TestAppendTaskRunLeavesCleanTextAlone(t *testing.T) {
	// Redaction is a boundary, not a filter: an ordinary failure reason is the
	// single most useful string on this endpoint and must arrive intact.
	reason := "build failed: ./cmd/hive/main.go:42: undefined: Foo"
	got := writeOneTaskRun(t, TaskRunRecord{
		TaskID:   "task-1",
		Username: "alice",
		Outcome:  outcomeFailed,
		Reason:   reason,
	})
	if got.Reason != reason {
		t.Errorf("clean reason altered:\n got %q\nwant %q", got.Reason, reason)
	}
}

func TestHandleContributeRunsServesRedactedReason(t *testing.T) {
	// End to end: what an anonymous reader actually receives.
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123"
	dir := t.TempDir()
	path := filepath.Join(dir, "task_runs.jsonl")
	orig := taskRunLogPath
	taskRunLogPath = path
	t.Cleanup(func() { taskRunLogPath = orig })

	hub := &ContributeWSHub{}
	hub.appendTaskRun(TaskRunRecord{
		TaskID:   "task-1",
		Username: "alice",
		Outcome:  outcomeFailed,
		Reason:   "auth failed using token " + secret,
	})

	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleContributeRuns(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=alice", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("token served to an anonymous reader: %s", rr.Body.String())
	}
}
