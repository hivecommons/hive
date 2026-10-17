package nous

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

// Editing the hypothesis or tags is not material; changing the research
// question (beyond whitespace/case) or the repo set is, because it
// invalidates the baseline and anything proposed so far.
func TestCampaignMaterialChange(t *testing.T) {
	c := Campaign{Question: "Cut CI time", Hypothesis: "shard", Scope: Scope{Repos: []string{"acme/api", "acme/web"}, Tags: []string{"ci"}}}
	cases := []struct {
		name string
		p    Patch
		want bool
	}{
		{"empty", Patch{}, false},
		{"hypothesis only", Patch{Hypothesis: strPtr("split the slowest package")}, false},
		{"same question different whitespace/case", Patch{Question: strPtr("  cut   CI  time ")}, false},
		{"new question", Patch{Question: strPtr("Fewer flaky tests")}, true},
		{"same repos reordered, new tags", Patch{Scope: &Scope{Repos: []string{"ACME/WEB", "acme/api"}, Tags: []string{"ci", "speed"}}}, false},
		{"narrowed scope", Patch{Scope: &Scope{Repos: []string{"acme/api"}}}, true},
		{"widened scope", Patch{Scope: &Scope{Repos: []string{"acme/api", "acme/web", "acme/docs"}}}, true},
		{"to hive-wide", Patch{Scope: &Scope{}}, true},
	}
	for _, tc := range cases {
		if got := c.MaterialChange(tc.p); got != tc.want {
			t.Errorf("%s: MaterialChange = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (Campaign{}).MaterialChange(Patch{Scope: &Scope{}}) {
		t.Error("hive-wide → hive-wide must not be material")
	}
	if (Campaign{}).VersionNumber() != 1 || (Campaign{Version: 3}).VersionNumber() != 3 {
		t.Error("VersionNumber must default to 1")
	}
}

// Restart archives the edited campaign as version N (frozen outcome, history
// kept) and starts version N+1 with the edited fields, a fresh StartedAt and
// restarted_from pointing back. A restart whose scope collides with another
// running campaign is refused before anything is written.
func TestCampaignRestartKeepsLineage(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), DefaultFileName))
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a, err := st.Create("Cut CI time", "shard", Scope{Repos: []string{"acme/api"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("Web flakes", "", Scope{Repos: []string{"acme/web"}}, t0); err != nil {
		t.Fatal(err)
	}

	// Scope collision with #2 → *Conflict, store untouched.
	_, _, err = st.Restart(a.ID, Patch{Scope: &Scope{Repos: []string{"acme/api", "ACME/WEB"}}}, t0.Add(time.Hour), Outcome{})
	var conf *Conflict
	if !errors.As(err, &conf) || conf.With.ID != 2 {
		t.Fatalf("conflicting restart err = %v", err)
	}
	if all := st.All(); len(all) != 2 || !all[0].Running() {
		t.Fatalf("refused restart must not change the store: %+v", all)
	}

	// Paused campaigns restart too.
	if _, err := st.Pause(a.ID); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(2 * time.Hour)
	prev, next, err := st.Restart(a.ID, Patch{Question: strPtr(" Cut CI time by half "), Hypothesis: strPtr("shard + cache")}, t1, Outcome{Snapshots: 40, Experiments: 2})
	if err != nil {
		t.Fatal(err)
	}
	if prev.ID != 1 || !prev.Archived() || prev.EndedAt == nil || !prev.EndedAt.Equal(t1) || prev.Outcome.Snapshots != 40 || prev.VersionNumber() != 1 {
		t.Fatalf("archived v1 = %+v", prev)
	}
	if next.ID != 3 || next.Version != 2 || next.RestartedFrom != 1 || next.Question != "Cut CI time by half" || next.Hypothesis != "shard + cache" || !next.StartedAt.Equal(t1) || !next.Running() || len(next.Scope.Repos) != 1 || next.Scope.Repos[0] != "acme/api" {
		t.Fatalf("v2 = %+v", next)
	}
	// Restarting again chains the version number.
	_, v3, err := st.Restart(next.ID, Patch{Scope: &Scope{Repos: []string{"acme/api", "acme/docs"}}}, t1.Add(time.Hour), Outcome{})
	if err != nil {
		t.Fatal(err)
	}
	if v3.Version != 3 || v3.RestartedFrom != 3 || len(v3.Scope.Repos) != 2 {
		t.Fatalf("v3 = %+v", v3)
	}
	// Archived versions cannot be restarted; unknown ids are not found.
	if _, _, err := st.Restart(1, Patch{}, t1, Outcome{}); !errors.Is(err, ErrArchived) {
		t.Fatalf("restart archived err = %v", err)
	}
	if _, _, err := st.Restart(99, Patch{}, t1, Outcome{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restart unknown err = %v", err)
	}
	// Lineage survives a reload.
	re := NewStore(st.Path())
	got, ok := re.Get(v3.ID)
	if !ok || got.Version != 3 || got.RestartedFrom != 3 {
		t.Fatalf("reloaded = %+v ok=%v", got, ok)
	}
	if arch := re.Archived(); len(arch) != 2 || arch[0].ID != 3 || arch[1].ID != 1 {
		t.Fatalf("Archived = %+v", arch)
	}
}
