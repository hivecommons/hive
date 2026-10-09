package dashboard

import (
	"strings"
	"testing"
)

func TestReviewSubsectionsAreIndependentCollapsibleTiles(t *testing.T) {
	html := indexHTML(t)
	card := reviewSectionHTML(t, html)
	for _, tc := range []struct {
		id      string
		heading string
		bodyID  string
		countID string
	}{
		{id: "review-pipeline-subsection", heading: "review-pipeline-heading", bodyID: "review-pipeline-body", countID: "review-pipeline-count"},
		{id: "review-queue-subsection", heading: "review-queue-heading", bodyID: "review-queue-body", countID: "review-queue-count"},
	} {
		sectionStart := strings.Index(card, `id="`+tc.id+`"`)
		if sectionStart < 0 {
			t.Fatalf("Review card missing %s", tc.id)
		}
		nextSection := strings.Index(card[sectionStart+len(tc.id):], `<section class="review-subsection"`)
		section := card[sectionStart:]
		if nextSection >= 0 {
			section = card[sectionStart : sectionStart+len(tc.id)+nextSection]
		}
		for _, want := range []string{
			`class="dash-card" data-section-card-shell="1"`,
			`id="` + tc.heading + `"`,
			`data-action="toggleSection"`,
			`data-arg0="` + tc.id + `"`,
			`aria-expanded="true"`,
			`aria-controls="` + tc.bodyID + `"`,
			`class="section-chevron"`,
			`data-section-help="` + tc.id + `"`,
			`id="` + tc.countID + `"`,
			`id="` + tc.id + `-summary"`,
		} {
			if !strings.Contains(section, want) {
				t.Fatalf("%s tile is missing %q", tc.id, want)
			}
		}
		if strings.Contains(section, `data-dashboard-grip`) {
			t.Fatalf("%s must not include the top-level dashboard drag grip", tc.id)
		}
		if count := strings.Index(section, `id="`+tc.countID+`"`); count < strings.Index(section, `id="`+tc.heading+`"`) || strings.Index(section, `id="`+tc.bodyID+`"`) < count {
			t.Fatalf("%s count span must live in the tile header", tc.id)
		}
	}
}

func TestReviewSubsectionTileCollapseMetaAndSummaries(t *testing.T) {
	html := indexHTML(t)
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for _, id := range []string{"review-pipeline-subsection", "review-queue-subsection"} {
		entry := dashboardSectionConfigEntry(t, config, id)
		if !strings.Contains(entry, "summary: '—'") {
			t.Fatalf("%s collapse config must start with an unloaded summary fallback: %s", id, entry)
		}
	}
	for _, want := range []string{
		"function reviewPipelineSubsectionSummary()",
		"setSectionSummary('review-pipeline-subsection', reviewPipelineSubsectionSummary())",
		"setSectionSummary('review-queue-subsection', _reviewQueueTotal + ' PR'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("review subsection collapsed-summary wiring missing %q", want)
		}
	}
}

func TestReviewSubsectionTilesDoNotAddInlineStyles(t *testing.T) {
	html := indexHTML(t)
	card := reviewSectionHTML(t, html)
	for _, id := range []string{"review-pipeline-subsection", "review-queue-subsection"} {
		start := strings.Index(card, `id="`+id+`"`)
		if start < 0 {
			t.Fatalf("Review card missing %s", id)
		}
		next := strings.Index(card[start+len(id):], `<section class="review-subsection"`)
		section := card[start:]
		if next >= 0 {
			section = card[start : start+len(id)+next]
		}
		if strings.Contains(section, `style="`) || strings.Contains(section, `style='`) {
			t.Fatalf("%s tile markup must not add inline style attributes", id)
		}
	}
}

func reviewSectionHTML(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, `id="review-queue-section" data-dashboard-section="review-queue-section"`)
	if start < 0 {
		t.Fatal("Review card not found")
	}
	end := strings.Index(html[start:], `id="nous-section"`)
	if end < 0 {
		t.Fatal("could not isolate Review card")
	}
	return html[start : start+end]
}
