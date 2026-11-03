package nous

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hivecommons/hive#12532: several campaigns may run at once when their
// scopes do not overlap; overlapping scopes are refused with *Conflict.
func TestCampaignCreateConcurrentWithDisjointScopes(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a, err := st.Create("Cut CI time", "shard tests", Scope{Repos: []string{"acme/api", " acme/api ", ""}, Intervention: "ci", Tags: []string{"ci", "CI"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || a.Status() != StatusActive || !a.Running() || len(a.Scope.Repos) != 1 || len(a.Scope.Tags) != 1 || a.Hypothesis != "shard tests" {
		t.Fatalf("a = %+v", a)
	}
	b, err := st.Create("Fewer flaky tests", "", Scope{Repos: []string{"acme/web"}}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != 2 || !b.Running() {
		t.Fatalf("b = %+v", b)
	}
	if open := st.Open(); len(open) != 2 || open[0].ID != 1 || open[1].ID != 2 {
		t.Fatalf("Open = %+v", open)
	}
	// Legacy single-campaign view is the oldest running campaign.
	if act, ok := st.Active(); !ok || act.ID != 1 {
		t.Fatalf("Active = %+v ok=%v", act, ok)
	}

	// Shared repo → conflict naming the other campaign and the shared repo.
	_, err = st.Create("Overlap", "", Scope{Repos: []string{"ACME/API", "acme/docs"}}, t0)
	var conf *Conflict
	if !errors.As(err, &conf) || conf.With.ID != 1 || len(conf.Shared) != 1 || conf.Shared[0] != "ACME/API" {
		t.Fatalf("overlapping create err = %v (%+v)", err, conf)
	}
	if conf.Error() == "" {
		t.Fatal("Conflict must describe itself")
	}
	// Hive-wide conflicts with anything running.
	_, err = st.Create("Everything", "", Scope{}, t0)
	if !errors.As(err, &conf) || conf.With.ID != 1 || conf.Shared != nil {
		t.Fatalf("hive-wide create err = %v", err)
	}
	if c, ok := st.Conflict(Scope{Repos: []string{"acme/web"}}, 0); !ok || c.ID != 2 {
		t.Fatalf("Conflict probe = %+v ok=%v", c, ok)
	}
	if _, ok := st.Conflict(Scope{Repos: []string{"acme/web"}}, 2); ok {
		t.Fatal("Conflict must ignore the excluded id")
	}
	if _, ok := st.Conflict(Scope{Repos: []string{"acme/other"}}, 0); ok {
		t.Fatal("disjoint scope must not conflict")
	}
	// Three campaigns never got created.
	if all := st.All(); len(all) != 2 {
		t.Fatalf("All = %+v", all)
	}
}

func TestCampaignPauseReleasesScopeAndResumeRechecks(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a, _ := st.Create("A", "", Scope{Repos: []string{"acme/api"}}, t0)
	p, err := st.Pause(a.ID)
	if err != nil || p.Status() != StatusPaused || !p.Paused() || p.Running() || p.Archived() {
		t.Fatalf("Pause = %+v err=%v", p, err)
	}
	if _, ok := st.Active(); ok {
		t.Fatal("a paused campaign is not the legacy active campaign")
	}
	if open := st.Open(); len(open) != 1 || open[0].Status() != StatusPaused {
		t.Fatalf("Open after pause = %+v", open)
	}
	b, err := st.Create("B", "", Scope{Repos: []string{"acme/api"}}, t0)
	if err != nil {
		t.Fatalf("paused scope must be free: %v", err)
	}
	var conf *Conflict
	if _, err := st.Resume(a.ID); !errors.As(err, &conf) || conf.With.ID != b.ID {
		t.Fatalf("Resume into a taken scope err = %v", err)
	}
	if _, err := st.Pause(b.ID); err != nil {
		t.Fatal(err)
	}
	r, err := st.Resume(a.ID)
	if err != nil || !r.Running() {
		t.Fatalf("Resume = %+v err=%v", r, err)
	}
	// Resuming a running campaign is a no-op success.
	if r, err := st.Resume(a.ID); err != nil || !r.Running() {
		t.Fatalf("Resume running = %+v err=%v", r, err)
	}
	if _, err := st.Pause(99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Pause unknown err = %v", err)
	}
}

func TestCampaignArchiveAndUpdate(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a, _ := st.Create("A", "", Scope{Repos: []string{"acme/api"}}, t0)
	b, _ := st.Create("B", "", Scope{Repos: []string{"acme/web"}}, t0)

	q, h := "  A edited ", " because "
	u, err := st.Update(a.ID, Patch{Question: &q, Hypothesis: &h})
	if err != nil || u.Question != "A edited" || u.Hypothesis != "because" || len(u.Scope.Repos) != 1 {
		t.Fatalf("Update = %+v err=%v", u, err)
	}
	// Widening into b's repo is refused while both run.
	var conf *Conflict
	if _, err := st.Update(a.ID, Patch{Scope: &Scope{Repos: []string{"acme/api", "acme/web"}}}); !errors.As(err, &conf) || conf.With.ID != b.ID {
		t.Fatalf("Update widening err = %v", err)
	}
	// A paused campaign may hold any scope; it is re-checked on resume.
	if _, err := st.Pause(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(a.ID, Patch{Scope: &Scope{Repos: []string{"acme/web"}}}); err != nil {
		t.Fatalf("Update paused err = %v", err)
	}
	if _, err := st.Resume(a.ID); !errors.As(err, &conf) {
		t.Fatalf("Resume after widening err = %v", err)
	}

	now := t0.Add(48 * time.Hour)
	arch, err := st.Archive(b.ID, now, Outcome{Snapshots: 192, Experiments: 1})
	if err != nil || !arch.Archived() || arch.Status() != StatusArchived || !arch.EndedAt.Equal(now) || arch.Outcome.Snapshots != 192 {
		t.Fatalf("Archive = %+v err=%v", arch, err)
	}
	for _, err := range []error{
		func() error { _, err := st.Archive(b.ID, now, Outcome{}); return err }(),
		func() error { _, err := st.Pause(b.ID); return err }(),
		func() error { _, err := st.Update(b.ID, Patch{Question: &q}); return err }(),
	} {
		if !errors.Is(err, ErrArchived) {
			t.Fatalf("mutating an archived campaign err = %v, want ErrArchived", err)
		}
	}
	if _, err := st.Archive(99, now, Outcome{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Archive unknown err = %v", err)
	}
	if _, err := st.Update(99, Patch{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update unknown err = %v", err)
	}
	if got, ok := st.Get(b.ID); !ok || !got.Archived() {
		t.Fatalf("Get = %+v ok=%v", got, ok)
	}
	if _, ok := st.Get(99); ok {
		t.Fatal("Get unknown must report false")
	}
	if arch := st.Archived(); len(arch) != 1 || arch[0].ID != b.ID {
		t.Fatalf("Archived = %+v", arch)
	}
}

// A version-1 file (one active campaign, no status/scope) loads as: active
// campaign → first running card (hive-wide), earlier ones → archived; the
// next save writes version 2 with explicit statuses.
func TestCampaignMigratesVersionOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	v1 := `{"version":1,"campaigns":[
	  {"id":1,"question":"","started_at":"2026-09-01T00:00:00Z","ended_at":"2026-10-01T00:00:00Z","outcome":{"snapshots":2688,"experiments":0,"principles":0}},
	  {"id":2,"question":"Cut CI time","started_at":"2026-10-01T00:00:00Z","outcome":{"snapshots":0,"experiments":0,"principles":0}}
	]}`
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(path)
	all := st.All()
	if len(all) != 2 || all[0].Status() != StatusArchived || all[1].Status() != StatusActive || !all[1].Scope.HiveWide() {
		t.Fatalf("migrated = %+v", all)
	}
	if open := st.Open(); len(open) != 1 || open[0].ID != 2 {
		t.Fatalf("Open = %+v", open)
	}
	// The migrated campaign is hive-wide, so a second one conflicts until
	// it is narrowed or paused.
	var conf *Conflict
	if _, err := st.Create("Another", "", Scope{Repos: []string{"acme/api"}}, time.Now()); !errors.As(err, &conf) || conf.With.ID != 2 {
		t.Fatalf("create next to hive-wide err = %v", err)
	}
	if _, err := st.Update(2, Patch{Scope: &Scope{Repos: []string{"acme/web"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("Another", "", Scope{Repos: []string{"acme/api"}}, time.Now()); err != nil {
		t.Fatalf("create after narrowing err = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil || f.Version != FileVersion {
		t.Fatalf("saved file version = %d err=%v", f.Version, err)
	}
	for _, c := range f.Campaigns {
		if c.State == "" {
			t.Fatalf("campaign %d saved without an explicit status", c.ID)
		}
	}
}

// Legacy Start (goals PUT / campaign/start) swaps only the oldest running
// campaign and hands its scope to the replacement.
func TestCampaignStartKeepsOtherRunningCampaigns(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a, _ := st.Create("A", "", Scope{Repos: []string{"acme/api"}}, t0)
	b, _ := st.Create("B", "", Scope{Repos: []string{"acme/web"}}, t0)
	prev, next, err := st.Start("A2", t0.Add(time.Hour), Outcome{Snapshots: 4})
	if err != nil || prev == nil || prev.ID != a.ID || !prev.Archived() || next.ID != 3 || len(next.Scope.Repos) != 1 || next.Scope.Repos[0] != "acme/api" {
		t.Fatalf("Start prev=%+v next=%+v err=%v", prev, next, err)
	}
	if got, _ := st.Get(b.ID); !got.Running() {
		t.Fatalf("B must keep running: %+v", got)
	}
	// With no running campaign, Start's hive-wide default is still guarded
	// against a paused→nothing situation: pause everything, Start succeeds.
	if _, err := st.Pause(next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pause(b.ID); err != nil {
		t.Fatal(err)
	}
	if prev, next, err := st.Start("fresh", t0.Add(2*time.Hour), Outcome{}); err != nil || prev != nil || next.ID != 4 {
		t.Fatalf("Start with only paused prev=%+v next=%+v err=%v", prev, next, err)
	}
}

func TestScopeOverlaps(t *testing.T) {
	cases := []struct {
		a, b   Scope
		shared int
		want   bool
	}{
		{Scope{}, Scope{}, 0, true},
		{Scope{}, Scope{Repos: []string{"x/y"}}, 0, true},
		{Scope{Repos: []string{"x/y"}}, Scope{Repos: []string{"X/Y"}}, 1, true},
		{Scope{Repos: []string{"x/y"}}, Scope{Repos: []string{"x/z"}}, 0, false},
		{Scope{Repos: []string{"x/y", "x/z"}}, Scope{Repos: []string{"x/z", "x/w"}}, 1, true},
	}
	for _, c := range cases {
		shared, got := c.a.Overlaps(c.b)
		if got != c.want || len(shared) != c.shared {
			t.Errorf("Overlaps(%v, %v) = %v,%v want %d,%v", c.a, c.b, shared, got, c.shared, c.want)
		}
	}
}

func TestCampaignMutationsOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(path)
	if _, err := st.Create("q", "", Scope{}, time.Now()); err == nil {
		t.Fatal("Create on corrupt file must error")
	}
	if _, err := st.Pause(1); err == nil {
		t.Fatal("Pause on corrupt file must error")
	}
	if _, err := st.Archive(1, time.Now(), Outcome{}); err == nil {
		t.Fatal("Archive on corrupt file must error")
	}
	if _, err := st.Update(1, Patch{}); err == nil {
		t.Fatal("Update on corrupt file must error")
	}
	if st.Open() != nil {
		t.Fatal("Open on corrupt file must be nil")
	}
	if _, ok := st.Get(1); ok {
		t.Fatal("Get on corrupt file must report false")
	}
	if _, ok := st.Conflict(Scope{}, 0); ok {
		t.Fatal("Conflict on corrupt file must report false")
	}
}
