package hubactivity

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func item(repo string, n int, stage string, stageN int, mods ...string) Item {
	since := t0.Add(-time.Duration(n) * time.Minute)
	return Item{Repo: repo, Number: n, Kind: "pr", Title: fmt.Sprintf("item %d", n), Stage: stage, StageNumber: stageN, Since: &since, Mods: mods}
}

func numbers(items []Item) []int {
	out := make([]int, len(items))
	for i, it := range items {
		out[i] = it.Number
	}
	return out
}

func TestBuild_DefaultCapIs100AndDisclosesTruncation(t *testing.T) {
	var in []Item
	for i := 1; i <= 143; i++ {
		in = append(in, item("o/r", i, "actionable", 10))
	}

	d := Build(in, BuildOptions{Now: t0})

	if len(d.Items) != DefaultMaxItems {
		t.Fatalf("items = %d, want %d", len(d.Items), DefaultMaxItems)
	}
	if d.Truncated != 43 {
		t.Fatalf("truncated = %d, want 43", d.Truncated)
	}
	if d.Totals == nil || d.Totals.Items != 143 || d.Totals.ByStage["actionable"] != 143 {
		t.Fatalf("totals = %+v, want 143 actionable", d.Totals)
	}
}

func TestBuild_MaxItemsIsClampedToCeiling(t *testing.T) {
	var in []Item
	for i := 1; i <= MaxItemsCeiling+50; i++ {
		in = append(in, item("o/r", i, "actionable", 10))
	}

	d := Build(in, BuildOptions{Now: t0, MaxItems: MaxItemsCeiling * 10})

	if len(d.Items) != MaxItemsCeiling || d.Truncated != 50 {
		t.Fatalf("items=%d truncated=%d, want %d/50", len(d.Items), d.Truncated, MaxItemsCeiling)
	}
}

func TestBuild_UrgencyOrdering(t *testing.T) {
	in := []Item{
		item("o/r", 1, "pr_open", 50),
		item("o/r", 2, "ci_review", 70),
		item("o/r", 3, "actionable", 10, ModStalled),
		item("o/r", 4, "actionable", 10, ModNeedsDecision),
		item("o/r", 5, "triage", 15, ModNeedsHuman),
	}

	d := Build(in, BuildOptions{Now: t0, MaxItems: 4})

	// needs-human/needs-decision first (furthest stage within the rank: 5 is
	// at 15, 4 at 10), then stalled, then furthest stage.
	got := numbers(d.Items)
	want := []int{5, 4, 3, 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if d.Truncated != 1 {
		t.Fatalf("truncated = %d, want 1", d.Truncated)
	}
}

func TestBuild_PrivateItemsDroppedButCounted(t *testing.T) {
	priv := item("o/secret", 9, "ci_review", 70)
	priv.Private = true
	in := []Item{item("o/r", 1, "actionable", 10), priv}

	d := Build(in, BuildOptions{Now: t0})

	for _, it := range d.Items {
		if it.Private || it.Repo == "o/secret" {
			t.Fatalf("private item leaked into digest: %+v", it)
		}
	}
	if d.Totals.Items != 2 || d.Totals.ByStage["ci_review"] != 1 {
		t.Fatalf("totals must count private items: %+v", d.Totals)
	}
	if d.Truncated != 0 {
		t.Fatalf("a private drop is not truncation, got %d", d.Truncated)
	}
}

func TestBuild_SharePrivateKeepsPrivateItems(t *testing.T) {
	priv := item("o/secret", 9, "ci_review", 70)
	priv.Private = true

	d := Build([]Item{priv}, BuildOptions{Now: t0, SharePrivate: true})

	if len(d.Items) != 1 || !d.Items[0].Private {
		t.Fatalf("share_private should keep the item marked private: %+v", d.Items)
	}
}

func TestBuild_ETagStableAcrossTimeAndChangesWithContent(t *testing.T) {
	in := []Item{item("o/r", 1, "actionable", 10)}

	a := Build(in, BuildOptions{Now: t0})
	b := Build(in, BuildOptions{Now: t0.Add(time.Hour)})
	in[0].Stage = "ci_review"
	c := Build(in, BuildOptions{Now: t0})

	if a.ETag == "" || !strings.HasPrefix(a.ETag, `W/"`) {
		t.Fatalf("etag %q is not a weak etag", a.ETag)
	}
	if a.ETag != b.ETag {
		t.Fatalf("etag must not depend on generated_at: %q vs %q", a.ETag, b.ETag)
	}
	if a.ETag == c.ETag {
		t.Fatalf("etag must change when content changes")
	}
}

func TestBuild_DoesNotMutateInput(t *testing.T) {
	in := []Item{item("o/r", 2, "actionable", 10), item("o/r", 1, "ci_review", 70)}
	in[0].Title = strings.Repeat("x", MaxTitleRunes+10)

	Build(in, BuildOptions{Now: t0})

	if in[0].Number != 2 || len(in[0].Title) != MaxTitleRunes+10 {
		t.Fatalf("Build mutated its input: %+v", in[0])
	}
}

func TestSanitize_EnforcesHubCeilingAndBounds(t *testing.T) {
	d := Digest{GeneratedAt: t0, ETag: `W/"x"`, Totals: &Totals{Items: 1}}
	for i := 1; i <= MaxItemsCeiling+7; i++ {
		it := item("o/r", i, "actionable", 10)
		it.Title = "a\x00b\nc" + strings.Repeat("é", MaxTitleRunes)
		it.Mods = []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10"}
		d.Items = append(d.Items, it)
	}
	d.Items = append(d.Items, Item{Repo: "", Number: 1}, Item{Repo: "o/r", Number: 0})

	got := Sanitize(d)

	if len(got.Items) != MaxItemsCeiling {
		t.Fatalf("items = %d, want ceiling %d", len(got.Items), MaxItemsCeiling)
	}
	// 7 over the ceiling; the 2 malformed rows are discarded, not counted.
	if got.Truncated != 7 {
		t.Fatalf("truncated = %d, want 7", got.Truncated)
	}
	it := got.Items[0]
	if strings.ContainsAny(it.Title, "\x00\n") {
		t.Fatalf("control characters survived: %q", it.Title)
	}
	if n := len([]rune(it.Title)); n > MaxTitleRunes {
		t.Fatalf("title runes = %d, want <= %d", n, MaxTitleRunes)
	}
	if len(it.Mods) != MaxModsPerItem {
		t.Fatalf("mods = %d, want %d", len(it.Mods), MaxModsPerItem)
	}
	if len(d.Items[0].Mods) != 10 {
		t.Fatalf("Sanitize mutated its input")
	}
}

func TestSanitize_ValidatesTokensAndDedupes(t *testing.T) {
	d := Digest{ETag: `W/"x"`, Truncated: -5, Totals: &Totals{Items: -1, ByStage: map[string]int{"a": maxCount}, ByModifier: map[string]int{"m": -3}}}
	d.Items = []Item{
		{Repo: "o/r", Number: 1, Kind: "pr", AuthorKind: "hive", Stage: "ci_review", Mods: []string{"stalled", "<b>", "Bad Mod"}, StageNumber: -9},
		{Repo: "O/R", Number: 1, Kind: "pr"},                 // duplicate of o/r#1
		{Repo: "javascript:alert(1)", Number: 2, Kind: "pr"}, // not owner/name
		{Repo: "o/../x", Number: 3},                          // not owner/name
		{Repo: "o/r", Number: 4, Kind: "<script>", AuthorKind: "robot", Stage: "<b>x</b>"},
	}

	got := Sanitize(d)

	if len(got.Items) != 2 {
		t.Fatalf("items = %+v, want o/r#1 and o/r#4", got.Items)
	}
	first, last := got.Items[0], got.Items[1]
	if first.Kind != "pr" || first.AuthorKind != "hive" || first.Stage != "ci_review" || fmt.Sprint(first.Mods) != "[stalled]" || first.StageNumber != 0 {
		t.Fatalf("valid tokens mangled: %+v", first)
	}
	if last.Kind != "" || last.AuthorKind != "" || last.Stage != "" {
		t.Fatalf("invalid tokens must be cleared: %+v", last)
	}
	if got.Truncated != 0 || got.Totals.Items != 0 || got.Totals.ByModifier["m"] != 0 || got.Totals.ByStage["a"] != maxCount {
		t.Fatalf("integers not clamped: %+v %+v", got.Truncated, got.Totals)
	}
}

func TestCleanText_DropsLineAndParagraphSeparators(t *testing.T) {
	if got := cleanText("a\u2028b\u2029c\u202ed", 50); got != "abcd" {
		t.Fatalf("cleanText = %q, want abcd", got)
	}
}

func TestDigest_IsUnchangedMarker(t *testing.T) {
	full := Build([]Item{item("o/r", 1, "actionable", 10)}, BuildOptions{Now: t0})
	marker := Digest{GeneratedAt: t0, ETag: full.ETag}

	if full.IsUnchangedMarker() {
		t.Fatalf("a full digest is not an unchanged marker")
	}
	if !marker.IsUnchangedMarker() {
		t.Fatalf("etag+generated_at only must read as unchanged marker")
	}
	if (Digest{}).IsUnchangedMarker() {
		t.Fatalf("an empty digest carries no etag and is not a marker")
	}
}
