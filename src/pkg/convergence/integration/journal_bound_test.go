package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression for #20123: once the journal index bound was reached every
// per-key release was refused, so reservations were never released and the
// "index bound reached" error repeated on every claim release.
func TestReleasePassesJournalIndexBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	now := epoch.Add(time.Hour)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	write := func(e Event) {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"o/r#1", "o/r#2"} {
		r := Reservation{Key: key, Claim: "agent", ClaimGeneration: 7, IntentGeneration: 1, ExpiresAt: now.Add(time.Hour)}
		write(Event{Version: SchemaVersion, ID: "reserve/" + key, At: now, Reservation: &r})
	}
	for n := 2; n < maxJournalRecords; n++ {
		c := Coverage{Repo: "o/r", Base: "v5", From: now.Add(-time.Hour), Through: now, Complete: true, Source: "github-actions"}
		write(Event{Version: SchemaVersion, ID: fmt.Sprintf("coverage/%d", n), At: now, Coverage: &c})
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot(now).Reservations) != 2 {
		t.Fatal("reservations not replayed")
	}
	c := Coverage{Repo: "o/r", Base: "v5", From: now.Add(-time.Hour), Through: now, Complete: true, Source: "github-actions"}
	if err := s.Append(Event{Version: SchemaVersion, ID: "coverage/over", At: now, Coverage: &c}); err == nil {
		t.Fatal("observation admitted past the journal index bound")
	}
	offset := s.offset
	if err := s.Append(Event{Version: SchemaVersion, ID: "release/o/r#9/1", At: now, Release: "o/r#9", ReleaseClaim: "agent", ReleaseGeneration: 7}); err != nil {
		t.Fatal("no-op release failed at the bound:", err)
	}
	if s.offset != offset {
		t.Fatal("no-op release grew the journal past the index bound")
	}
	for n, key := range []string{"o/r#1", "o/r#2"} {
		if err := s.Append(Event{Version: SchemaVersion, ID: fmt.Sprintf("release/%s/%d", key, n), At: now, Release: key, ReleaseClaim: "agent", ReleaseGeneration: 7}); err != nil {
			t.Fatal("release refused at the journal index bound:", err)
		}
	}
	if len(s.Snapshot(now).Reservations) != 0 {
		t.Fatal("reservations retained after release")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal("journal with releases past the index bound did not replay:", err)
	}
	if len(reopened.Snapshot(now).Reservations) != 0 {
		t.Fatal("released reservations replayed as held")
	}
}
