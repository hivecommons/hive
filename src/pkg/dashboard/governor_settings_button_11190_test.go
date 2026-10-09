package dashboard

import (
	"strings"
	"testing"
)

func TestGovernorSettingsButtonStaysNextToHeaderControls(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, ".governor-card .dash-card-actions { margin-left: 0; }") {
		t.Fatal("Governor settings button still uses a one-off far-right action override")
	}
	header := jsFunctionBody(t, html, "function sectionCardHeader(opts)")
	if !strings.Contains(header, "${title}${dashboardSectionHelpMark(id)}${settings}${subtitle}") {
		t.Fatal("section settings buttons must render next to help in the shared title cluster")
	}
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	entry := dashboardSectionConfigEntry(t, config, "governor")
	if !strings.Contains(entry, "settingsHtml: sectionSettingsGear('Governor'") {
		t.Fatal("Governor settings button must use the shared section settings gear path")
	}
}
