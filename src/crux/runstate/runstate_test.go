package runstate

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := 0
	s.NewID = func() string { ids++; return "run_" + strings.Repeat("0", 11) + string(rune('a'+ids-1)) }
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { now = now.Add(time.Second); return now }
	return s, dir
}

func TestCreateGetAndIndex(t *testing.T) {
	s, dir := newStore(t)
	rec, created, err := s.Create(Record{Owner: "operator", Detail: json.RawMessage(`{"x":1}`)})
	if err != nil || !created {
		t.Fatalf("Create: %v created=%v", err, created)
	}
	if rec.ID == "" || rec.Status != StatusAccepted || rec.Kind != KindRun || rec.CreatedAt.IsZero() {
		t.Fatalf("record not filled in: %+v", rec)
	}
	got, err := s.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.Owner != "operator" || string(got.Detail) != `{"x":1}` {
		t.Fatalf("Get = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(Dir(dir), rec.ID, RecordName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if _, err := s.Get("../escape"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get invalid id = %v", err)
	}
	entries := readIndex(t, dir)
	if len(entries) != 1 || entries[0].ID != rec.ID || entries[0].Status != StatusAccepted {
		t.Fatalf("index = %+v", entries)
	}
}

func TestUpdateTimestampsAndTerminalGuard(t *testing.T) {
	s, dir := newStore(t)
	rec, _, _ := s.Create(Record{})
	run, err := s.Update(rec.ID, func(r *Record) error { r.Status = StatusRunning; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if run.StartedAt == nil || run.FinishedAt != nil {
		t.Fatalf("running timestamps = %+v", run)
	}
	done, err := s.Update(rec.ID, func(r *Record) error {
		r.Status = StatusCompleted
		r.Reason = "ok"
		r.ID = "forged"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.ID != rec.ID || done.FinishedAt == nil || !done.FinishedAt.After(*done.StartedAt) || done.Reason != "ok" {
		t.Fatalf("completed = %+v", done)
	}
	if _, err := s.Update(rec.ID, func(r *Record) error { r.Status = StatusRunning; return nil }); err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("leaving terminal: %v", err)
	}
	// Reason may still be amended on a terminal record.
	if _, err := s.Update(rec.ID, func(r *Record) error { r.Reason = "ok; late note"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(rec.ID, func(r *Record) error { r.Status = "bogus"; return nil }); err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("invalid status: %v", err)
	}
	boom := errors.New("boom")
	if _, err := s.Update(rec.ID, func(r *Record) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn error: %v", err)
	}
	if _, err := s.Update("missing", func(r *Record) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	statuses := []string{}
	for _, e := range readIndex(t, dir) {
		statuses = append(statuses, e.Status)
	}
	if want := "accepted running completed completed"; strings.Join(statuses, " ") != want {
		t.Fatalf("index statuses = %v", statuses)
	}
}

func TestIdempotencyAcrossReopen(t *testing.T) {
	s, dir := newStore(t)
	key := DeriveIdempotencyKey("operator", "k1")
	if key == "" || key == DeriveIdempotencyKey("other", "k1") || DeriveIdempotencyKey("operator", " ") != "" {
		t.Fatalf("DeriveIdempotencyKey scoping broken")
	}
	first, created, err := s.Create(Record{IdempotencyKey: key})
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if _, err := s.Update(first.ID, func(r *Record) error { r.Status = StatusFailed; return nil }); err != nil {
		t.Fatal(err)
	}
	again, created, err := s.Create(Record{IdempotencyKey: key})
	if err != nil || created || again.ID != first.ID || again.Status != StatusFailed {
		t.Fatalf("replay = %+v created=%v err=%v", again, created, err)
	}
	other, created, _ := s.Create(Record{IdempotencyKey: DeriveIdempotencyKey("operator", "k2")})
	if !created || other.ID == first.ID {
		t.Fatalf("different key reused record")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	replay, created, err := reopened.Create(Record{IdempotencyKey: key})
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("after reopen = %+v created=%v err=%v", replay, created, err)
	}
}

func TestListAndReconcile(t *testing.T) {
	s, dir := newStore(t)
	a, _, _ := s.Create(Record{})
	b, _, _ := s.Create(Record{})
	c, _, _ := s.Create(Record{})
	s.Update(b.ID, func(r *Record) error { r.Status = StatusRunning; return nil })
	s.Update(c.ID, func(r *Record) error { r.Status = StatusCompleted; return nil })
	// A directory with artifacts but no record is skipped, not an error.
	if err := os.MkdirAll(filepath.Join(Dir(dir), "run_orphan"), 0o770); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != a.ID || list[1].ID != b.ID || list[2].ID != c.ID {
		t.Fatalf("List = %+v", list)
	}
	changed, err := s.Reconcile("restarted")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || changed[0].ID != a.ID || changed[1].ID != b.ID {
		t.Fatalf("Reconcile changed %+v", changed)
	}
	for _, id := range []string{a.ID, b.ID} {
		rec, _ := s.Get(id)
		if rec.Status != StatusUnfinished || rec.Reason != "restarted" || rec.FinishedAt == nil {
			t.Fatalf("%s = %+v", id, rec)
		}
	}
	if rec, _ := s.Get(c.ID); rec.Status != StatusCompleted {
		t.Fatalf("completed record touched: %+v", rec)
	}
	if again, _ := s.Reconcile("x"); len(again) != 0 {
		t.Fatalf("second reconcile changed %+v", again)
	}
	empty := &Store{dir: filepath.Join(t.TempDir(), "none"), Now: time.Now, NewID: newID}
	if list, err := empty.List(); err != nil || list != nil {
		t.Fatalf("missing dir List = %v, %v", list, err)
	}
}

func TestCorruptRecordIsAnError(t *testing.T) {
	s, dir := newStore(t)
	rec, _, _ := s.Create(Record{})
	if err := os.WriteFile(filepath.Join(Dir(dir), rec.ID, RecordName), []byte("{"), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(rec.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt Get = %v", err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("corrupt List succeeded")
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open over a corrupt record succeeded")
	}
}

func TestOpenFailsOnUnwritableWorkDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Fatal("Open under a file succeeded")
	}
}

func TestStatusSets(t *testing.T) {
	for _, st := range []string{StatusCompleted, StatusFailed, StatusCancelled, StatusUnfinished, StatusUnsupported} {
		if !Terminal(st) || !Valid(st) {
			t.Errorf("%s should be terminal and valid", st)
		}
	}
	for _, st := range []string{StatusAccepted, StatusRunning, StatusWaiting} {
		if Terminal(st) || !Valid(st) {
			t.Errorf("%s should be valid and not terminal", st)
		}
	}
	if Valid("") || Valid("done") {
		t.Error("unknown statuses accepted")
	}
	if !validID(newID()) || validID("") || validID(strings.Repeat("a", maxIDLen+1)) || validID("A") {
		t.Error("validID")
	}
}

func readIndex(t *testing.T, workDir string) []IndexEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(Dir(workDir), IndexName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []IndexEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e IndexEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}
