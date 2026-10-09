package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var settingHelpCallRE = regexp.MustCompile(`settingHelpMark\('([\w-]+)', '([\w-]*)', `)

// TestSettingHelpMarksAreDocsLinks keeps every Settings help mark on the same
// "?" docs-link convention as the dashboard card marks (#11213).
func TestSettingHelpMarksAreDocsLinks(t *testing.T) {
	html := indexHTML(t)
	for _, old := range []string{`class="config-info">i<`, `class="config-info" tabindex="0">i<`} {
		if strings.Contains(html, old) {
			t.Errorf("index.html still has an inline %q help mark; use settingHelpMark(page, anchor, label, tip)", old)
		}
	}

	mark := jsFunctionBody(t, html, "function settingHelpMark(page, anchor, label, tipHtml)")
	for _, want := range []string{
		`class="config-info section-help-mark"`,
		"dashboardDocsHref(page, anchor)",
		`data-docs-page="${esc(page)}"`,
		`data-docs-anchor="${esc(anchor)}"`,
		`target="_blank"`,
		`rel="noopener noreferrer"`,
		`data-action="gh3"`,
		`data-stop="1"`,
		`aria-label="Help: ${esc(label)}"`,
		`aria-describedby="${tipId}"`,
		`role="tooltip"`,
		">?<",
	} {
		if !strings.Contains(mark, want) {
			t.Errorf("settingHelpMark is missing %q", want)
		}
	}

	calls := settingHelpCallRE.FindAllStringSubmatch(html, -1)
	if got := strings.Count(html, "settingHelpMark("); got != len(calls)+1 {
		t.Fatalf("%d settingHelpMark( occurrences but only %d calls name a literal page and anchor", got-1, len(calls))
	}
	if len(calls) < 200 {
		t.Fatalf("only %d settingHelpMark calls found, want >= 200", len(calls))
	}
	for _, m := range calls {
		raw, err := os.ReadFile("../../docs/" + m[1] + ".md")
		if err != nil {
			t.Errorf("settings help link %s: %v", m[0], err)
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
			t.Errorf("settings help link %s points at a heading that does not exist", m[0])
		}
	}
}
