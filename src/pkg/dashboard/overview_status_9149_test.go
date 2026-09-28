package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestOverviewStatusAndPageConsumeServer9149(t *testing.T) {
	s := newTestServer()
	cfg := config.DashboardIssueBandsConfig{WaitingLabels: []string{"custom-human"}, DoneLabels: []string{"custom-done"}, StaleDays: 7}
	s.deps = &Dependencies{Config: &config.Config{Dashboard: config.DashboardConfig{IssueBands: cfg}}}
	status := parityStatus9102(t)
	before, _ := json.Marshal(status)
	now := mustTime9102(t, "2026-09-27T00:00:00Z")
	response := s.statusWithOverviewBands(status, now)
	after, _ := json.Marshal(status)
	if string(before) != string(after) {
		t.Fatal("classification mutated cached status")
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Repos []struct {
			Issues     []map[string]any `json:"actionableIssues"`
			HeldIssues []map[string]any `json:"heldIssues"`
			PRs        []map[string]any `json:"openPrs"`
			HeldPRs    []map[string]any `json:"heldPrs"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{overviewKindIssues, overviewKindPRs} {
		rows, specs := s.overviewRows(status, kind, cfg, overviewFilters{}, now)
		items := append(wire.Repos[0].Issues, wire.Repos[0].HeldIssues...)
		actualSpecs := response.OverviewBands.Issues
		if kind == overviewKindPRs {
			items = append(wire.Repos[0].PRs, wire.Repos[0].HeldPRs...)
			actualSpecs = response.OverviewBands.PRs
		}
		if !reflect.DeepEqual(specs, actualSpecs) {
			t.Fatalf("%s status taxonomy/counts differ from exports", kind)
		}
		for _, row := range rows {
			export := row.(map[string]any)
			found := false
			for _, item := range items {
				if int(item["number"].(float64)) != export["number"] {
					continue
				}
				found = true
				var label string
				for _, spec := range specs {
					if spec.Key == item["band"] {
						label = spec.Label
					}
				}
				if label != export["band"] || item["stale"] != export["stale"] || item["held"] != export["held"] {
					t.Fatalf("status item %#v differs from export %#v", item, export)
				}
				signals, ok := item["signals"].([]any)
				if !ok {
					t.Fatalf("signals missing from item %#v", item)
				}
				labels := make([]any, 0)
				for _, signal := range signals {
					labels = append(labels, signal.(map[string]any)["label"].(string))
				}
				key := "state_signals"
				if kind == overviewKindPRs {
					key = "signals"
				}
				if !reflect.DeepEqual(labels, export[key]) {
					t.Fatalf("signals differ for %s #%v", kind, item["number"])
				}
			}
			if !found {
				t.Fatalf("missing %s export item %#v", kind, export)
			}
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\nconst window = {_lastStatus: " + string(payload) + "};\n")
	script.WriteString(`
const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];
const OVERVIEW_CHART_PERCENT_SCALE = 100;
const OVERVIEW_CHART_DECIMAL_PLACES = 1;
let token = '';
const localStorage = {getItem: () => token};
`)
	for _, name := range []string{"issueBandSpec", "issueBandLabel", "issueBandRule", "issueBandTip", "issueBandRank", "issueUpdatedAt", "issueIsStale", "groupedRepoIssues", "prBandSpec", "prBandLabel", "prBandRule", "prBandTip", "prBandRank", "prUpdatedAt", "prCreatedAt", "prReviewClassRank", "prIsStale", "groupedRepoPRs", "overviewRepoName", "overviewIssueBandSlices", "overviewPRBandSlices", "overviewExportURL", "esc", "prSignalHTML", "overviewLegendHTML"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
for (const [kind, slices] of [['issues', overviewIssueBandSlices(window._lastStatus.repos)], ['prs', overviewPRBandSlices(window._lastStatus.repos)]]) {
 const specs = window._lastStatus.overview_bands[kind];
 assert.deepEqual(slices.map(s => ({key:s.key, label:s.label, rule:s.rule, count:s.count})), specs.map(s => ({key:s.key, label:s.label, rule:s.rule, count:s.count})));
 for (const slice of slices) for (const entry of slice.items) {
  assert.equal(entry.info.band, slice.key);
  for (const signal of entry.info.signals) assert.ok(prSignalHTML(entry.info).includes(esc(signal.label)));
 }
 const legend = overviewLegendHTML(slices, kind);
 assert.ok(legend.includes('/api/overview/' + kind + '.csv?band='));
}
// The browser must honor the response even when labels/timestamps would
// produce a different classification if it still ran its old rules.
const supplied = {number: 99, labels:['needs-human'], band:'done', stale:false, signals:[{glyph:'X', label:'server signal'}], updated_at:'2000-01-01'};
assert.equal(groupedRepoIssues([supplied])[0].band, 'done');
assert.equal(issueIsStale(supplied), false);
assert.equal(groupedRepoPRs([supplied], [])[0].band, 'done');
assert.equal(prIsStale(supplied), false);
assert.equal(overviewIssueBandSlices([]).reduce((n,s) => n+s.count, 0), 0);
assert.equal(overviewPRBandSlices([]).reduce((n,s) => n+s.count, 0), 0);
assert.equal(overviewExportURL('issues'), '/api/overview/issues.csv');
assert.equal(overviewExportURL('pr','waiting'), '/api/overview/prs.csv?band=waiting');
token = 'a&b?c';
const url = new URL(overviewExportURL('prs','in-review'), 'https://hive.test');
assert.equal(url.searchParams.get('token'), token);
assert.equal(url.searchParams.get('band'), 'in-review');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("page consumer failed: %v\n%s", err, out)
	}
}

func TestOverviewStatusHandler9149(t *testing.T) {
	s := newTestServer()
	s.status = parityStatus9102(t)
	for _, query := range []string{"", "?fields=repos,overview_bands"} {
		rr := httptest.NewRecorder()
		s.handleStatus(rr, httptest.NewRequest("GET", "/api/status"+query, nil))
		var response map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["overview_bands"] == nil {
			t.Fatal("status missing taxonomy")
		}
		repo := response["repos"].([]any)[0].(map[string]any)
		for _, key := range []string{"actionableIssues", "heldIssues", "openPrs", "heldPrs"} {
			for _, raw := range repo[key].([]any) {
				item := raw.(map[string]any)
				for _, field := range []string{"band", "signals", "stale", "held", "number", "title"} {
					if _, ok := item[field]; !ok {
						t.Fatalf("%s missing %s", key, field)
					}
				}
			}
		}
	}
	// Empty collections still carry the taxonomy, and null status retains its
	// existing initializing response instead of fabricating empty bands.
	s.status = &StatusPayload{}
	if got := s.statusWithOverviewBands(s.status, time.Now()); len(got.OverviewBands.Issues) != 5 || len(got.OverviewBands.PRs) != 6 {
		t.Fatal("missing empty taxonomy")
	}
	s.status = nil
	rr := httptest.NewRecorder()
	s.handleStatus(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"status":"initializing"`) {
		t.Fatal(rr.Body.String())
	}
}

func TestOverviewStatusSSE9149(t *testing.T) {
	s := newTestServer()
	ch := make(chan []byte, 4)
	s.sseClients[ch] = struct{}{}
	s.UpdateStatus(parityStatus9102(t))
	assertFrame := func(wantHeld bool) {
		t.Helper()
		select {
		case frame := <-ch:
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(frame), "data: "))), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["overview_bands"] == nil {
				t.Fatal("SSE missing band specs")
			}
			repo := payload["repos"].([]any)[0].(map[string]any)
			key := "openPrs"
			if wantHeld {
				key = "heldPrs"
			}
			found := false
			for _, raw := range repo[key].([]any) {
				item := raw.(map[string]any)
				if item["number"] != float64(11) {
					continue
				}
				found = true
				band := "eligible"
				if wantHeld {
					band = "waiting"
				}
				if item["band"] != band || item["held"] != wantHeld {
					t.Fatalf("SSE classification after hold=%v: %#v", wantHeld, item)
				}
			}
			if !found {
				t.Fatal("SSE missing PR")
			}
		default:
			t.Fatal("no full status frame")
		}
	}
	assertFrame(false)
	s.applyRepoHoldToStatus("octo/demo", 11, "pr", true, "hold", nil)
	assertFrame(true)
}
