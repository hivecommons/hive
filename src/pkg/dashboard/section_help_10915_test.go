package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const dashboardSectionsDoc = "../../docs/dashboard-sections.md"

var (
	sectionHelpEntryRE    = regexp.MustCompile(`'([\w-]+)':\s*\{\s*heading:\s*'([^']+)',\s*help:\s*(?:"([^"]+)"|'([^']+)')\s*\}`)
	sectionHelpNonSlugRE  = regexp.MustCompile(`[^\w\s-]`)
	sectionHelpDocLinkRE  = regexp.MustCompile(`"/docs/([\w-]+)\.md(?:#([\w-]+))?"`)
	sectionHelpBannedTerm = regexp.MustCompile(`(?i)\b(actionable|held|outside|bands?|needs-human|merge-eligible|autonomy level|agents?|governor|fleet|contributors?)\b`)
)

const sectionHelpMaxWords = 25

type sectionHelpEntry struct{ id, heading, help string }

func dashboardSectionHelpEntries(t *testing.T, html string) []sectionHelpEntry {
	t.Helper()
	table := jsConstObject(t, html, "const DASHBOARD_SECTION_HELP")
	var out []sectionHelpEntry
	for _, m := range sectionHelpEntryRE.FindAllStringSubmatch(table, -1) {
		out = append(out, sectionHelpEntry{id: m[1], heading: m[2], help: m[3] + m[4]})
	}
	if len(out) == 0 {
		t.Fatal("DASHBOARD_SECTION_HELP has no entries")
	}
	return out
}

func sectionHelpSlug(heading string) string {
	h := sectionHelpNonSlugRE.ReplaceAllString(strings.ToLower(heading), "")
	return strings.ReplaceAll(strings.TrimSpace(h), " ", "-")
}

// sectionHelpDocEntries maps each "## " heading of the help page to the first
// non-empty line of its entry.
func sectionHelpDocEntries(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(dashboardSectionsDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardSectionsDoc, err)
	}
	entries := map[string]string{}
	heading := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			heading = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			entries[heading] = ""
			continue
		}
		if heading != "" && entries[heading] == "" && strings.TrimSpace(line) != "" {
			entries[heading] = strings.TrimSpace(line)
		}
	}
	return entries
}

// TestDashboardSectionHelpCoverage keeps every dashboard section's "?" mark,
// short description and help-page entry in sync (#10915).
func TestDashboardSectionHelpCoverage(t *testing.T) {
	html := indexHTML(t)
	entries := dashboardSectionHelpEntries(t, html)
	docEntries := sectionHelpDocEntries(t)

	byID := map[string]sectionHelpEntry{}
	headings := map[string]bool{}
	for _, e := range entries {
		byID[e.id] = e
		headings[e.heading] = true
		first, ok := docEntries[e.heading]
		if !ok {
			t.Errorf("section %q: help page has no %q heading for anchor #%s", e.id, "## "+e.heading, sectionHelpSlug(e.heading))
			continue
		}
		if first != e.help {
			t.Errorf("section %q: short description differs from the first sentence of its help entry\n  mark: %q\n  page: %q", e.id, e.help, first)
		}
		if n := len(strings.Fields(e.help)); n > sectionHelpMaxWords {
			t.Errorf("section %q: short description has %d words, want at most %d", e.id, n, sectionHelpMaxWords)
		}
		if term := sectionHelpBannedTerm.FindString(e.help); term != "" {
			t.Errorf("section %q: short description uses the Hive term %q; explain it on the help page instead", e.id, term)
		}
	}

	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for _, m := range regexp.MustCompile(`(?m)^\s*'([\w-]+)':\s*\{`).FindAllStringSubmatch(config, -1) {
		if _, ok := byID[m[1]]; !ok {
			t.Errorf("section %q has no entry in DASHBOARD_SECTION_HELP", m[1])
		}
	}
	for _, id := range []string{"governor", "token-panel", "cost-panel", "advisory-digest-section", "governor-pr-models-section", "lifecycle-section"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("section %q has no entry in DASHBOARD_SECTION_HELP", id)
		}
	}

	navTargets := regexp.MustCompile(`<a class="oc-nav-item"[^>]*data-section="([\w-]+)"`).FindAllStringSubmatch(html, -1)
	if len(navTargets) == 0 {
		t.Fatal("dashboard sidebar has no section nav items")
	}
	foundLifecycleNav := false
	for _, m := range navTargets {
		id := m[1]
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("sidebar nav target %q has no matching section id", id)
		}
		if _, ok := byID[id]; !ok {
			t.Errorf("sidebar nav target %q has no entry in DASHBOARD_SECTION_HELP", id)
		}
		if id == "lifecycle-section" {
			foundLifecycleNav = true
		}
	}
	if !foundLifecycleNav {
		t.Error("Lifecycle Timeline is missing from the dashboard sidebar nav")
	}

	// Runs and Platform are documented for the v6 line but are not v5 sections.
	v6Only := map[string]bool{"Runs": true, "Platform": true}
	for heading := range docEntries {
		if heading != "Words used on this page" && !v6Only[heading] && !headings[heading] {
			t.Errorf("help page entry %q has no matching section in DASHBOARD_SECTION_HELP", heading)
		}
	}
}

func TestDashboardSectionHelpMarksRender(t *testing.T) {
	html := indexHTML(t)
	if body := jsFunctionBody(t, html, "function sectionCardHeader(opts)"); !strings.Contains(body, "${dashboardSectionHelpMark(id)}") {
		t.Fatal("sectionCardHeader does not render the section help mark next to the title")
	}
	if body := jsFunctionBody(t, html, "function renderGovernorPRModelsTile()"); !strings.Contains(body, "dashboardSectionHelpMark('governor-pr-models-section')") {
		t.Fatal("PRs by model sub-header has no help mark")
	}
	if !strings.Contains(html, `Advisory Digest<span data-section-help="advisory-digest-section"></span>`) {
		t.Fatal("Advisory Digest sub-header has no help mark slot")
	}
	mark := jsFunctionBody(t, html, "function dashboardSectionHelpMark(sectionId)")
	for _, want := range []string{
		`class="config-info section-help-mark"`,
		`target="_blank"`,
		`data-action="gh3"`,
		`aria-label="Help: ${esc(entry.heading)}"`,
		`aria-describedby="${esc(tipId)}"`,
		`role="tooltip"`,
		"DASHBOARD_HELP_CLICK_LINE",
		"dashboardDocsHref(DASHBOARD_HELP_PAGE, anchor)",
	} {
		if !strings.Contains(mark, want) {
			t.Errorf("dashboardSectionHelpMark is missing %q", want)
		}
	}
	if !strings.Contains(html, "const DASHBOARD_HELP_CLICK_LINE = 'Click (or press Enter) for the full explanation';") {
		t.Error("help pop-up lost its fixed click line")
	}
}

// TestOverviewLearnMoreLinksResolve guards the Overview tiles' "Learn more"
// links: each must name a page and heading that exist under src/docs.
func TestOverviewLearnMoreLinksResolve(t *testing.T) {
	src, err := os.ReadFile("overview_status.go")
	if err != nil {
		t.Fatalf("reading overview_status.go: %v", err)
	}
	matches := sectionHelpDocLinkRE.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("overview_status.go has no /docs/ links")
	}
	for _, m := range matches {
		raw, err := os.ReadFile("../../docs/" + m[1] + ".md")
		if err != nil {
			t.Errorf("Learn more link %s: %v", m[0], err)
			continue
		}
		if m[2] == "" {
			continue
		}
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "#") && sectionHelpSlug(strings.TrimLeft(line, "# ")) == m[2] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Learn more link %s points at a heading that does not exist", m[0])
		}
	}
}
