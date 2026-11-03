package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/rfccoverage"
)

// The triage existing-coverage check reads the same index the Settings
// search box uses; it must keep parsing and keep resolving to tabs.
func TestSettingsSearchPageParsesForTriage(t *testing.T) {
	page, err := SettingsSearchPage()
	if err != nil {
		t.Fatal(err)
	}
	settings := rfccoverage.ParseSettingsIndex(page)
	if len(settings) < 20 {
		t.Fatalf("parsed %d settings from the embedded page, want the full static index", len(settings))
	}
	found := false
	for _, s := range settings {
		if s.Tab == "" || s.Key == "" || s.Label == "" {
			t.Errorf("incomplete entry %+v", s)
		}
		if s.Key == "issues.close_on_merge" && s.Tab == "Features" {
			found = true
		}
	}
	if !found {
		t.Fatal("issues.close_on_merge (Features) missing from the parsed index")
	}
	html := string(page)
	for _, want := range []string{"function settingsSearchPrefill(query)", "openConfigDialog('governor', null, tab, parts[2] || '')", "settingsSearchPrefill(_configState.initialSearch)"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q: the #config/governor/<tab>/<query> deep link triage comments use", want)
		}
	}
}
