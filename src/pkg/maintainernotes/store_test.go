package maintainernotes

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(start time.Time) func() time.Time {
	t := start
	return func() time.Time {
		t = t.Add(time.Minute)
		return t
	}
}

func TestStoreAppendListDeletePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes", "maintainer-notes.jsonl")
	s := New(path)
	s.now = fixedClock(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))

	a, err := s.Add("HiveCommons/Hive", 42, "alice", "  waiting on legal  ")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if a.Repo != "hivecommons/hive" || a.Text != "waiting on legal" || a.ID == "" {
		t.Fatalf("add normalised badly: %+v", a)
	}
	b, err := s.Add("hivecommons/hive", 42, "bob", "owner wants this after v6")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.Add("hivecommons/hive", 7, "alice", "other item"); err != nil {
		t.Fatalf("add: %v", err)
	}

	got := s.List("hivecommons/hive", 42)
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("list = %+v, want [a b]", got)
	}
	if all := s.All(); len(all) != 3 {
		t.Fatalf("all = %d, want 3", len(all))
	}

	// Author check: bob may not delete alice's note; alice may; an owner
	// may delete anyone's.
	if _, err := s.Delete(a.ID, "bob", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob deleting alice's note: err=%v, want ErrForbidden", err)
	}
	if _, err := s.Delete(a.ID, "Alice", false); err != nil {
		t.Fatalf("alice deleting own note: %v", err)
	}
	if _, err := s.Delete(b.ID, "owner", true); err != nil {
		t.Fatalf("owner deleting bob's note: %v", err)
	}
	if _, err := s.Delete(b.ID, "owner", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err=%v, want ErrNotFound", err)
	}
	if got := s.List("hivecommons/hive", 42); len(got) != 0 {
		t.Fatalf("after deletes list = %+v, want empty", got)
	}

	// Replay from disk: the tombstones hide both deleted notes, the third
	// survives.
	re := New(path)
	if got := re.List("hivecommons/hive", 42); len(got) != 0 {
		t.Fatalf("replayed list = %+v, want empty", got)
	}
	if got := re.List("hivecommons/hive", 7); len(got) != 1 || got[0].Text != "other item" {
		t.Fatalf("replayed #7 = %+v", got)
	}
}

func TestStoreValidation(t *testing.T) {
	s := New("")
	cases := []struct {
		repo   string
		number int
		author string
		text   string
		want   error
	}{
		{"hivecommons", 1, "a", "x", ErrBadItem},
		{"hivecommons/hive", 0, "a", "x", ErrBadItem},
		{"hivecommons/hive", 1, "", "x", ErrNoAuthor},
		{"hivecommons/hive", 1, "a", "   ", ErrEmpty},
		{"hivecommons/hive", 1, "a", strings.Repeat("é", MaxTextLen+1), ErrTooLong},
	}
	for _, c := range cases {
		if _, err := s.Add(c.repo, c.number, c.author, c.text); !errors.Is(err, c.want) {
			t.Errorf("Add(%q,%d,%q,len %d): err=%v want %v", c.repo, c.number, c.author, len(c.text), err, c.want)
		}
	}
	if _, err := s.Add("hivecommons/hive", 1, "a", strings.Repeat("é", MaxTextLen)); err != nil {
		t.Errorf("exactly MaxTextLen runes must be accepted: %v", err)
	}
}

func TestKickSectionCap(t *testing.T) {
	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	var notes []Note
	for i := 0; i < 6; i++ {
		notes = append(notes, Note{Author: "m" + string(rune('a'+i)), Text: strings.Repeat("x", 300), CreatedAt: base.Add(time.Duration(i) * time.Hour)})
	}
	out := KickSection(notes, KickSectionCap)
	if !strings.HasPrefix(out, KickSectionHeader+"\n") {
		t.Fatalf("section must open with %q:\n%s", KickSectionHeader, out)
	}
	// Newest first.
	if !strings.Contains(strings.SplitN(out, "\n", 3)[2], "- @mf (2026-11-08)") {
		t.Fatalf("newest note must lead:\n%s", out)
	}
	body := out[strings.Index(out, "- @"):]
	if i := strings.Index(body, "… and "); i >= 0 {
		body = body[:i]
	}
	if len(body) > KickSectionCap {
		t.Fatalf("notes body %d bytes exceeds cap %d", len(body), KickSectionCap)
	}
	if !strings.Contains(out, "more note(s) on the dashboard") {
		t.Fatalf("overflow must be counted:\n%s", out)
	}
	if KickSection(nil, 0) != "" {
		t.Fatal("no notes must render nothing")
	}
}
