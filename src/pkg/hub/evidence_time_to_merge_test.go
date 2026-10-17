package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestSpokeEvidenceTimeToMergeWire pins that the spoke's per-window median
// time-to-merge (#12464) decodes into the hub's HeartbeatEvidence and
// survives sanitizing, and that an old spoke leaves it nil, never zero.
func TestSpokeEvidenceTimeToMergeWire(t *testing.T) {
	in := spoke.HeartbeatPayload{Evidence: &spoke.HeartbeatEvidence{
		Merged90d:   12,
		TimeToMerge: &spoke.HeartbeatEvidenceTimeToMerge{MedianSeconds: 7200, N: 5},
		Windows: map[string]*spoke.HeartbeatEvidence{
			"7d":  {Merged90d: 3, TimeToMerge: &spoke.HeartbeatEvidenceTimeToMerge{MedianSeconds: 600, N: 2}},
			"24h": {Merged90d: 1},
		},
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	got := sanitizeEvidence(out.Evidence)
	if got.TimeToMerge == nil || *got.TimeToMerge != (EvidenceTimeToMerge{MedianSeconds: 7200, N: 5}) {
		t.Errorf("time_to_merge = %+v", got.TimeToMerge)
	}
	if w := got.Windows["7d"]; w == nil || w.TimeToMerge == nil || *w.TimeToMerge != (EvidenceTimeToMerge{MedianSeconds: 600, N: 2}) {
		t.Errorf("7d time_to_merge = %+v", w)
	}
	if w := got.Windows["24h"]; w == nil || w.TimeToMerge != nil {
		t.Errorf("24h must not report time_to_merge: %+v", w)
	}

	// A spoke predating #12464 sends no time_to_merge key.
	raw, err = json.Marshal(spoke.HeartbeatPayload{Evidence: &spoke.HeartbeatEvidence{Merged90d: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "time_to_merge") {
		t.Fatalf("absent time_to_merge must be omitted on the wire: %s", raw)
	}
	out = HeartbeatPayload{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if got := sanitizeEvidence(out.Evidence); got.TimeToMerge != nil {
		t.Fatalf("old spoke must leave time_to_merge nil: %+v", got.TimeToMerge)
	}
}

func TestSanitizeEvidenceTimeToMerge(t *testing.T) {
	for name, tc := range map[string]struct {
		in     *EvidenceTimeToMerge
		merged int
		want   *EvidenceTimeToMerge
	}{
		"nil":             {nil, 5, nil},
		"empty sample":    {&EvidenceTimeToMerge{MedianSeconds: 60, N: 0}, 5, nil},
		"negative sample": {&EvidenceTimeToMerge{MedianSeconds: 60, N: -3}, 5, nil},
		"no merges":       {&EvidenceTimeToMerge{MedianSeconds: 60, N: 2}, 0, nil},
		"n above merged":  {&EvidenceTimeToMerge{MedianSeconds: 60, N: 9}, 5, &EvidenceTimeToMerge{MedianSeconds: 60, N: 5}},
		"negative median": {&EvidenceTimeToMerge{MedianSeconds: -1, N: 2}, 5, &EvidenceTimeToMerge{MedianSeconds: 0, N: 2}},
		"huge median":     {&EvidenceTimeToMerge{MedianSeconds: 1 << 50, N: 2}, 5, &EvidenceTimeToMerge{MedianSeconds: maxEvidenceTimeToMergeSeconds, N: 2}},
		"kept":            {&EvidenceTimeToMerge{MedianSeconds: 3600, N: 2}, 5, &EvidenceTimeToMerge{MedianSeconds: 3600, N: 2}},
	} {
		got := sanitizeEvidenceTimeToMerge(tc.in, tc.merged)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
}

// evidenceTimeToMergeRegistry: L4 has three reporting hives (odd count) and
// one old spoke; L3 two (even count); L2 none.
func evidenceTimeToMergeRegistry() []RegistryEntry {
	ttm := func(s int64, n int) *EvidenceTimeToMerge { return &EvidenceTimeToMerge{MedianSeconds: s, N: n} }
	return []RegistryEntry{
		{ID: "a", ACMMLevel: 4, Evidence: &HeartbeatEvidence{Merged90d: 10, TimeToMerge: ttm(3600, 4),
			Windows: map[string]*HeartbeatEvidence{"7d": {Merged90d: 2, TimeToMerge: ttm(900, 2)}}}},
		{ID: "b", ACMMLevel: 4, Evidence: &HeartbeatEvidence{Merged90d: 20, TimeToMerge: ttm(3*86400, 10)}},
		{ID: "c", ACMMLevel: 4, Evidence: &HeartbeatEvidence{Merged90d: 8, TimeToMerge: ttm(7200, 1)}},
		{ID: "old", ACMMLevel: 4, Evidence: &HeartbeatEvidence{Merged90d: 30}},
		{ID: "d", ACMMLevel: 3, Evidence: &HeartbeatEvidence{Merged90d: 5, TimeToMerge: ttm(600, 3)}},
		{ID: "e", ACMMLevel: 3, Evidence: &HeartbeatEvidence{Merged90d: 6, TimeToMerge: ttm(1801, 6)}},
		{ID: "f", ACMMLevel: 2, Evidence: &HeartbeatEvidence{Merged90d: 7}},
		{ID: "never", ACMMLevel: 2},
	}
}

// TestEvidenceViewTimeToMergeByLevel: each ACMM level reports the median of
// its hives' medians (labelled as such), the PRs behind them and how many
// hives reported one; a level where no hive reports it omits the block.
func TestEvidenceViewTimeToMergeByLevel(t *testing.T) {
	view := buildEvidenceView(evidenceTimeToMergeRegistry())
	levels := map[int]EvidenceACMMLevel{}
	for _, a := range view.ByACMMLevel {
		levels[a.Level] = a
	}
	want := map[int]*EvidenceLevelTimeToMerge{
		// medians 3600, 7200, 259200 → 7200; the old spoke is not a zero.
		4: {Hives: 3, N: 15, MedianOfHiveMediansSeconds: 7200},
		// medians 600, 1801 → 600 + 1201/2 = 1200 (rounded down).
		3: {Hives: 2, N: 9, MedianOfHiveMediansSeconds: 1200},
		2: nil,
	}
	for level, w := range want {
		got := levels[level].TimeToMerge
		if (got == nil) != (w == nil) || (got != nil && *got != *w) {
			t.Errorf("L%d time_to_merge = %+v, want %+v", level, got, w)
		}
	}
	for _, h := range view.Hives {
		if h.ID == "a" && (h.Counts == nil || h.Counts.TimeToMerge == nil || h.Counts.TimeToMerge.MedianSeconds != 3600) {
			t.Errorf("hive a counts must carry its median for client re-pooling: %+v", h.Counts)
		}
		if h.ID == "old" && (h.Counts == nil || h.Counts.TimeToMerge != nil) {
			t.Errorf("old spoke must carry no time_to_merge: %+v", h.Counts)
		}
	}
	for _, s := range evidenceWaiting(view) {
		if strings.Contains(s, "time-to-merge") {
			t.Errorf("waiting still lists time-to-merge although levels report it: %q", s)
		}
	}

	// The 7d window pools each hive's windowed median only.
	week := buildEvidenceViewWindow(evidenceTimeToMergeRegistry(), evidenceWindowFor(7*24))
	for _, a := range week.ByACMMLevel {
		var w *EvidenceLevelTimeToMerge
		if a.Level == 4 {
			w = &EvidenceLevelTimeToMerge{Hives: 1, N: 2, MedianOfHiveMediansSeconds: 900}
		}
		if (a.TimeToMerge == nil) != (w == nil) || (w != nil && *a.TimeToMerge != *w) {
			t.Errorf("7d L%d time_to_merge = %+v, want %+v", a.Level, a.TimeToMerge, w)
		}
	}

	// Nothing reports it: every level omits the block and waiting lists it.
	none := buildEvidenceView([]RegistryEntry{{ID: "x", ACMMLevel: 4, Evidence: &HeartbeatEvidence{Merged90d: 3}}})
	if none.ByACMMLevel[0].TimeToMerge != nil {
		t.Errorf("time_to_merge = %+v, want nil", none.ByACMMLevel[0].TimeToMerge)
	}
	if ws := strings.Join(evidenceWaiting(none), "\n"); !strings.Contains(ws, evidenceWaitingTimeToMerge) {
		t.Errorf("waiting must list time-to-merge when no level reports it: %q", ws)
	}
	raw, err := json.Marshal(none)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "time_to_merge") {
		t.Errorf("unreported time_to_merge must be omitted: %s", raw)
	}
}

// TestEvidenceTimeToMergeShippedJS drives the shipped drill-down: the
// client-side re-pool reproduces the server's median of hive medians for
// the whole fleet and a level selection, the autonomy table shows the
// column with its label, and a level without data says "not reported"
// rather than 0.
func TestEvidenceTimeToMergeShippedJS(t *testing.T) {
	raw, err := json.Marshal(buildEvidenceView(evidenceTimeToMergeRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Server, Client, L3 json.RawMessage
		HTML               string
		Waiting            []string
		Durations          []string
	}
	runEvidenceJS(t, evidenceHarness(t)+"\nvar data = "+string(raw)+";\n"+
		"function ttm(v) { return (v.by_acmm_level || []).map(function(a) { return {level: a.level, time_to_merge: a.time_to_merge || null}; }); }\n"+
		"var l3 = evidenceRepool(data, evidenceFilterHives(data.hives, {hives: null, level: 3}));\n"+
		"result = {Server: ttm(data), Client: ttm(evidenceRepool(data, data.hives)), L3: ttm(l3),"+
		" HTML: evidencePanelHTML(200, data), Waiting: evidenceWaiting(data),"+
		" Durations: [evidenceDuration(1200), evidenceDuration(7200), evidenceDuration(3 * 86400), evidenceDuration(0)]};", &got)

	if string(got.Server) != string(got.Client) {
		t.Errorf("client re-pool differs from server\nserver: %s\nclient: %s", got.Server, got.Client)
	}
	if want := `[{"level":3,"time_to_merge":{"hives":2,"n":9,"median_of_hive_medians_s":1200}}]`; string(got.L3) != want {
		t.Errorf("L3 selection = %s, want %s", got.L3, want)
	}
	if want := []string{"20m", "2.0h", "3.0d", "0m"}; strings.Join(got.Durations, ",") != strings.Join(want, ",") {
		t.Errorf("durations = %q, want %q", got.Durations, want)
	}
	for _, want := range []string{
		">Median time-to-merge (median of hive medians)</th>",
		`<td class="evidence-rate"><span class="evidence-rate-main">2.0h</span><span class="evidence-rate-margin">median of 3 hive medians, 15 PRs</span></td></tr>`,
		`<td class="evidence-rate"><span class="evidence-rate-main">20m</span><span class="evidence-rate-margin">median of 2 hive medians, 9 PRs</span></td></tr>`,
		`<td class="evidence-empty">not reported</td></tr>`,
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("drill-down is missing %q", want)
		}
	}
	if strings.Contains(got.HTML, "not reported by spokes yet") {
		t.Error("the old \"not reported by spokes yet\" sentence is still shipped")
	}
	for _, s := range got.Waiting {
		if strings.Contains(s, "time-to-merge") {
			t.Errorf("JS waiting still lists time-to-merge: %q", s)
		}
	}
	if strings.Contains(got.HTML, "NaN") || strings.Contains(got.HTML, "undefined") {
		t.Errorf("drill-down leaked NaN/undefined:\n%s", got.HTML)
	}
}
