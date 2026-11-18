package stagelabels

import (
	"testing"
	"time"
)

// hivecommons/hive#18872: a nudge comment withheld because the hive adopts
// the head anyway is remembered per head, and cleared once it is posted.
func TestStoreNoteNudgeWithheld(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	s.NoteNudgeWithheld("missing", "aaa") // unknown key: no-op
	if _, ok := s.Get("missing"); ok {
		t.Fatal("NoteNudgeWithheld created a record")
	}
	key := Key("acme/widget", 7)
	s.Observe(key, ReturnedToContributor, "conflicting")
	s.NoteNudgeWithheld(key, "aaa")
	if r, _ := s.Get(key); r.NudgeWithheldHead != "aaa" {
		t.Fatalf("withheld not recorded: %+v", r)
	}
	s.NoteNudgeWithheld(key, "aaa") // unchanged: no-op
	s.NoteNudgeWithheld(key, "")
	if r, _ := s.Get(key); r.NudgeWithheldHead != "" {
		t.Fatalf("withheld not cleared: %+v", r)
	}
}
