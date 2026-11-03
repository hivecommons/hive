package dashboard

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The tooltip's trail is built from the payload's history entries, so the
// fields the SPA reads must be the ones the server writes. hivecommons/hive
// #14787 hovered as "Trail: undefined → undefined → undefined → undefined"
// because index.html read h.stage while StageTrackerHop emits number/slug.
func TestStageTrackerTrailFieldsMatchSPA(t *testing.T) {
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	key := stagelabels.Key("hivecommons/hive", 14787)
	store.Observe(key, stagelabels.ReviewPending, "required-check-pending:build-and-test")
	clock = now.Add(10 * time.Minute)
	store.Observe(key, stagelabels.NeedsAuthor, "check-failing:coverage")
	clock = now.Add(20 * time.Minute)
	store.Observe(key, stagelabels.ReviewPending, "label:needs-human")
	clock = now.Add(30 * time.Minute)
	store.Observe(key, stagelabels.NeedsDecision, "parked on a person: needs-human")
	rec, _ := store.Get(key)

	hops := stageTrackerHistory(rec)
	if len(hops) != 4 {
		t.Fatalf("history = %d hops, want 4: %+v", len(hops), hops)
	}
	raw, err := json.Marshal(hops)
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wantNums := []float64{50, 71, 50, 85}
	wantSlugs := []string{"review-pending", "needs-author", "review-pending", "needs-decision"}
	for i, h := range wire {
		if h["number"] != wantNums[i] || h["slug"] != wantSlugs[i] || h["at"] == nil {
			t.Fatalf("hop %d on the wire = %v, want number %v slug %q and an at", i, h, wantNums[i], wantSlugs[i])
		}
		if _, ok := h["stage"]; ok {
			t.Fatalf("hop %d carries a stage field the SPA must not depend on: %v", i, h)
		}
	}

	html := indexHTML(t)
	hopName := jsFunc(t, html, "stageTrackerHopName")
	for _, field := range []string{"h.number", "h.slug"} {
		if !strings.Contains(hopName, field) {
			t.Fatalf("stageTrackerHopName does not read %s — the trail would print undefined:\n%s", field, hopName)
		}
	}
	tip := jsFunc(t, html, "stageTrackerTooltip")
	if !strings.Contains(tip, "stageTrackerTrailText(item)") {
		t.Fatalf("stageTrackerTooltip must build its trail through stageTrackerTrailText:\n%s", tip)
	}
	// No tracker tooltip code may stringify a history hop field directly —
	// that is exactly how "undefined" reached the hover.
	if m := regexp.MustCompile(`String\(h\.(stage|number|slug)\)`).FindString(tip + jsFunc(t, html, "stageTrackerTrailText")); m != "" {
		t.Fatalf("tooltip stringifies a hop field without naming it: %s", m)
	}
}
