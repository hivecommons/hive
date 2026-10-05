package dashboard

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// TestSidebarLabelsMatchSectionHeaderTitles (#10563) pins that every sidebar
// nav entry reads the same as the title of the section it scrolls to
// (case-insensitive; CSS uppercases the header).
func TestSidebarLabelsMatchSectionHeaderTitles(t *testing.T) {
	page := indexHTML(t)

	navRe := regexp.MustCompile(`<a class="oc-nav-item[^"]*" data-section="([^"]+)"[^>]*><span class="oc-nav-emoji">[^<]*</span><span class="oc-nav-text">([^<]+)</span>`)
	navs := navRe.FindAllStringSubmatch(page, -1)
	if len(navs) == 0 {
		t.Fatal("no sidebar nav entries found in static dashboard")
	}

	// Rendered entirely by JS; no static header to compare against.
	dynamic := map[string]bool{"governor": true, "token-panel": true, "cost-panel": true}

	buttonRe := regexp.MustCompile(`(?s)<button.*?</button>`)
	tagRe := regexp.MustCompile(`<[^>]+>`)
	h2Re := regexp.MustCompile(`(?s)<h2[^>]*>(.*?)</h2>`)

	checked := 0
	for _, nav := range navs {
		section, label := nav[1], strings.TrimSpace(html.UnescapeString(nav[2]))
		if dynamic[section] {
			continue
		}
		var inner string
		if i := strings.Index(page, `<h2 class="section-label section-header-toggle mb-4" data-action="toggleSection" data-arg0="`+section+`"`); i >= 0 {
			inner = h2Re.FindStringSubmatch(page[i:])[1]
		} else if i := strings.Index(page, `id="`+section+`"`); i >= 0 {
			m := h2Re.FindStringSubmatch(page[i:])
			if m == nil {
				t.Errorf("section %q has no header", section)
				continue
			}
			inner = m[1]
		} else {
			t.Errorf("sidebar entry %q points at missing section %q", label, section)
			continue
		}
		title := tagRe.ReplaceAllString(buttonRe.ReplaceAllString(inner, ""), " ")
		title = html.UnescapeString(title)
		title = strings.TrimLeftFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		title = strings.Join(strings.Fields(title), " ")
		lt, ll := strings.ToLower(title), strings.ToLower(label)
		if lt != ll && !strings.HasPrefix(lt, ll+" ") {
			t.Errorf("sidebar label %q does not match header title of section %q (%q)", label, section, title)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d sidebar/section pairs were checked; the parser is likely broken", checked)
	}
}
