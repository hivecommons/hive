package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The Features tab is organised into FEATURES_SECTIONS: headed
// `.config-section.features-section` groups with a stable id and
// `data-settings-section` anchor, a chip table of contents at the top, and a
// scroll helper that targets the dialog body (the scroll container) rather
// than the window. The Settings search index names each hit's section by the
// heading it sits under, so the two must agree or "Open tab" lands the
// operator on a heading that does not exist.

var featuresSectionEntryRe = regexp.MustCompile(`\{ id: '([a-z-]+)', title: '([^']+)', desc: '([^']+)' \}`)

// featuresSectionCatalog returns the id→title map declared in FEATURES_SECTIONS.
func featuresSectionCatalog(t *testing.T, html string) map[string]string {
	t.Helper()
	start := strings.Index(html, "const FEATURES_SECTIONS = [")
	if start < 0 {
		t.Fatal("FEATURES_SECTIONS not found")
	}
	end := strings.Index(html[start:], "];")
	if end < 0 {
		t.Fatal("FEATURES_SECTIONS unterminated")
	}
	out := map[string]string{}
	for _, m := range featuresSectionEntryRe.FindAllStringSubmatch(html[start:start+end], -1) {
		if _, dup := out[m[1]]; dup {
			t.Errorf("FEATURES_SECTIONS: duplicate id %q", m[1])
		}
		if strings.TrimSpace(m[3]) == "" {
			t.Errorf("FEATURES_SECTIONS %q: empty description", m[1])
		}
		out[m[1]] = m[2]
	}
	return out
}

func TestFeaturesSections_CatalogRenderedOnce(t *testing.T) {
	html := featuresMarkup(t)
	catalog := featuresSectionCatalog(t, html)
	const minSections = 4 // the table of contents only earns its place at four or more
	if len(catalog) < minSections {
		t.Fatalf("FEATURES_SECTIONS has %d entries, want at least %d", len(catalog), minSections)
	}
	fn := extractJSFunction(t, html, "renderGovFeatures")
	for id, title := range catalog {
		marker := "featuresSectionHtml('" + id + "'"
		if n := strings.Count(fn, marker); n != 1 {
			t.Errorf("section %q (%s): rendered %d times in renderGovFeatures, want exactly 1", id, title, n)
		}
	}
	// Every rendered section must be declared, so a typo cannot throw at render time.
	for _, m := range regexp.MustCompile(`featuresSectionHtml\('([^']+)'`).FindAllStringSubmatch(fn, -1) {
		if _, ok := catalog[m[1]]; !ok {
			t.Errorf("renderGovFeatures renders undeclared section %q", m[1])
		}
	}
	if !strings.Contains(fn, "${featuresSectionTocHtml()}") {
		t.Error("renderGovFeatures must render the section table of contents")
	}
	tocIdx := strings.Index(fn, "${featuresSectionTocHtml()}")
	firstSection := strings.Index(fn, "featuresSectionHtml('")
	if tocIdx > firstSection {
		t.Error("table of contents must precede the first section")
	}
}

func TestFeaturesSections_AnchorsAndHeadings(t *testing.T) {
	html := featuresMarkup(t)
	fn := extractJSFunction(t, html, "featuresSectionHtml")
	for _, want := range []string{
		`class="config-section features-section"`,
		`id="${featuresSectionDomId(s.id)}"`,
		`data-settings-section="${esc(s.id)}"`,
		`<h3 class="features-section-title">${esc(s.title)}</h3>`,
		`class="features-section-desc`,
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("featuresSectionHtml missing %s", want)
		}
	}
	dom := extractJSFunction(t, html, "featuresSectionDomId")
	if !strings.Contains(dom, "'features-section-' + id") {
		t.Error("featuresSectionDomId must produce the stable features-section-<id> prefix")
	}
	toc := extractJSFunction(t, html, "featuresSectionTocHtml")
	for _, want := range []string{
		`class="features-toc"`,
		`class="features-toc-chip"`,
		`data-action="settingsScrollToSection"`,
		`data-prevent="1"`,
		`href="#${featuresSectionDomId(s.id)}"`,
	} {
		if !strings.Contains(toc, want) {
			t.Errorf("featuresSectionTocHtml missing %s", want)
		}
	}
	// settingsScrollToSection resolves the body and delegates the scroll to
	// configScrollBodyTo, which the section rail shares.
	scroll := extractJSFunction(t, html, "settingsScrollToSection") + extractJSFunction(t, html, "configScrollBodyTo")
	if !strings.Contains(scroll, "getElementById('config-body')") || !strings.Contains(scroll, "body.scrollTo(") {
		t.Error("settingsScrollToSection must scroll #config-body, the dialog's scroll container")
	}
	if strings.Contains(scroll, "window.scrollTo") || strings.Contains(scroll, "scrollIntoView") {
		t.Error("settingsScrollToSection must not scroll the window")
	}
	for _, cls := range []string{".features-toc {", ".features-toc-chip {", ".features-section-title {", ".features-section-desc {"} {
		if !strings.Contains(html, cls) {
			t.Errorf("stylesheet missing rule %s", cls)
		}
	}
	// Each section's own h3 must be the first h3 in it so settingsSearchSectionName
	// (nearest .config-section → first h3) derives the heading, not a nested title.
	rf := extractJSFunction(t, html, "renderGovFeatures")
	for _, seg := range strings.Split(rf, "featuresSectionHtml('")[1:] {
		end := strings.Index(seg, "`)}")
		if end < 0 {
			end = len(seg)
		}
		if strings.Contains(seg[:end], "<h3") {
			id := seg[:strings.Index(seg, "'")]
			t.Errorf("section %q nests an <h3>; use settings-subsection-title so search derives the section heading", id)
		}
	}
}

var featuresSearchHitRe = regexp.MustCompile(`\{ tab: 'Features', section: '([^']+)', label: '([^']+)'`)

func TestFeaturesSections_SearchIndexSectionsMatchHeadings(t *testing.T) {
	html := featuresMarkup(t)
	catalog := featuresSectionCatalog(t, html)
	titles := map[string]bool{}
	for _, title := range catalog {
		titles[title] = true
	}
	hits := featuresSearchHitRe.FindAllStringSubmatch(html, -1)
	if len(hits) == 0 {
		t.Fatal("no Features entries in the Settings search index")
	}
	for _, m := range hits {
		if !titles[m[1]] {
			t.Errorf("search hit %q: section %q is not a Features heading (have %v)", m[2], m[1], titles)
		}
	}
}
