package retro

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/timeline"
)

func TestLaneDue(t *testing.T) {
	l := &Lane{interval: time.Hour}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if !l.Due(now) {
		t.Error("never-run lane must be due")
	}
	l.lastRun = now.Add(-30 * time.Minute)
	if l.Due(now) {
		t.Error("lane inside interval must not be due")
	}
	l.lastRun = now.Add(-2 * time.Hour)
	if !l.Due(now) {
		t.Error("lane past interval must be due")
	}
}

func TestMetaBool(t *testing.T) {
	cases := []struct {
		value interface{}
		want  bool
	}{
		{"true", true},
		{"YES", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"", false},
		{true, true},
	}
	for _, tt := range cases {
		b := &beads.Bead{Metadata: map[string]interface{}{"flag": tt.value}}
		if got := metaBool(b, "flag"); got != tt.want {
			t.Errorf("metaBool(%v) = %v, want %v", tt.value, got, tt.want)
		}
	}
	if metaBool(nil, "flag") {
		t.Error("nil bead must be false")
	}
	if metaBool(&beads.Bead{}, "flag") {
		t.Error("missing key must be false")
	}
}

func TestSeverityToPriority(t *testing.T) {
	cases := map[string]beads.Priority{
		"critical": beads.PriorityCritical,
		"HIGH":     beads.PriorityHigh,
		"medium":   beads.PriorityMedium,
		"low":      beads.PriorityLow,
		"unknown":  beads.PriorityMinor,
		"":         beads.PriorityMinor,
	}
	for sev, want := range cases {
		if got := severityToPriority(sev); got != want {
			t.Errorf("severityToPriority(%q) = %v, want %v", sev, got, want)
		}
	}
}

func TestRoundDuration(t *testing.T) {
	if got := roundDuration(42*time.Minute + 29*time.Second); got != 42*time.Minute {
		t.Errorf("sub-hour rounds to minutes, got %v", got)
	}
	if got := roundDuration(5*time.Hour + 31*time.Minute); got != 6*time.Hour {
		t.Errorf("over an hour rounds to hours, got %v", got)
	}
}

func TestApplyPRMetadata(t *testing.T) {
	t.Run("builds pr ref from repo and number", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_repo": "hivecommons/hive", "pr_number": "17"}}
		r := RetroRecord{}
		applyPRMetadata(b, &r)
		if r.PRRef != "hivecommons/hive#17" {
			t.Errorf("PRRef = %q", r.PRRef)
		}
	})
	t.Run("falls back to issue ref repo", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_number": "9"}}
		r := RetroRecord{IssueRef: "hivecommons/hive#3"}
		applyPRMetadata(b, &r)
		if r.PRRef != "hivecommons/hive#9" {
			t.Errorf("PRRef = %q", r.PRRef)
		}
	})
	t.Run("existing pr ref untouched", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_repo": "other/repo", "pr_number": "5"}}
		r := RetroRecord{PRRef: "hivecommons/hive#1"}
		applyPRMetadata(b, &r)
		if r.PRRef != "hivecommons/hive#1" {
			t.Errorf("PRRef = %q", r.PRRef)
		}
	})
	t.Run("merged metadata sets merged state", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_merged": "true"}}
		r := RetroRecord{}
		applyPRMetadata(b, &r)
		if r.PRState != "merged" {
			t.Errorf("PRState = %q", r.PRState)
		}
	})
	t.Run("closed metadata sets closed state", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_closed_at": "2026-08-01T00:00:00Z"}}
		r := RetroRecord{}
		applyPRMetadata(b, &r)
		if r.PRState != "closed" {
			t.Errorf("PRState = %q", r.PRState)
		}
	})
	t.Run("existing state untouched", func(t *testing.T) {
		b := &beads.Bead{Metadata: map[string]interface{}{"pr_merged": "true"}}
		r := RetroRecord{PRState: "closed"}
		applyPRMetadata(b, &r)
		if r.PRState != "closed" {
			t.Errorf("PRState = %q", r.PRState)
		}
	})
}

func TestPRRefFromAttrs(t *testing.T) {
	cases := []struct {
		name     string
		attrs    map[string]string
		issueRef string
		want     string
	}{
		{"empty attrs", nil, "hivecommons/hive#1", ""},
		{"no repo anywhere", map[string]string{"pr_number": "4"}, "", ""},
		{"pr_number with explicit repo", map[string]string{"pr_repo": "hivecommons/hive", "pr_number": "4"}, "", "hivecommons/hive#4"},
		{"hash-prefixed number key", map[string]string{"number": "#12"}, "hivecommons/hive#1", "hivecommons/hive#12"},
		{"pr key fallback", map[string]string{"pr": "8"}, "hivecommons/hive#1", "hivecommons/hive#8"},
		{"non-numeric values", map[string]string{"pr_number": "abc"}, "hivecommons/hive#1", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := prRefFromAttrs(tt.attrs, tt.issueRef); got != tt.want {
				t.Errorf("prRefFromAttrs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitPRRefAndRepoPart(t *testing.T) {
	if repo, n, ok := splitPRRef(" hivecommons/hive#42 "); !ok || repo != "hivecommons/hive" || n != 42 {
		t.Errorf("splitPRRef = %q %d %v", repo, n, ok)
	}
	for _, bad := range []string{"", "#42", "hivecommons/hive#", "hivecommons/hive#0", "a#b#c", "plain"} {
		if _, _, ok := splitPRRef(bad); ok {
			t.Errorf("splitPRRef(%q) must not parse", bad)
		}
	}
	if got := repoPart("hivecommons/hive#5"); got != "hivecommons/hive" {
		t.Errorf("repoPart = %q", got)
	}
	if got := repoPart("nonsense"); got != "" {
		t.Errorf("repoPart on invalid ref = %q, want empty", got)
	}
}

func TestCanonicalIssueRef(t *testing.T) {
	if got := canonicalIssueRef(" hivecommons/hive#12 "); got != "hivecommons/hive#12" {
		t.Errorf("canonicalIssueRef = %q", got)
	}
	for _, bad := range []string{"#12", "no-hash", "a#1#2", ""} {
		if got := canonicalIssueRef(bad); got != "" {
			t.Errorf("canonicalIssueRef(%q) = %q, want empty", bad, got)
		}
	}
}

func TestCompletedAt(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if got := completedAt(nil); !got.IsZero() {
		t.Errorf("nil bead completedAt = %v", got)
	}
	b := &beads.Bead{Metadata: map[string]interface{}{"closed_at": base.Format(time.RFC3339)}}
	if got := completedAt(b); !got.Equal(base) {
		t.Errorf("metadata closed_at = %v, want %v", got, base)
	}
	open := &beads.Bead{Status: beads.StatusOpen}
	if got := completedAt(open); !got.IsZero() {
		t.Errorf("open bead completedAt = %v, want zero", got)
	}

	store := newStore(t, "completed-at")
	created, err := store.Create("closed bead", beads.TypeChore, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := completedAt(closed); got.IsZero() {
		t.Error("closed bead must report a completion time from ClosedAt")
	}
}

func TestOpenDuplicate(t *testing.T) {
	advisory := newStore(t, "advisory")
	l := &Lane{advisoryStore: advisory}

	if l.openDuplicate("nothing filed yet") {
		t.Error("empty store must have no duplicate")
	}
	created, err := advisory.Create("repeat offender", beads.TypeAdvisory, beads.PriorityMedium, "retro", "")
	if err != nil {
		t.Fatal(err)
	}
	if !l.openDuplicate("repeat offender") {
		t.Error("open advisory with same title must be a duplicate")
	}
	if l.openDuplicate("different title") {
		t.Error("different title must not be a duplicate")
	}
	if err := advisory.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	if l.openDuplicate("repeat offender") {
		t.Error("closed advisory must not count as a duplicate")
	}
}

func TestApplyTimelineIgnoresMissingRefs(t *testing.T) {
	r := RetroRecord{}
	applyTimeline(nil, &r)
	if r.KicksReceived != 0 {
		t.Error("nil timeline must be a no-op")
	}
	tl := timeline.NewStore()
	applyTimeline(tl, &r)
	if r.KicksReceived != 0 {
		t.Error("empty issue ref must be a no-op")
	}
}
