package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIssueWeekPathValidation(t *testing.T) {
	root := t.TempDir()
	got, err := IssueWeekPath(root, "Owner/Name", "2026-W05")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Owner", "Name", "issues", "2026-W05.json"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	for _, repo := range []string{"", "noslash", "/n", "o/", "../n", "o/..", "./n", "o/a/b", `o\x/n`} {
		if _, err := IssueWeekPath(root, repo, "2026-W05"); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
	}
	for _, week := range []string{"", "2026W05", "2026-W5", "2026-W00", "2026-W54", "2025-W53", "abcd-W01", "2026-Wxx", "2026-X05"} {
		if _, err := IssueWeekPath(root, "o/r", week); err == nil {
			t.Errorf("week %q accepted", week)
		}
	}
	if _, err := IssueWeekPath(root, "o/r", "2020-W53"); err != nil {
		t.Errorf("2020-W53 should be valid: %v", err)
	}
}

func TestIssueWeekRoundTrip(t *testing.T) {
	root := t.TempDir()
	from := time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)
	in := IssueWeek{
		Week:        "2026-W05",
		Opened:      map[string]int{AuthorHuman: 2, AuthorAgent: 1},
		Closed:      3,
		CoveredFrom: from,
		CoveredTo:   from.AddDate(0, 0, 7),
		Events:      []IssueEvent{{At: from.Add(time.Hour), Action: "opened", Kind: AuthorHuman}},
	}
	if err := WriteIssueWeek(root, "o/r", in); err != nil {
		t.Fatal(err)
	}
	if err := WriteIssueWeek(root, "o/r", in); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	out, err := ReadIssueWeek(root, "o/r", "2026-W05")
	if err != nil {
		t.Fatal(err)
	}
	if out.Closed != 3 || out.Opened[AuthorHuman] != 2 || len(out.Events) != 1 || !out.CoveredTo.Equal(in.CoveredTo) {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if err := WriteIssueWeek(root, "bad", in); err == nil {
		t.Error("invalid repo written")
	}
	if _, err := ReadIssueWeek(root, "o/r", "2026-W06"); err == nil {
		t.Error("missing week read")
	}
}

func TestReadIssueWeekRejectsInvalid(t *testing.T) {
	root := t.TempDir()
	from := time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)
	good := IssueWeek{Week: "2026-W05", Opened: map[string]int{}, CoveredFrom: from, CoveredTo: from.Add(time.Hour)}
	cases := map[string]func(*IssueWeek){
		"nil opened":      func(w *IssueWeek) { w.Opened = nil },
		"negative closed": func(w *IssueWeek) { w.Closed = -1 },
		"negative kind":   func(w *IssueWeek) { w.Opened = map[string]int{AuthorBot: -1} },
		"empty window":    func(w *IssueWeek) { w.CoveredTo = w.CoveredFrom },
		"week mismatch":   func(w *IssueWeek) { w.Week = "2026-W06" },
	}
	for name, mutate := range cases {
		w := good
		mutate(&w)
		if err := WriteIssueWeek(root, "o/r", IssueWeek{Week: "2026-W05", Opened: map[string]int{}, CoveredFrom: from, CoveredTo: from.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		path, _ := IssueWeekPath(root, "o/r", "2026-W05")
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadIssueWeek(root, "o/r", "2026-W05"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	path, _ := IssueWeekPath(root, "o/r", "2026-W05")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIssueWeek(root, "o/r", "2026-W05"); err == nil {
		t.Error("malformed JSON accepted")
	}
}

func TestIssueWeekCovers(t *testing.T) {
	from := time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)
	w := IssueWeek{Week: "2026-W05", CoveredFrom: from, CoveredTo: from.AddDate(0, 0, 8)}
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"zero", time.Time{}, false},
		{"before", from.Add(-time.Nanosecond), false},
		{"inclusive start", from, true},
		{"inside", from.Add(time.Hour), true},
		{"different week", from.AddDate(0, 0, 7), false},
		{"exclusive end", w.CoveredTo, false},
		{"same instant in another zone", from.In(time.FixedZone("west", -8*60*60)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Covers(tc.at); got != tc.want {
				t.Errorf("Covers(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
	// Check the exclusive end within the ISO week, independently of the week gate.
	w.CoveredTo = from.Add(time.Hour)
	if w.Covers(w.CoveredTo) {
		t.Error("coverage end accepted")
	}
	if got := IssueWeekKey(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)); got != "2020-W53" {
		t.Errorf("ISO year boundary = %q, want 2020-W53", got)
	}
}

func TestLoadIssueSeries(t *testing.T) {
	root := t.TempDir()
	from := time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)
	events := []IssueEvent{
		{At: from, Action: IssueActionOpened, Kind: AuthorHuman},
		{At: from.Add(time.Hour), Action: IssueActionOpened, Kind: AuthorAgent},
		{At: from.Add(2 * time.Hour), Action: IssueActionOpened, Kind: AuthorBot},
		{At: from.Add(3 * time.Hour), Action: IssueActionClosed},
	}
	for _, tc := range []struct {
		repo   string
		events []IssueEvent
	}{
		{"o/populated", events},
		{"other/empty", []IssueEvent{}},
		{"o/missing-events", nil},
		{"o/unknown-action", []IssueEvent{{At: from, Action: "reopened"}}},
		{"o/unknown-kind", []IssueEvent{{At: from, Action: IssueActionOpened, Kind: "unknown"}}},
		{"o/outside-window", append(append([]IssueEvent{}, events...), IssueEvent{At: from.AddDate(0, 0, 7), Action: IssueActionClosed})},
	} {
		w := IssueWeek{
			Week: "2026-W05", Opened: map[string]int{},
			CoveredFrom: from, CoveredTo: from.AddDate(0, 0, 7), Events: tc.events,
		}
		if err := WriteIssueWeek(root, tc.repo, w); err != nil {
			t.Fatal(err)
		}
	}
	// One unreadable snapshot must not hide valid weeks from other repositories.
	path, err := IssueWeekPath(root, "o/broken", "2026-W05")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadIssueSeries(root)
	if len(got) != 2 {
		t.Fatalf("loaded %d weeks, want populated and observed-empty only: %+v", len(got), got)
	}
	byRepo := make(map[string]IssueWeek)
	for _, w := range got {
		byRepo[w.Repo] = w.IssueWeek
	}
	if w, ok := byRepo["o/populated"]; !ok || len(w.Events) != len(events) || !w.Events[0].At.Equal(from) {
		t.Errorf("populated week lost: %+v", w)
	}
	if w, ok := byRepo["other/empty"]; !ok || w.Events == nil || len(w.Events) != 0 {
		t.Errorf("observed empty week lost: %+v", w)
	}
}

func TestLoadIssueSeriesUnavailable(t *testing.T) {
	for _, root := range []string{filepath.Join(t.TempDir(), "missing"), "["} {
		if got := LoadIssueSeries(root); len(got) != 0 {
			t.Errorf("LoadIssueSeries(%q) = %+v, want no weeks", root, got)
		}
	}
}
