package nous

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCampaignEnsureMigratesExistingDataToCampaignOne(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "sub", DefaultFileName))
	if _, ok := st.Active(); ok {
		t.Fatal("empty store must have no active campaign")
	}
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c, created, err := st.Ensure("", first)
	if err != nil || !created {
		t.Fatalf("Ensure = %+v created=%v err=%v", c, created, err)
	}
	if c.ID != 1 || !c.StartedAt.Equal(first) || c.Archived() || c.Status() != "active" {
		t.Fatalf("campaign 1 = %+v", c)
	}
	// Second Ensure is a no-op.
	again, created, err := st.Ensure("other", time.Now())
	if err != nil || created || again.ID != 1 || again.Question != "" {
		t.Fatalf("second Ensure = %+v created=%v err=%v", again, created, err)
	}
	// Persisted with the schema version.
	data, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil || f.Version != FileVersion || len(f.Campaigns) != 1 {
		t.Fatalf("file = %s err=%v", data, err)
	}
}

func TestCampaignStartArchivesPreviousAndRestartsBaseline(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := st.Ensure("", t0); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(72 * time.Hour)
	outcome := Outcome{Snapshots: 2688, Experiments: 2, Principles: 1, Results: []map[string]interface{}{{"id": "exp-1", "verdict": "improved"}}}
	prev, next, err := st.Start("Can we cut CI time by 20%?", now, outcome)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.ID != 1 || !prev.Archived() || !prev.EndedAt.Equal(now) || prev.Outcome.Snapshots != 2688 || len(prev.Outcome.Results) != 1 {
		t.Fatalf("archived = %+v", prev)
	}
	if next.ID != 2 || next.Question != "Can we cut CI time by 20%?" || !next.StartedAt.Equal(now) || next.Archived() {
		t.Fatalf("next = %+v", next)
	}
	act, ok := st.Active()
	if !ok || act.ID != 2 {
		t.Fatalf("active = %+v ok=%v", act, ok)
	}
	arch := st.Archived()
	if len(arch) != 1 || arch[0].ID != 1 || arch[0].Status() != "archived" {
		t.Fatalf("archived list = %+v", arch)
	}

	// Reload from disk in a fresh store: the same shape comes back.
	re := NewStore(st.Path())
	all := re.All()
	if len(all) != 2 || all[0].ID != 1 || all[1].ID != 2 || !all[0].Archived() || all[1].Archived() {
		t.Fatalf("reloaded = %+v", all)
	}
	// A third campaign gets ID 3 and archives 2.
	prev, next, err = re.Start("Another question", now.Add(time.Hour), Outcome{})
	if err != nil || prev == nil || prev.ID != 2 || next.ID != 3 {
		t.Fatalf("third start prev=%+v next=%+v err=%v", prev, next, err)
	}
}

func TestCampaignStartWithoutActiveHasNoPrevious(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	prev, next, err := st.Start("q", time.Now(), Outcome{})
	if err != nil || prev != nil || next.ID != 1 {
		t.Fatalf("prev=%+v next=%+v err=%v", prev, next, err)
	}
}

func TestCampaignSetQuestion(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	if err := st.SetQuestion("q"); err != ErrNoActive {
		t.Fatalf("SetQuestion on empty store err = %v, want ErrNoActive", err)
	}
	if _, _, err := st.Ensure("", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetQuestion("  trimmed  "); err != nil {
		t.Fatal(err)
	}
	act, _ := NewStore(st.Path()).Active()
	if act.Question != "trimmed" || act.ID != 1 {
		t.Fatalf("active after SetQuestion = %+v", act)
	}
}

func TestCampaignLoadErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(path)
	if _, ok := st.Active(); ok {
		t.Fatal("corrupt file must not yield an active campaign")
	}
	if st.Archived() != nil || st.All() != nil {
		t.Fatal("corrupt file must yield nil lists")
	}
	if _, _, err := st.Ensure("", time.Now()); err == nil {
		t.Fatal("Ensure on corrupt file must error")
	}
	if _, _, err := st.Start("q", time.Now(), Outcome{}); err == nil {
		t.Fatal("Start on corrupt file must error")
	}
	if err := st.SetQuestion("q"); err == nil {
		t.Fatal("SetQuestion on corrupt file must error")
	}
}

func TestQuestionChanged(t *testing.T) {
	cases := []struct {
		prev, next string
		want       bool
	}{
		{"", "", false},
		{"Cut CI time", "", false},
		{"", "Cut CI time", true},
		{"Cut CI time", "cut  ci   time", false},
		{"Cut CI time", "Cut CI time by 20%", true},
		{"a", " A ", false},
	}
	for _, c := range cases {
		if got := QuestionChanged(c.prev, c.next); got != c.want {
			t.Errorf("QuestionChanged(%q, %q) = %v, want %v", c.prev, c.next, got, c.want)
		}
	}
}
