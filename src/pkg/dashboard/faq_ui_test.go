package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFAQPanelStructure guards the refreshed FAQ section (#12439): the section
// keeps the ids the layout/reorder code relies on, every question is a
// <details class="faq-q"> with a <summary>, every answer links to a source
// doc (the panel is a map, not a second source of truth), the filter input is
// present, and the panel carries no inline style attributes.
func TestFAQPanelStructure(t *testing.T) {
	raw, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static/index.html: %v", err)
	}
	html := string(raw)

	for _, want := range []string{
		`id="faq-section" data-dashboard-section="faq-section"`,
		`<div id="faq-panel" class="faq-panel">`,
		`id="faq-filter"`,
		`data-input-action="faqFilter"`,
		`id="faq-filter-count"`,
		`id="faq-empty"`,
		`class="faq-footer"`,
		`https://hivecommons.dev/discord`,
		`https://github.com/hivecommons/hive/issues/new/choose`,
		`function faqFilter()`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("static/index.html is missing %q", want)
		}
	}

	panel := faqPanel(t, html)
	if strings.Contains(panel, ` style="`) || strings.Contains(panel, ` style='`) {
		t.Errorf("#faq-panel contains inline style attributes; use .faq-* classes on design tokens instead")
	}

	detailsRE := regexp.MustCompile(`(?s)<details class="faq-q"[^>]*>(.*?)</details>`)
	items := detailsRE.FindAllStringSubmatch(panel, -1)
	if len(items) < 25 {
		t.Fatalf("expected at least 25 FAQ questions, found %d", len(items))
	}
	if n := strings.Count(panel, `<details`); n != len(items) {
		t.Errorf("found %d <details> in #faq-panel but only %d carry class=\"faq-q\"", n, len(items))
	}
	for i, m := range items {
		body := m[1]
		if !strings.Contains(body, "<summary>") || !strings.Contains(body, "</summary>") {
			t.Errorf("FAQ question %d has no <summary>: %s", i+1, firstLine(body))
		}
		if strings.Count(body, "<summary>") != 1 {
			t.Errorf("FAQ question %d must have exactly one <summary>", i+1)
		}
		answer := body[strings.Index(body, "</summary>")+len("</summary>"):]
		if !strings.Contains(answer, `href="`) {
			t.Errorf("FAQ question %d answer has no doc link (href): %s", i+1, firstLine(body))
		}
	}

	groups := strings.Count(panel, `<section class="faq-group"`)
	if groups < 5 {
		t.Errorf("expected at least 5 FAQ groups, found %d", groups)
	}
	if strings.Count(panel, `<h3 class="faq-group-title">`) != groups {
		t.Errorf("every .faq-group needs exactly one .faq-group-title heading")
	}

	for _, stale := range []string{
		// #12439: reviewer verdicts do not stop the App auto-merge sweep.
		"pulls a PR out of the auto-merge lane",
	} {
		if strings.Contains(panel, stale) {
			t.Errorf("FAQ still contains stale claim %q", stale)
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
