package dashboard

import (
	"strings"
	"testing"
)

func TestGovernorSettingsSearchStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="settings-search-panel"`,
		`role="search"`,
		`aria-label="Search global settings"`,
		`id="settings-search-input"`,
		`placeholder="Search settings…"`,
		`id="settings-search-count"`,
		`aria-live="polite"`,
		"function applySettingsSearch(value)",
		"function settingsSearchBuildIndex()",
		"function settingsSearchOpenTab(tab)",
		"settings-search-mark",
		"settings-search-key",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("settings search UI missing %q", want)
		}
	}
}

func TestGovernorSettingsSearchIndexesKnownSettings(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"Strategy Lab",
		"dashboard.strategy_lab",
		"Kick log source",
		"logging.kick_log_source",
		"Watchdog",
		"governor.watchdog",
		"Restart thresholds",
		"Maturity Level",
		"ACMM",
		"Release channel",
		"Knowledge channel",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("settings search known setting/index seed missing %q", want)
		}
	}
}

func TestGovernorSettingsSearchOnlyShowsKeyHintsWhenSearchActive(t *testing.T) {
	html := indexHTML(t)
	fn := jsFunctionBody(t, html, "function renderSettingsSearchResults(query)")
	if !strings.Contains(fn, "item.key ? '<code class=\"settings-search-key\">'") {
		t.Fatal("settings search results do not render YAML key hints from indexed key data")
	}

	keyFn := jsFunctionBody(t, html, "function settingsSearchKeyFromField(field)")
	if !strings.Contains(keyFn, "[data-section][data-key]") {
		t.Fatal("settings search key extraction does not cover toggle-backed data-section/data-key settings")
	}
	if !strings.Contains(fn, "No settings match") {
		t.Fatal("settings search does not render an in-app empty state")
	}
}
