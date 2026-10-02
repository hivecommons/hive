package upstreamwatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// errFake is the failure the memStore injects on demand.
var errFake = errors.New("fake store failure")

// memStore is the in-memory fake the later slices' tests use in place of a
// FileStore. It copies nothing: callers mutate the State they loaded, which
// is exactly how the poller uses a real store.
type memStore struct {
	mu    sync.Mutex
	state State
	err   error
}

func (m *memStore) Load() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if m.state == nil {
		return State{}, nil
	}
	return m.state, nil
}

func (m *memStore) Save(state State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.state = state
	return nil
}

var _ Store = (*memStore)(nil)
var _ Store = (*FileStore)(nil)

func TestFileStore_MissingFileIsEmpty(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "nope.json"))
	state, err := s.Load()
	if err != nil || state == nil || len(state) != 0 {
		t.Fatalf("Load = %v, %v; want empty non-nil state", state, err)
	}
}

func TestFileStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "upstream-watch.json")
	s := NewFileStore(path)
	if s.Path() != path {
		t.Fatalf("Path = %q", s.Path())
	}
	merged := time.Date(2026, 9, 29, 1, 2, 3, 0, time.UTC)
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)

	in := State{}
	repo := in.Repo("acme/widgets")
	repo.Upstream = "upstream/widgets"
	repo.LastRunAt = now
	repo.Put(Outcome{Ref: RefPR(42), Status: StatusFiled, ItemTime: merged, IssueNumber: 7}, now)
	repo.Dismiss(RefRelease("v1.2.0"), "closed as not planned", now)

	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}

	out, err := NewFileStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	got := out["acme/widgets"]
	if got == nil || got.Upstream != "upstream/widgets" || !got.Watermark.Equal(merged) || !got.LastRunAt.Equal(now) {
		t.Fatalf("round trip = %+v", got)
	}
	rec, ok := got.Record(RefPR(42))
	if !ok || rec.Status != StatusFiled || rec.IssueNumber != 7 || !rec.FiledAt.Equal(now) {
		t.Fatalf("pr record = %+v, ok=%v", rec, ok)
	}
	if !got.Dismissed(RefRelease("v1.2.0")) {
		t.Fatalf("dismissal not persisted: %+v", got.Refs)
	}
}

func TestFileStore_SaveNilWritesEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := NewFileStore(path).Save(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("state file = %q, %v", data, err)
	}
}

func TestFileStore_LoadErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := NewFileStore(write("corrupt.json", "{not json")).Load(); err == nil {
		t.Fatal("corrupt state loaded without error; the watermark would be forgotten")
	}
	if _, err := NewFileStore(write("future.json", `{"version": 99, "repos": {}}`)).Load(); err == nil {
		t.Fatal("future state version loaded without error")
	}
	state, err := NewFileStore(write("nulls.json", `{"version": 1, "repos": {"a/b": null, "c/d": {"watermark": "2026-09-29T00:00:00Z", "refs": {"upstream#1": null, "upstream#2": {"ref": "upstream#2", "status": "filed"}}}}}`)).Load()
	if err != nil || len(state) != 1 {
		t.Fatalf("null entries: %v, %v", state, err)
	}
	repo := state["c/d"]
	if repo == nil || repo.Seen(RefPR(1)) || !repo.Seen(RefPR(2)) {
		t.Fatalf("null ref not dropped: %+v", repo)
	}
	// A repo saved without refs loads with a usable (non-nil) map.
	state, err = NewFileStore(write("norefs.json", `{"version": 1, "repos": {"a/b": {}}}`)).Load()
	if err != nil || state["a/b"].Refs == nil {
		t.Fatalf("missing refs map: %v, %v", state, err)
	}
	// A directory where the file should be is an I/O error, not "empty".
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(filepath.Join(dir, "isdir")).Load(); err == nil {
		t.Fatal("reading a directory succeeded")
	}
}

func TestFileStore_SaveErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Parent "directory" is a regular file: MkdirAll fails.
	if err := NewFileStore(filepath.Join(blocker, "state.json")).Save(nil); err == nil {
		t.Fatal("save under a regular file succeeded")
	}
	// Temp path is a directory: the write fails.
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewFileStore(target).Save(nil); err == nil {
		t.Fatal("save over a directory temp path succeeded")
	}
	// Target is a non-empty directory: the rename fails and the temp file is
	// cleaned up.
	target2 := filepath.Join(dir, "state2.json")
	if err := os.MkdirAll(filepath.Join(target2, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewFileStore(target2).Save(nil); err == nil {
		t.Fatal("rename over a non-empty directory succeeded")
	}
	if _, err := os.Stat(target2 + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file not cleaned up after a failed rename: %v", err)
	}
}

func TestStateRepoCreatesEntry(t *testing.T) {
	s := State{}
	r := s.Repo("acme/widgets")
	if r == nil || s["acme/widgets"] != r {
		t.Fatalf("Repo did not store the new entry: %v", s)
	}
	if again := s.Repo("acme/widgets"); again != r {
		t.Fatal("Repo returned a different entry for the same key")
	}
}

func TestRepoStateWatermarkAdvance(t *testing.T) {
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status RefStatus
		item   time.Time
		start  time.Time
		want   time.Time
	}{
		{name: "filed advances", status: StatusFiled, item: base.Add(time.Hour), want: base.Add(time.Hour)},
		{name: "skipped advances", status: StatusSkipped, item: base.Add(time.Hour), want: base.Add(time.Hour)},
		{name: "ported advances", status: StatusPorted, item: base.Add(time.Hour), want: base.Add(time.Hour)},
		{name: "dismissed does not advance", status: StatusDismissed, item: base.Add(time.Hour), want: time.Time{}},
		{name: "zero item time does not advance", status: StatusFiled, want: time.Time{}},
		{name: "older item does not rewind", status: StatusFiled, item: base, start: base.Add(2 * time.Hour), want: base.Add(2 * time.Hour)},
		{name: "equal item does not rewind", status: StatusFiled, item: base, start: base, want: base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RepoState{Watermark: tt.start}
			r.Put(Outcome{Ref: RefPR(1), Status: tt.status, ItemTime: tt.item}, base)
			if !r.Watermark.Equal(tt.want) {
				t.Fatalf("watermark = %v, want %v", r.Watermark, tt.want)
			}
		})
	}
}

func TestRepoStateDedupeAndDismissal(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	r := &RepoState{}

	if r.Seen(RefPR(1)) || r.Dismissed(RefPR(1)) || r.IssueFor(RefPR(1)) != 0 {
		t.Fatal("empty state reported a known ref")
	}
	// An empty ref is never recorded: it would collide with every other
	// unkeyed write.
	r.Put(Outcome{Status: StatusFiled}, now)
	if len(r.Refs) != 0 {
		t.Fatalf("empty ref recorded: %+v", r.Refs)
	}

	r.Put(Outcome{Ref: RefPR(1), Status: StatusFiled, ItemTime: now, IssueNumber: 11}, now)
	if !r.Seen(RefPR(1)) || r.IssueFor(RefPR(1)) != 11 || r.Dismissed(RefPR(1)) {
		t.Fatalf("filed ref = %+v", r.Refs[RefPR(1)])
	}

	// A dismissal keeps the issue number already recorded for the ref.
	r.Dismiss(RefPR(1), "closed as not planned", now.Add(time.Hour))
	rec, ok := r.Record(RefPR(1))
	if !ok || rec.IssueNumber != 11 || rec.Reason != "closed as not planned" || !r.Dismissed(RefPR(1)) {
		t.Fatalf("dismissed ref = %+v, ok=%v", rec, ok)
	}
	// Dismissal is terminal for dedupe purposes: the ref still reads as seen.
	if !r.Seen(RefPR(1)) {
		t.Fatal("dismissed ref would be resurfaced")
	}

	r.Put(Outcome{Ref: RefRelease("v2.0.0"), Status: StatusSkipped, ItemTime: now, Reason: "no touched file exists in the fork"}, now)
	rec, ok = r.Record(RefRelease("v2.0.0"))
	if !ok || rec.Status != StatusSkipped || rec.IssueNumber != 0 || rec.Reason == "" {
		t.Fatalf("skipped ref = %+v, ok=%v", rec, ok)
	}
}

func TestStateSummary(t *testing.T) {
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	s := State{}
	if _, ok := s.Summary("acme/widgets", 0); ok {
		t.Fatal("summary for an unknown repo reported ok")
	}

	r := s.Repo("acme/widgets")
	r.Upstream = "upstream/widgets"
	r.LastRunAt = base.Add(5 * time.Hour)
	r.Put(Outcome{Ref: RefPR(1), Status: StatusFiled, ItemTime: base, IssueNumber: 11}, base)
	r.Put(Outcome{Ref: RefPR(2), Status: StatusPorted, ItemTime: base.Add(time.Hour), IssueNumber: 12}, base.Add(time.Hour))
	r.Put(Outcome{Ref: RefPR(3), Status: StatusSkipped, ItemTime: base.Add(2 * time.Hour)}, base.Add(2*time.Hour))
	r.Put(Outcome{Ref: RefRelease("v1.0.0"), Status: StatusFiled, ItemTime: base.Add(3 * time.Hour), IssueNumber: 13}, base.Add(3*time.Hour))
	r.Dismiss(RefRelease("v1.0.0"), "upstream/dismissed", base.Add(4*time.Hour))

	sum, ok := s.Summary("acme/widgets", 0)
	if !ok {
		t.Fatal("summary for a known repo reported not ok")
	}
	if sum.Repo != "acme/widgets" || sum.Upstream != "upstream/widgets" || !sum.LastRunAt.Equal(base.Add(5*time.Hour)) {
		t.Fatalf("summary header = %+v", sum)
	}
	if !sum.Watermark.Equal(base.Add(3 * time.Hour)) {
		t.Fatalf("watermark = %v", sum.Watermark)
	}
	if sum.Surfaced != 3 || sum.Ported != 1 || sum.Dismissed != 1 || sum.Skipped != 1 {
		t.Fatalf("counts = %+v", sum)
	}
	if len(sum.Recent) != 4 || sum.Recent[0].Ref != RefRelease("v1.0.0") || sum.Recent[0].IssueNumber != 13 {
		t.Fatalf("recent = %+v", sum.Recent)
	}
	if sum.Recent[3].Ref != RefPR(1) {
		t.Fatalf("recent not newest-first: %+v", sum.Recent)
	}

	// limit caps the recent list.
	sum, _ = s.Summary("acme/widgets", 2)
	if len(sum.Recent) != 2 {
		t.Fatalf("limited recent = %+v", sum.Recent)
	}

	// Equal timestamps fall back to the ref for a stable order.
	s2 := State{}
	r2 := s2.Repo("a/b")
	r2.Put(Outcome{Ref: RefPR(9), Status: StatusFiled, ItemTime: base}, base)
	r2.Put(Outcome{Ref: RefPR(8), Status: StatusFiled, ItemTime: base}, base)
	sum2, _ := s2.Summary("a/b", 0)
	if sum2.Recent[0].Ref != RefPR(8) {
		t.Fatalf("tie not broken by ref: %+v", sum2.Recent)
	}
}

func TestMemStoreRoundTrip(t *testing.T) {
	m := &memStore{}
	state, err := m.Load()
	if err != nil || len(state) != 0 {
		t.Fatalf("Load = %v, %v", state, err)
	}
	state.Repo("a/b").Upstream = "up/b"
	if err := m.Save(state); err != nil {
		t.Fatal(err)
	}
	back, err := m.Load()
	if err != nil || back["a/b"].Upstream != "up/b" {
		t.Fatalf("round trip = %v, %v", back, err)
	}

	m.err = errFake
	if _, err := m.Load(); err == nil {
		t.Fatal("Load ignored the injected error")
	}
	if err := m.Save(state); err == nil {
		t.Fatal("Save ignored the injected error")
	}
}
