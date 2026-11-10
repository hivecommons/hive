package maintainernotes

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestStoreGetDefaultAndReplayEdgeCases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintainer-notes.jsonl")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	// A file as it may be left after crashes and hand edits: a blank line,
	// garbage, a note without an id, a duplicate append, a tombstone for a
	// note nobody has seen, and notes out of order.
	lines := []string{
		"",
		"not json",
		`{"repo":"acme/widget","number":1,"author":"x","text":"no id","created_at":"2026-11-08T12:00:00Z"}`,
		`{"id":"n2","repo":"acme/widget","number":1,"author":"bob","text":"second","created_at":"2026-11-08T13:00:00Z"}`,
		`{"id":"n1","repo":"acme/widget","number":1,"author":"alice","text":"first","created_at":"2026-11-08T12:00:00Z"}`,
		`{"id":"n1","repo":"acme/widget","number":1,"author":"alice","text":"dup","created_at":"2026-11-08T12:00:00Z"}`,
		`{"id":"ghost","repo":"acme/widget","number":1,"author":"a","deleted_at":"2026-11-08T12:00:00Z"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	s.now = fixedClock(at)
	got := s.List("acme/widget", 1)
	if len(got) != 2 || got[0].ID != "n1" || got[0].Text != "first" || got[1].ID != "n2" {
		t.Fatalf("replayed list = %+v, want [n1 first, n2]", got)
	}
	if n, ok := s.Get("n2"); !ok || n.Author != "bob" {
		t.Fatalf("Get(n2) = %+v, %v", n, ok)
	}
	if _, ok := s.Get("ghost"); ok {
		t.Fatal("a tombstoned-before-seen id must not resolve")
	}
	if s.List("not-a-repo", 1) != nil {
		t.Fatal("a malformed repo lists nothing")
	}
	if _, err := s.Delete("missing", "alice", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	// The tombstone names who deleted and keeps the note's identity.
	d, err := s.Delete("n2", " BOB ", false)
	if err != nil || d.ID != "n2" {
		t.Fatalf("author delete (case/space-insensitive): %+v, %v", d, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"deleted_by":"BOB"`) || !strings.Contains(string(data), `"deleted_at":"2026-11-08T12:00:00Z"`) {
		t.Fatalf("tombstone not appended:\n%s", data)
	}
	if New(path).List("acme/widget", 1)[0].ID != "n1" || len(New(path).List("acme/widget", 1)) != 1 {
		t.Fatal("replay must honour the tombstone")
	}

	// Default() is one process-wide store at DefaultPath.
	first := Default()
	if second := Default(); first != second || first.path != DefaultPath {
		t.Fatalf("Default() must be the singleton at %s", DefaultPath)
	}
	if len(newID()) != idBytes*2 {
		t.Fatalf("newID() = %q, want %d hex chars", newID(), idBytes*2)
	}
}

func TestStoreAppendFailureLeavesMemoryUnchanged(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file: MkdirAll fails.
	s := New(filepath.Join(blocker, "notes.jsonl"))
	if _, err := s.Add("acme/widget", 1, "alice", "hello"); err == nil {
		t.Fatal("Add must surface the write error")
	}
	if len(s.All()) != 0 {
		t.Fatal("a failed append must not be remembered")
	}
	// An in-memory note whose tombstone cannot be written stays live.
	m := New("")
	n, err := m.Add("acme/widget", 1, "alice", "hello")
	if err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(blocker, "notes.jsonl")
	if _, err := m.Delete(n.ID, "alice", false); err == nil {
		t.Fatal("Delete must surface the write error")
	}
	if _, ok := m.Get(n.ID); !ok {
		t.Fatal("a note whose tombstone failed to persist must stay live")
	}
}

func TestStoreFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "notes", "maintainer-notes.jsonl")
	s := New(path)
	if _, err := s.Add("acme/widget", 1, "alice", "waiting on legal"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("new notes file mode = %04o, want 0600: the notes are maintainer-private and every agent UID can read /data", got)
	}

	// A file an earlier release left world-readable is tightened on the next write.
	wide := filepath.Join(t.TempDir(), "maintainer-notes.jsonl")
	if err := os.WriteFile(wide, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(wide).Add("acme/widget", 1, "alice", "hello"); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(wide)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("pre-existing 0644 notes file left at %04o after a write, want 0600", got)
	}
}

func TestNormalizeRepo(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{" HiveCommons/Hive ", "hivecommons/hive", true},
		{"hive", "", false},
		{"/hive", "", false},
		{"owner/", "", false},
		{"owner/a/b", "", false},
		{"own er/repo", "", false},
		{"owner/re po", "", false},
	} {
		got, ok := NormalizeRepo(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeRepo(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestKickSectionDefaultCapAndTinyRoom(t *testing.T) {
	at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	notes := []Note{
		{Author: "alice", Text: "line one\nline two", CreatedAt: at},
		{Author: "bob", Text: "héllo wörld, a note that is longer than the room left", CreatedAt: at.Add(time.Minute)},
	}
	// capBytes <= 0 means the default; newlines inside a note fold to spaces.
	out := KickSection(notes, 0)
	if !strings.Contains(out, "- @alice (2026-11-08): line one line two\n") || strings.Contains(out, "more note(s)") {
		t.Fatalf("default cap must type both notes whole:\n%s", out)
	}
	// A cap smaller than the first line cuts it on a rune boundary and
	// counts the rest.
	out = KickSection(notes, 30)
	if !strings.Contains(out, "- @bob (2026-11-08): héll"+elidedTrail) && !strings.Contains(out, "- @bob (2026-11-08): hél"+elidedTrail) {
		t.Fatalf("cut must land on a rune boundary with the ellipsis:\n%s", out)
	}
	if !strings.Contains(out, "… and 1 more note(s) on the dashboard.") {
		t.Fatalf("the unshown note must be counted:\n%s", out)
	}
	// No room at all: header only, every note counted.
	out = KickSection(notes, 2)
	if strings.Contains(out, "- @") || !strings.Contains(out, "… and 2 more note(s)") {
		t.Fatalf("tiny cap:\n%s", out)
	}
}
