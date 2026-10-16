package originlabel

import (
	"reflect"
	"sort"
	"testing"
)

func TestWithEnforcesExactlyOneOriginLabel(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		origin string
		want   []string
	}{
		{"nil input", nil, Agent, []string{Agent}},
		{"empty input", []string{}, Review, []string{Review}},
		{"appends when absent", []string{"kind/bug", "agent/scanner"}, Agent, []string{"kind/bug", "agent/scanner", Agent}},
		{"already present once", []string{"nit", Review}, Review, []string{"nit", Review}},
		{"retired name takes the slot", []string{"from-review", "nit"}, Review, []string{Review, "nit"}},
		{"retired plus new never doubles", []string{"from-review", Review, "nit"}, Review, []string{Review, "nit"}},
		{"foreign origin replaced", []string{Advisory, "hive/advisory"}, SelfReport, []string{SelfReport, "hive/advisory"}},
		{"agent-supplied origin overridden", []string{"origin/reviewer", "kind/bug"}, Agent, []string{Agent, "kind/bug"}},
		{"case duplicates fold", []string{"Nit", "nit", "FROM-REVIEW"}, Review, []string{"Nit", Review}},
		{"blanks skipped", []string{"", "  ", "nit"}, Upstream, []string{"nit", Upstream}},
		{"non-origin origin adds nothing", []string{"nit", Review}, "hive/advisory", []string{"nit"}},
	}
	for _, tc := range cases {
		got := With(tc.in, tc.origin)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: With(%v, %q) = %v, want %v", tc.name, tc.in, tc.origin, got, tc.want)
		}
		if n := countOrigin(got); Is(tc.origin) && n != 1 {
			t.Errorf("%s: %d origin labels, want exactly 1", tc.name, n)
		}
	}
}

func TestWithDoesNotMutateInput(t *testing.T) {
	in := []string{"from-review", "nit"}
	_ = With(in, Review)
	if !reflect.DeepEqual(in, []string{"from-review", "nit"}) {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestIsCanonicalOf(t *testing.T) {
	if !Is("origin/review") || !Is(" Origin/Agent ") || Is("origin/") || Is("agent/reviewer") || Is("from-review") {
		t.Fatal("Is misclassified")
	}
	if got, ok := Canonical("From-Review"); !ok || got != Review {
		t.Fatalf("Canonical(from-review) = %q,%v", got, ok)
	}
	if got, ok := Canonical("nit"); ok || got != "nit" {
		t.Fatalf("Canonical(nit) = %q,%v", got, ok)
	}
	if Of([]string{"nit", Audit}) != Audit || Of([]string{"nit"}) != "" {
		t.Fatal("Of wrong")
	}
}

func TestVocabularyIsCompleteAndDescribed(t *testing.T) {
	want := []string{Agent, Review, Advisory, Recommendations, SelfReport, Feedback, Upstream, Audit, Dashboard}
	got := append([]string(nil), All...)
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("All = %v, want %v", got, want)
	}
	for _, l := range All {
		if !Is(l) {
			t.Errorf("%q is not an origin label", l)
		}
		if _, ok := Descriptions[l]; !ok {
			t.Errorf("%q has no description", l)
		}
	}
	for retired, to := range Retired {
		if !Is(to) || Is(retired) {
			t.Errorf("Retired[%q]=%q malformed", retired, to)
		}
	}
	if Color != "8250df" {
		t.Fatalf("Color = %q", Color)
	}
}

func countOrigin(labels []string) int {
	n := 0
	for _, l := range labels {
		if Is(l) {
			n++
		}
	}
	return n
}
