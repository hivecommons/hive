package turn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistFailsWhenDirectoryMissing(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "missing", "turn.json")}
	err := store.Persist(context.Background(), testEnvelope())
	if err == nil {
		t.Fatal("Persist succeeded without a containing directory")
	}
	if !strings.Contains(err.Error(), "create temporary envelope") {
		t.Fatalf("error = %v, want temp-file creation failure", err)
	}
}

func TestPersistCommitFailureCleansUpTempFile(t *testing.T) {
	// The destination path is an existing non-empty directory, so the final
	// rename must fail after the temp file was written and synced.
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := FileStore{Path: filepath.Join(target, "turn.json")}
	if err := inner.Persist(context.Background(), testEnvelope()); err != nil {
		t.Fatalf("seed inner envelope: %v", err)
	}

	err := FileStore{Path: target}.Persist(context.Background(), testEnvelope())
	if err == nil {
		t.Fatal("Persist committed over a non-empty directory")
	}
	if !strings.Contains(err.Error(), "commit envelope") {
		t.Fatalf("error = %v, want commit failure", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, ".turn-envelope-*.tmp"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("temporary files after failed commit = %v, err = %v", matches, globErr)
	}
}

func TestLoadFailsWhenEnvelopeMissing(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "turn.json")}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load succeeded on a missing envelope")
	} else if !strings.Contains(err.Error(), "read envelope") {
		t.Fatalf("error = %v, want read failure", err)
	}
}

func TestValidateRejectsVersionMismatch(t *testing.T) {
	env := testEnvelope()
	env.Version = EnvelopeVersion + 1
	err := env.Validate()
	if err == nil {
		t.Fatal("Validate accepted a future envelope version")
	}
	if !strings.Contains(err.Error(), "envelope version") {
		t.Fatalf("error = %v, want version mismatch", err)
	}
	if _, err := ParseEnvelope([]byte(`{"version":99,"session_id":"s"}`)); err == nil {
		t.Fatal("ParseEnvelope accepted a version mismatch")
	}
}

func TestJournalAmbiguousReturnsOnlyIntendedEntries(t *testing.T) {
	j := Journal{Entries: []JournalEntry{
		{IdempotencyKey: "k-done", Status: OpSucceeded},
		{IdempotencyKey: "k-open", Status: OpIntended},
		{IdempotencyKey: "k-failed", Status: OpFailed},
		{IdempotencyKey: "k-open-2", Status: OpIntended},
	}}
	got := j.Ambiguous()
	if len(got) != 2 {
		t.Fatalf("Ambiguous() returned %d entries, want 2: %+v", len(got), got)
	}
	if got[0].IdempotencyKey != "k-open" || got[1].IdempotencyKey != "k-open-2" {
		t.Fatalf("Ambiguous() = %+v, want the two intended entries in order", got)
	}

	empty := Journal{}
	if entries := empty.Ambiguous(); entries != nil {
		t.Fatalf("Ambiguous() on empty journal = %+v, want nil", entries)
	}
}

func TestExecutorNowFallsBackToWallClock(t *testing.T) {
	x := &JournaledExecutor{}
	before := time.Now().UTC().Add(-time.Second)
	got := x.now()
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("now() = %v, want within [%v, %v]", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("now() location = %v, want UTC", got.Location())
	}

	fixed := time.Date(2026, 9, 7, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
	x.Now = func() time.Time { return fixed }
	if got := x.now(); !got.Equal(fixed) || got.Location() != time.UTC {
		t.Fatalf("now() with injected clock = %v (loc %v), want %v in UTC", got, got.Location(), fixed)
	}
}
