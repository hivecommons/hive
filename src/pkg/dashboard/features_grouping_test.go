package dashboard

import (
	"os"
	"strings"
	"testing"
)

// The Features tab groups settings by drawing a rule between sections. The
// Review Gate section had grown to eleven controls spanning three unrelated
// concerns, which read as one undifferentiated list. These tests pin the
// grouping that fixed it, because the failure mode is silent: a new control
// appended in the wrong place looks fine in a diff and wrong on screen.
func featuresMarkup(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	return string(b)
}

// Sub-groups must group with whitespace and a quiet label, never a rule. The
// rule is what separates one feature from the next; spending it inside a
// feature makes one feature look like several.
func TestConfigSubgroupGroupsWithoutARule(t *testing.T) {
	html := featuresMarkup(t)

	i := strings.Index(html, ".config-subgroup {")
	if i < 0 {
		t.Fatal("the .config-subgroup style is missing — sub-group headings have nothing to render as")
	}
	rule := html[i : strings.Index(html[i:], "}")+i]

	for _, banned := range []string{"border-top", "border-bottom", "border:"} {
		if strings.Contains(rule, banned) {
			t.Errorf(".config-subgroup declares %q; a sub-group must not draw a rule, which is reserved for the section boundary", banned)
		}
	}
	if !strings.Contains(rule, "margin") {
		t.Error(".config-subgroup declares no margin, so it cannot group by whitespace")
	}
}

// The reviewer controls must stay one unit: every sub-group heading the Review
// Gate section declares has to sit inside that section, not drift into a
// neighbour.
func TestReviewGateKeepsItsSubgroups(t *testing.T) {
	html := featuresMarkup(t)

	gate := strings.Index(html, ">Review Gate ")
	if gate < 0 {
		t.Fatal("Review Gate section heading not found")
	}
	// The section ends where the next bordered section begins.
	rest := html[gate:]
	next := strings.Index(rest, "border-top:1px solid var(--border)")
	if next < 0 {
		t.Fatal("no section boundary after Review Gate")
	}
	section := rest[:next]

	for _, want := range []string{`<div class="config-subgroup">Merge gate</div>`, `<div class="config-subgroup">Reviewers</div>`} {
		if !strings.Contains(section, want) {
			t.Errorf("the Review Gate section is missing %s — its controls read as one flat list again", want)
		}
	}

	// Ordering matters: the gate controls come before the reviewer controls.
	if strings.Index(section, "Merge gate") > strings.Index(section, ">Reviewers<") {
		t.Error("the Merge gate sub-group must precede Reviewers")
	}
}

// Recommendations is not a merge gate — it opens an issue and gates nothing —
// so it gets its own section rather than a tail appended to the reviewer unit.
func TestRecommendationsIsItsOwnSection(t *testing.T) {
	html := featuresMarkup(t)

	gate := strings.Index(html, ">Review Gate ")
	rec := strings.Index(html, ">Merge Recommendations ")
	if rec < 0 {
		t.Fatal("the Merge Recommendations section heading is missing")
	}
	if rec < gate {
		t.Fatal("Merge Recommendations must follow the Review Gate section")
	}

	// Every recommendations control must live after that heading, which is
	// what proves they were moved out of the reviewer unit rather than
	// merely re-labelled in place.
	for _, id := range []string{"rec-enabled-switch", "rec-repos-input", "rec-minready-input"} {
		at := strings.Index(html, id)
		if at < 0 {
			t.Errorf("control %s disappeared", id)
			continue
		}
		if at < rec {
			t.Errorf("control %s sits above the Merge Recommendations heading, so it is still inside the Review Gate section", id)
		}
	}

	// It must be a real section, i.e. preceded by the section rule.
	head := html[:rec]
	lastRule := strings.LastIndex(head, "border-top:1px solid var(--border)")
	if lastRule < gate {
		t.Error("Merge Recommendations is not preceded by a section rule, so it will not read as a separate feature")
	}
}
