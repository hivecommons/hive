package webstatic

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// settingsSearchScenario runs one scenario from testdata/settings_search_scenarios.js
// against the shipped index.html in a node mini-DOM and decodes its JSON result.
func settingsSearchScenario(t *testing.T, name string, out any) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	cmd := exec.Command("node", "testdata/settings_search_harness.js", "../static/index.html", name)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("settings search scenario %s failed: %v\n%s", name, err, raw)
	}
	// The harness prints the result JSON as its last line; anything before it is noise.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), out); err != nil {
		t.Fatalf("settings search scenario %s: bad JSON %v\n%s", name, err, raw)
	}
}

type settingsSearchScroll struct {
	Key   string `json:"key"`
	Block string `json:"block"`
}

// Settings search → "Open tab" must switch to the tab where the setting is
// rendered, scroll that exact row into view inside the dialog, flash it and
// move focus to it — not drop the user at the top of a long tab.
func TestSettingsSearchOpenTabRevealsRow(t *testing.T) {
	var got struct {
		Error         string                 `json:"error"`
		Groups        []string               `json:"groups"`
		Hits          []map[string]string    `json:"hits"`
		ActiveTab     string                 `json:"activeTab"`
		RowFound      bool                   `json:"rowFound"`
		RowClass      string                 `json:"rowClass"`
		Scrolled      []settingsSearchScroll `json:"scrolled"`
		Focused       string                 `json:"focused"`
		SearchCleared bool                   `json:"searchCleared"`
	}
	settingsSearchScenario(t, "openTab", &got)
	if got.Error != "" {
		t.Fatalf("scenario error: %s (groups=%v hits=%v)", got.Error, got.Groups, got.Hits)
	}
	// dashboard.experiment_lab is rendered by the Dashboard tab even though the
	// hand-written index used to file it under Features.
	if len(got.Groups) != 1 || got.Groups[0] != "Dashboard" {
		t.Errorf("result groups = %v, want [Dashboard]", got.Groups)
	}
	if len(got.Hits) == 0 || got.Hits[0]["action"] != "settingsSearchOpenHit" || got.Hits[0]["key"] != "dashboard.experiment_lab" {
		t.Errorf("hit rows must be clickable via settingsSearchOpenHit with the key; got %v", got.Hits)
	}
	if got.ActiveTab != "Dashboard" {
		t.Errorf("active tab = %q, want Dashboard", got.ActiveTab)
	}
	if !got.RowFound {
		t.Fatal("row [data-setting-key=dashboard.experiment_lab] not rendered in #config-body")
	}
	if !strings.Contains(" "+got.RowClass+" ", " cfg-row-flash ") {
		t.Errorf("target row class = %q, want cfg-row-flash", got.RowClass)
	}
	if len(got.Scrolled) != 1 || got.Scrolled[0].Key != "dashboard.experiment_lab" || got.Scrolled[0].Block != "center" {
		t.Errorf("scrollIntoView calls = %+v, want one on the row with block:center", got.Scrolled)
	}
	if !strings.Contains(got.Focused, "dashboard.experiment_lab") {
		t.Errorf("focus = %q, want the setting row/control", got.Focused)
	}
	if !got.SearchCleared {
		t.Error("search box should be cleared after opening a result")
	}
}

// Clicking the result row itself, and pressing Enter in the search box, both
// land on the setting.
func TestSettingsSearchRowClickAndEnter(t *testing.T) {
	type landing struct {
		Tab      string `json:"tab"`
		Key      string `json:"key"`
		Scrolled int    `json:"scrolled"`
	}
	var got struct {
		Error  string  `json:"error"`
		First  landing `json:"first"`
		Second landing `json:"second"`
	}
	settingsSearchScenario(t, "openHitAndEnter", &got)
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	if got.First != (landing{"Logging", "logging.kick_log_source", 1}) {
		t.Errorf("row click landed on %+v", got.First)
	}
	if got.Second != (landing{"Dashboard", "dashboard.experiment_lab", 1}) {
		t.Errorf("Enter landed on %+v", got.Second)
	}
}

// A target inside a collapsed <details> is expanded before being revealed.
func TestSettingsSearchRevealExpandsDetails(t *testing.T) {
	var got struct {
		RowKey      string `json:"rowKey"`
		DetailsOpen bool   `json:"detailsOpen"`
		Flashed     bool   `json:"flashed"`
		Focused     string `json:"focused"`
	}
	settingsSearchScenario(t, "revealExpandsDetails", &got)
	if got.RowKey != "x.y" || !got.DetailsOpen || !got.Flashed || got.Focused != "input" {
		t.Errorf("reveal inside <details>: %+v", got)
	}
}

// A static index entry whose tab hint is wrong must still list under, and
// open, the tab where the key is actually rendered.
func TestSettingsSearchDriftedEntryResolvesFromDOM(t *testing.T) {
	var got struct {
		Error     string   `json:"error"`
		Groups    []string `json:"groups"`
		Resolved  string   `json:"resolved"`
		ActiveTab string   `json:"activeTab"`
		Key       string   `json:"key"`
	}
	settingsSearchScenario(t, "driftedEntry", &got)
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	if got.Resolved != "Dashboard" || got.ActiveTab != "Dashboard" || got.Key != "dashboard.experiment_lab" || len(got.Groups) != 1 || got.Groups[0] != "Dashboard" {
		t.Errorf("drifted entry: %+v", got)
	}
}

// Controls a tab fills in after its own fetch are found by the retry loop.
func TestSettingsSearchRevealRetriesLazyRows(t *testing.T) {
	var got struct {
		FlashedBefore bool   `json:"flashedBefore"`
		Key           string `json:"key"`
		Scrolled      int    `json:"scrolled"`
	}
	settingsSearchScenario(t, "lazyRow", &got)
	if got.FlashedBefore || got.Key != "lazy.key" || got.Scrolled != 1 {
		t.Errorf("lazy row reveal: %+v", got)
	}
}

// Every hand-written search index entry must resolve to a rendered control
// carrying its key, in the tab it claims — otherwise "Open tab" strands the
// user on a tab with no such setting (as happened for Experiment Lab).
func TestSettingsSearchIndexResolvesToControls(t *testing.T) {
	var got struct {
		Entries  int `json:"entries"`
		Statics  int `json:"statics"`
		Keys     int `json:"keys"`
		Problems []struct {
			Entry   string `json:"entry"`
			Tab     string `json:"tab"`
			Key     string `json:"key"`
			Problem string `json:"problem"`
		} `json:"problems"`
	}
	settingsSearchScenario(t, "audit", &got)
	if got.Statics < 20 || got.Keys < 100 {
		t.Fatalf("audit rendered too little: %d static entries, %d keys", got.Statics, got.Keys)
	}
	for _, p := range got.Problems {
		t.Errorf("search index entry %q (tab %s, key %q): %s", p.Entry, p.Tab, p.Key, p.Problem)
	}
}
