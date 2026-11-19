package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestUploadStore(t *testing.T, now *time.Time) *UploadStore {
	t.Helper()
	s := NewUploadStore(filepath.Join(t.TempDir(), "uploads"))
	s.Now = func() time.Time { return *now }
	return s
}

func putTestUpload(t *testing.T, s *UploadStore, kind string, body string) UploadMeta {
	t.Helper()
	id, err := s.NewID()
	if err != nil {
		t.Fatal(err)
	}
	m := UploadMeta{ID: id, Kind: kind, Origin: UploadOriginFile, Filename: "f.md", Title: "F", Size: int64(len(body)), AddedBy: "ana", AddedAt: s.now()}
	if err := s.Put(m, []byte(body), "text:"+body, 0); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUploadStorePermissionsAndRead(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s := newTestUploadStore(t, &now)
	m := putTestUpload(t, s, UploadKindMarkdown, "hello")
	for _, p := range []string{s.Dir, filepath.Join(s.Dir, m.ID)} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != uploadDirMode {
			t.Fatalf("%s mode = %v, %v", p, info.Mode().Perm(), err)
		}
	}
	for _, f := range []string{uploadMetaFile, uploadOriginalFile, uploadTextFile} {
		info, err := os.Stat(filepath.Join(s.Dir, m.ID, f))
		if err != nil || info.Mode().Perm() != uploadFileMode {
			t.Fatalf("%s mode = %v, %v", f, info.Mode().Perm(), err)
		}
	}
	got, err := s.Get(m.ID)
	if err != nil || got.Title != "F" || got.AddedBy != "ana" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if text, err := s.Text(m.ID); err != nil || text != "text:hello" {
		t.Fatalf("Text = %q, %v", text, err)
	}
	if !s.HasOriginal(m.ID) {
		t.Fatal("original missing")
	}
	// The same id twice is refused, never overwritten.
	if err := s.Put(m, []byte("x"), "x", 0); err == nil {
		t.Fatal("duplicate id accepted")
	}
	for _, bad := range []string{"../etc", "..", "", "20261116t100000-ZZZZZZZZ", "x/y"} {
		if _, err := s.Get(bad); !errors.Is(err, ErrUploadNotFound) {
			t.Errorf("Get(%q) = %v", bad, err)
		}
		if _, err := s.Text(bad); !errors.Is(err, ErrUploadNotFound) {
			t.Errorf("Text(%q) = %v", bad, err)
		}
		if s.HasOriginal(bad) {
			t.Errorf("HasOriginal(%q)", bad)
		}
	}
	if err := s.Put(UploadMeta{ID: "../x"}, nil, "", 0); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("bad id Put = %v", err)
	}
}

func TestUploadStoreListNewestFirstAndQuota(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s := newTestUploadStore(t, &now)
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %v, %v", list, err)
	}
	a := putTestUpload(t, s, UploadKindText, "aaaa")
	now = now.Add(time.Minute)
	b := putTestUpload(t, s, UploadKindText, "bbbb")
	// A stray directory and a malformed entry are skipped.
	_ = os.MkdirAll(filepath.Join(s.Dir, "not-an-id"), 0o700)
	_ = os.MkdirAll(filepath.Join(s.Dir, "20261116t100000-00000000"), 0o700)
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	used, err := s.Usage()
	if err != nil || used <= 0 {
		t.Fatalf("Usage = %d, %v", used, err)
	}
	id, _ := s.NewID()
	err = s.Put(UploadMeta{ID: id}, []byte("0123456789"), "0123456789", used+5)
	var ue *UploadError
	if !errors.As(err, &ue) || ue.Code != UploadErrQuota {
		t.Fatalf("over quota = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, id)); !os.IsNotExist(err) {
		t.Fatal("a refused upload left a directory behind")
	}
	if err := s.Put(UploadMeta{ID: id}, []byte("01"), "", used+1<<20); err != nil {
		t.Fatalf("under quota = %v", err)
	}
}

func TestUploadStoreRetentionSweep(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s := newTestUploadStore(t, &now)
	doc := putTestUpload(t, s, UploadKindMarkdown, "doc")
	media := putTestUpload(t, s, UploadKindAudio, "audio")
	now = now.Add(6 * hoursPerDay * time.Hour)
	// Documents are kept when document retention is 0; media goes after 5 days.
	n, err := s.SweepRetention(5, 0)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if s.HasOriginal(media.ID) || !s.HasOriginal(doc.ID) {
		t.Fatal("media original must go, document original must stay")
	}
	if text, err := s.Text(media.ID); err != nil || text != "text:audio" {
		t.Fatalf("extracted text must stay: %q, %v", text, err)
	}
	if m, _ := s.Get(media.ID); m.OriginalDeletedAt == nil {
		t.Fatal("OriginalDeletedAt not recorded")
	}
	// A second sweep has nothing left; document retention removes docs.
	if n, err := s.SweepRetention(5, 0); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
	if n, err := s.SweepRetention(5, 30); err != nil || n != 0 {
		t.Fatalf("young doc = %d, %v", n, err)
	}
	if n, err := s.SweepRetention(5, 3); err != nil || n != 1 || s.HasOriginal(doc.ID) {
		t.Fatalf("doc retention = %d, %v", n, err)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KiB", 25 << 20: "25.0 MiB", 2 << 30: "2.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
