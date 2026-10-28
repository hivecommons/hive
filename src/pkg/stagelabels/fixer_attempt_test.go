package stagelabels

import (
	"testing"
	"time"
)

// The fixer-queue record belongs to one head: a new head resets it
// (hivecommons/hive#15036).
func TestFixerQueueRecordResetsOnNewHead(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := Key("acme/widget", 7)
	s.Observe(key, AdoptedByHive, "")
	s.MarkAdoptedInPlace(key, "aaa", "grace", now)
	s.MarkFixerQueued(key, "aaa")
	rec, _ := s.Get(key)
	if !rec.FixerQueuedUnattempted("aaa", now.Add(time.Minute)) || rec.FixerQueuedUnattempted("aaa", now.Add(FixerQueueFreshness+time.Minute)) {
		t.Fatalf("queued freshness: %+v", rec)
	}
	s.MarkFixerAttempted(key, "aaa")
	rec, _ = s.Get(key)
	if rec.FixerAttemptedSince("aaa").IsZero() || rec.FixerQueuedUnattempted("aaa", now) {
		t.Fatalf("attempt not recorded: %+v", rec)
	}
	s.MarkAdoptedHead(key, "bbb")
	s.MarkFixerQueued(key, "bbb")
	rec, _ = s.Get(key)
	if !rec.FixerAttemptedSince("bbb").IsZero() || !rec.FixerQueuedUnattempted("bbb", now) {
		t.Fatalf("new head must reset the attempt: %+v", rec)
	}
}
