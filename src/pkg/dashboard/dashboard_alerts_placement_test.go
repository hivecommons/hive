package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardRateLimitAlertRendersBeforeFirstContentSection(t *testing.T) {
	html := indexHTML(t)

	notices := strings.Index(html, `id="dash-notices"`)
	rateAlert := strings.Index(html, `id="gh-rate-alert"`)
	authAlert := strings.Index(html, `id="gh-auth-alert"`)
	firstSection := strings.Index(html, `id="overview-section"`)
	if notices < 0 || rateAlert < 0 || authAlert < 0 || firstSection < 0 {
		t.Fatalf("markup missing: dash-notices=%d gh-rate-alert=%d gh-auth-alert=%d overview-section=%d", notices, rateAlert, authAlert, firstSection)
	}
	noticesEnd := notices + strings.Index(html[notices:], `<div id="toast-container"></div>`)
	if noticesEnd < notices {
		t.Fatalf("dash-notices end marker missing after offset %d", notices)
	}
	if rateAlert < notices || authAlert < notices || rateAlert > noticesEnd || authAlert > noticesEnd {
		t.Fatalf("GitHub alert markup must stay inside the pinned top notices area: dash-notices=%d gh-auth-alert=%d gh-rate-alert=%d", notices, authAlert, rateAlert)
	}
	if rateAlert > firstSection || authAlert > firstSection {
		t.Fatalf("GitHub alerts must render before the first content section: gh-auth-alert=%d gh-rate-alert=%d overview-section=%d", authAlert, rateAlert, firstSection)
	}
}

func TestUpgradeBannerHostRendersInTopNoticeStack(t *testing.T) {
	html := indexHTML(t)

	notices := strings.Index(html, `id="dash-notices"`)
	upgradeBanner := strings.Index(html, `id="release-notes-banner"`)
	firstSection := strings.Index(html, `id="overview-section"`)
	if notices < 0 || upgradeBanner < 0 || firstSection < 0 {
		t.Fatalf("markup missing: dash-notices=%d release-notes-banner=%d overview-section=%d", notices, upgradeBanner, firstSection)
	}
	noticesEnd := notices + strings.Index(html[notices:], `<div id="toast-container"></div>`)
	if noticesEnd < notices {
		t.Fatalf("dash-notices end marker missing after offset %d", notices)
	}
	if upgradeBanner < notices || upgradeBanner > noticesEnd {
		t.Fatalf("upgrade banner host must stay inside the pinned top notices area: dash-notices=%d release-notes-banner=%d", notices, upgradeBanner)
	}
	if upgradeBanner > firstSection {
		t.Fatalf("upgrade banner host must render before the first content section: release-notes-banner=%d overview-section=%d", upgradeBanner, firstSection)
	}
}
