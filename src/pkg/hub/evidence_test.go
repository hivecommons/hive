package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSanitizeEvidenceBounds(t *testing.T) {
	if sanitizeEvidence(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	got := sanitizeEvidence(&HeartbeatEvidence{
		Verdicts90d: 20, FalseApprove90d: 99, FalseBlock90d: -3,
		Merged90d: 5, Reverted14d90d: 9,
		Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 4, Bad: 8}},
	})
	if got.FalseApprove90d != 20 || got.FalseBlock90d != 0 || got.Reverted14d90d != 5 || got.Calibration[0].Bad != 4 {
		t.Fatalf("not clamped: %+v", got)
	}
}

func TestBuildEvidenceViewSmallNIsUnknown(t *testing.T) {
	v := buildEvidenceView([]RegistryEntry{
		{ID: "a", Evidence: &HeartbeatEvidence{Verdicts90d: 20, FalseBlock90d: 5, Merged90d: 3, Reverted14d90d: 1,
			Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 6}}}},
		{ID: "b"},
	})
	if v.Spokes != 2 || v.Reporting != 1 || v.Hives[1].Reported {
		t.Fatalf("coverage wrong: %+v", v)
	}
	a := v.Hives[0]
	if a.FalseBlockRate.Rate == nil || *a.FalseBlockRate.Rate != 0.25 {
		t.Fatalf("false block rate: %+v", a.FalseBlockRate)
	}
	if a.RevertRate.Rate != nil {
		t.Fatal("n<10 must be unknown")
	}
	if v.Calibration[0].Rate == nil || *v.Calibration[0].Rate != 0.5 || v.Calibration[1].Rate != nil {
		t.Fatalf("pooled calibration: %+v", v.Calibration)
	}
}

func TestEvidenceCriteria(t *testing.T) {
	yes := true
	in := quadrantInputs{selfMergeAllowed: &yes, evidence: &HeartbeatEvidence{
		Verdicts90d: 20, FalseBlock90d: 4, FalseApprove90d: 1, Merged90d: 20, Reverted14d90d: 4}}
	findCriterion(t, trustCriteria(in), "delegation")
	findCriterion(t, trustCriteria(in), "revert_rate")
	findCriterion(t, satisfactionCriteria(in), "human_override_rate")
	assertUnmeasured(t, satisfactionCriteria(quadrantInputs{evidence: &HeartbeatEvidence{Verdicts90d: 9}}), "human_override_rate")
}

func TestSanitizeEvidenceFamiliesBounds(t *testing.T) {
	in := []string{" copilot ", "copilot", "", strings.Repeat("x", maxEvidenceFamilyRunes+5)}
	for i := 0; i < maxEvidenceFamilies+3; i++ {
		in = append(in, fmt.Sprintf("fam-%d", i))
	}
	got := sanitizeEvidenceFamilies(in)
	if len(got) != maxEvidenceFamilies {
		t.Fatalf("len = %d, want %d: %v", len(got), maxEvidenceFamilies, got)
	}
	if got[0] != "copilot" || len([]rune(got[1])) != maxEvidenceFamilyRunes {
		t.Fatalf("trim/dedupe/truncate wrong: %q %d", got[0], len([]rune(got[1])))
	}
	if sanitizeEvidenceFamilies(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestEvidenceEndpoint(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.registry.Hives = []RegistryEntry{
		{ID: "b"},
		{ID: "a", Evidence: &HeartbeatEvidence{Verdicts90d: 20, FalseBlock90d: 5, Merged90d: 10, Reverted14d90d: 1}},
	}

	handler := srv.requireAdmin(srv.handleEvidence)
	req := httptest.NewRequest("GET", evidencePath, nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "regularuser"})
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	srv.handleEvidence(w, httptest.NewRequest("GET", evidencePath, nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d content-type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	var got EvidenceView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Spokes != 2 || got.Reporting != 1 || len(got.Hives) != 2 || got.Hives[0].ID != "a" || !got.Hives[0].Reported {
		t.Fatalf("view = %+v", got)
	}
}

func TestEvidenceFamilyMix(t *testing.T) {
	for _, tc := range []struct {
		name               string
		reviewers, authors []string
		want               string
	}{
		{"no reviewers", nil, []string{"claude"}, evidenceFamilyUnknown},
		{"no authors", []string{"claude"}, nil, evidenceFamilyUnknown},
		{"same", []string{"claude"}, []string{"claude", "gpt"}, evidenceFamilySame},
		{"cross", []string{"gpt"}, []string{"claude"}, evidenceFamilyCross},
		{"mixed", []string{"claude", "gpt"}, []string{"claude"}, evidenceFamilyMixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceFamilyMix(tc.reviewers, tc.authors); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildEvidenceViewDrillDownSections(t *testing.T) {
	merged := func(v int) *int { return &v }
	v := buildEvidenceView([]RegistryEntry{
		{ID: "same", ACMMLevel: 3, PRsMerged90d: merged(40), Evidence: &HeartbeatEvidence{
			Verdicts90d: 20, FalseApprove90d: 2, Merged90d: 30, Reverted14d90d: 3,
			ReviewerFamilies: []string{"claude"}, AuthorFamilies: []string{"claude"}}},
		{ID: "cross", ACMMLevel: 3, PRsMerged90d: merged(5), Evidence: &HeartbeatEvidence{
			Verdicts90d: 10, FalseApprove90d: 1, Merged90d: 9, Reverted14d90d: 0,
			ReviewerFamilies: []string{"gpt"}, AuthorFamilies: []string{"claude"}}},
		{ID: "unknown", ACMMLevel: 5, Evidence: &HeartbeatEvidence{Verdicts90d: 50, FalseApprove90d: 50, Merged90d: 4}},
		{ID: "silent", ACMMLevel: 1, PRsMerged90d: merged(100)},
	})

	for _, tc := range []struct {
		name     string
		got      EvidenceRate
		wantN    int
		wantRate *float64
	}{
		{"same-family false approve", v.FamilyFalseApproveRate.Same, 20, ptrFloat(0.1)},
		{"cross-family false approve", v.FamilyFalseApproveRate.Cross, 10, ptrFloat(0.1)},
		{"mixed-family is empty", v.FamilyFalseApproveRate.Mixed, 0, nil},
		{"hive record coverage", v.Hives[1].RecordCoverage, 40, ptrFloat(0.75)},
		{"record over-count is capped", v.Hives[0].RecordCoverage, 5, nil},
		{"fleet record coverage", v.RecordCoverage, 45, ptrFloat(35.0 / 45)},
		{"no github count is unknown", v.Hives[3].RecordCoverage, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.N != tc.wantN {
				t.Fatalf("n = %d, want %d", tc.got.N, tc.wantN)
			}
			if (tc.got.Rate == nil) != (tc.wantRate == nil) {
				t.Fatalf("rate = %v, want %v", tc.got.Rate, tc.wantRate)
			}
			if tc.wantRate == nil {
				if tc.got.Lo != nil || tc.got.Hi != nil {
					t.Fatalf("n<10 must carry no interval: %+v", tc.got)
				}
				return
			}
			if *tc.got.Rate != *tc.wantRate {
				t.Fatalf("rate = %v, want %v", *tc.got.Rate, *tc.wantRate)
			}
			lo, hi := wilson(int(*tc.wantRate*float64(tc.wantN)+0.5), tc.wantN)
			if tc.got.Lo == nil || tc.got.Hi == nil || *tc.got.Lo != lo || *tc.got.Hi != hi || lo > *tc.wantRate || hi < *tc.wantRate {
				t.Fatalf("wilson = [%v,%v], want [%v,%v]", tc.got.Lo, tc.got.Hi, lo, hi)
			}
		})
	}

	if v.Hives[0].ID != "cross" || v.Hives[0].FamilyMix != evidenceFamilyCross || v.Hives[2].FamilyMix != "" || v.Hives[3].FamilyMix != evidenceFamilyUnknown {
		t.Fatalf("family mix rows: %+v", v.Hives)
	}
	if len(v.ByACMMLevel) != 2 {
		t.Fatalf("silent hives must not add an ACMM row: %+v", v.ByACMMLevel)
	}
	l3, l5 := v.ByACMMLevel[0], v.ByACMMLevel[1]
	if l3.Level != 3 || l3.Hives != 2 || l3.Merged90d != 39 || l3.RevertRate.Rate == nil || *l3.RevertRate.Rate != 3.0/39 {
		t.Fatalf("acmm 3: %+v", l3)
	}
	if l5.Level != 5 || l5.RevertRate.Rate != nil {
		t.Fatalf("acmm 5 n<10 must be unknown: %+v", l5)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// TestBuildEvidenceViewFleetPanels (#12370): the fleet analogues of the spoke
// panels pool across reporting hives with Wilson intervals, break down per
// ACMM level, carry raw counts per hive for client-side filtering, and are
// omitted entirely while no spoke reports them.
func TestBuildEvidenceViewFleetPanels(t *testing.T) {
	merged := func(v int) *int { return &v }
	newSpoke := &HeartbeatEvidence{
		Verdicts90d: 20, FalseApprove90d: 2, Merged90d: 30, Reverted14d90d: 3,
		OutcomesByAuthorKind: map[string]EvidenceOutcome{
			"agent": {N: 20, Merged: 16, Reverted: 2},
			"human": {N: 5, Merged: 4},
		},
		WriteCounters: map[string]EvidenceWriteCount{"guarded": {Attempted: 40, Blocked: 4}, "sandbox": {Attempted: 3}},
		ShadowGate: &EvidenceShadow{
			ReviewGate: EvidenceShadowGate{Held: EvidenceOutcome{N: 10, Merged: 8, Reverted: 2}, Passed: EvidenceOutcome{N: 15, Merged: 12}},
		},
	}
	otherSpoke := &HeartbeatEvidence{
		Verdicts90d: 10, Merged90d: 9,
		OutcomesByAuthorKind: map[string]EvidenceOutcome{"agent": {N: 10, Merged: 4, Reverted: 1}},
		WriteCounters:        map[string]EvidenceWriteCount{"guarded": {Attempted: 10, Blocked: 1}},
	}
	oldSpoke := &HeartbeatEvidence{Verdicts90d: 50, Merged90d: 4}

	v := buildEvidenceView([]RegistryEntry{
		{ID: "new", ACMMLevel: 3, PRsMerged90d: merged(40), Evidence: newSpoke},
		{ID: "other", ACMMLevel: 4, Evidence: otherSpoke},
		{ID: "old", ACMMLevel: 3, Evidence: oldSpoke},
		{ID: "silent", ACMMLevel: 1},
	})

	// Outcomes: fixed kind order; agent pooled over two hives; bot is a zero
	// row so the client never sees a missing kind.
	if len(v.OutcomesByAuthorKind) != 3 {
		t.Fatalf("outcomes = %+v", v.OutcomesByAuthorKind)
	}
	agent, human, bot := v.OutcomesByAuthorKind[1], v.OutcomesByAuthorKind[0], v.OutcomesByAuthorKind[2]
	if agent.Kind != "agent" || agent.Hives != 2 || agent.N != 30 || agent.Merged != 20 || agent.Reverted != 3 ||
		agent.MergeRate.Rate == nil || *agent.MergeRate.Rate != 20.0/30 || agent.RevertRate.Rate == nil || *agent.RevertRate.Rate != 3.0/20 {
		t.Errorf("agent = %+v", agent)
	}
	if human.Kind != "human" || human.Hives != 1 || human.N != 5 || human.MergeRate.Rate != nil || human.MergeRate.N != 5 {
		t.Errorf("human n<10 must be unknown: %+v", human)
	}
	if bot.Kind != "bot" || bot.Hives != 0 || bot.N != 0 || bot.MergeRate.Rate != nil {
		t.Errorf("bot = %+v", bot)
	}

	// Write counters: sorted by tier, pooled, block rate over attempted.
	if len(v.WriteCounters) != 2 || v.WriteCounters[0].Tier != "guarded" || v.WriteCounters[1].Tier != "sandbox" {
		t.Fatalf("write counters = %+v", v.WriteCounters)
	}
	g := v.WriteCounters[0]
	if g.Hives != 2 || g.Attempted != 50 || g.Blocked != 5 || g.BlockRate.Rate == nil || *g.BlockRate.Rate != 0.1 {
		t.Errorf("guarded = %+v", g)
	}
	if sb := v.WriteCounters[1]; sb.Hives != 1 || sb.Attempted != 3 || sb.BlockRate.Rate != nil {
		t.Errorf("sandbox n<10 must be unknown: %+v", sb)
	}

	// Shadow gate: only "new" reports one; intent evaluated nothing.
	sg := v.ShadowGate
	if sg == nil || sg.Hives != 1 || sg.ReviewGate.Hives != 1 || sg.ReviewGate.N != 25 || sg.Intent.Hives != 0 || sg.Intent.N != 0 {
		t.Fatalf("shadow = %+v", sg)
	}
	if sg.ReviewGate.Held.Open != 2 || sg.ReviewGate.Held.RevertRate.Rate != nil || sg.ReviewGate.Held.RevertRate.N != 8 ||
		sg.ReviewGate.Passed.Open != 3 || sg.ReviewGate.Passed.RevertRate.Rate == nil || *sg.ReviewGate.Passed.RevertRate.Rate != 0 {
		t.Errorf("review gate sides = held %+v passed %+v", sg.ReviewGate.Held, sg.ReviewGate.Passed)
	}

	// Per ACMM level: L3 pools new+old (old adds nothing to the panels), L4
	// is other alone, and the silent L1 hive adds no row.
	if len(v.ByACMMLevel) != 2 || v.ByACMMLevel[0].Level != 3 || v.ByACMMLevel[1].Level != 4 {
		t.Fatalf("acmm = %+v", v.ByACMMLevel)
	}
	l3, l4 := v.ByACMMLevel[0], v.ByACMMLevel[1]
	if l3.Hives != 2 || len(l3.OutcomesByAuthorKind) != 3 || l3.OutcomesByAuthorKind[1].N != 20 || l3.OutcomesByAuthorKind[1].Hives != 1 ||
		len(l3.WriteCounters) != 2 || l3.WriteCounters[0].Attempted != 40 || l3.ShadowGate == nil || l3.ShadowGate.ReviewGate.N != 25 {
		t.Errorf("L3 = %+v", l3)
	}
	if l4.Hives != 1 || l4.OutcomesByAuthorKind[1].N != 10 || len(l4.WriteCounters) != 1 || l4.WriteCounters[0].Blocked != 1 || l4.ShadowGate != nil {
		t.Errorf("L4 = %+v", l4)
	}

	// Per-hive raw counts: present for every reporting hive, nil otherwise.
	rows := map[string]EvidenceHive{}
	for _, h := range v.Hives {
		rows[h.ID] = h
	}
	if c := rows["new"].Counts; c == nil || c.Verdicts90d != 20 || c.FalseApprove90d != 2 || c.Merged90d != 30 || c.Reverted14d90d != 3 ||
		c.Recorded90d == nil || *c.Recorded90d != 30 || len(c.Calibration) != EvidenceCalibrationBuckets ||
		c.OutcomesByAuthorKind["agent"].N != 20 || c.WriteCounters["guarded"].Attempted != 40 || c.ShadowGate == nil {
		t.Errorf("new counts = %+v", c)
	}
	if c := rows["old"].Counts; c == nil || c.Recorded90d != nil || c.OutcomesByAuthorKind != nil || c.WriteCounters != nil || c.ShadowGate != nil {
		t.Errorf("old-spoke counts must carry no panel blocks: %+v", c)
	}
	if rows["silent"].Counts != nil {
		t.Errorf("silent hive must carry no counts: %+v", rows["silent"].Counts)
	}

	// A fleet of old spokes emits none of the panel sections.
	oldOnly := buildEvidenceView([]RegistryEntry{{ID: "old", ACMMLevel: 3, Evidence: oldSpoke}})
	if oldOnly.OutcomesByAuthorKind != nil || oldOnly.WriteCounters != nil || oldOnly.ShadowGate != nil ||
		oldOnly.ByACMMLevel[0].OutcomesByAuthorKind != nil || oldOnly.ByACMMLevel[0].ShadowGate != nil {
		t.Errorf("old-spoke fleet must omit the panels: %+v", oldOnly)
	}
	raw, err := json.Marshal(oldOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"outcomes_by_author_kind"`, `"write_counters"`, `"shadow_gate"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("json carries %s although no spoke reports it", k)
		}
	}
}

// TestSanitizeEvidenceFleetPanels: unknown author kinds are dropped, subsets
// are clamped to their totals, tier names are sanitized, bounded and deduped,
// and nil blocks stay nil (an old spoke reports nothing, not zero).
func TestSanitizeEvidenceFleetPanels(t *testing.T) {
	if got := sanitizeEvidence(&HeartbeatEvidence{}); got.OutcomesByAuthorKind != nil || got.WriteCounters != nil || got.ShadowGate != nil {
		t.Fatalf("nil blocks must stay nil: %+v", got)
	}
	tiers := map[string]EvidenceWriteCount{"  guarded ": {Attempted: 10, Blocked: 20}, "guarded": {Attempted: 1}, "": {Attempted: 5}, "bad\x00tier": {Attempted: -4, Blocked: -1}}
	for i := 0; i < maxEvidenceWriteTiers+5; i++ {
		tiers[fmt.Sprintf("tier-%02d", i)] = EvidenceWriteCount{Attempted: 1}
	}
	got := sanitizeEvidence(&HeartbeatEvidence{
		OutcomesByAuthorKind: map[string]EvidenceOutcome{
			"agent":   {N: 5, Merged: 9, Reverted: 9},
			"martian": {N: 1},
			"bot":     {N: -1, Merged: 1},
		},
		WriteCounters: tiers,
		ShadowGate:    &EvidenceShadow{Intent: EvidenceShadowGate{Held: EvidenceOutcome{N: 2, Merged: 5, Reverted: 7}}},
	})
	if len(got.OutcomesByAuthorKind) != 2 || got.OutcomesByAuthorKind["agent"] != (EvidenceOutcome{N: 5, Merged: 5, Reverted: 5}) ||
		got.OutcomesByAuthorKind["bot"] != (EvidenceOutcome{}) {
		t.Errorf("outcomes = %+v", got.OutcomesByAuthorKind)
	}
	if _, ok := got.OutcomesByAuthorKind["martian"]; ok {
		t.Error("unknown author kind kept")
	}
	if len(got.WriteCounters) != maxEvidenceWriteTiers {
		t.Errorf("tiers = %d, want capped at %d", len(got.WriteCounters), maxEvidenceWriteTiers)
	}
	if _, ok := got.WriteCounters[""]; ok {
		t.Error("empty tier kept")
	}
	// Sorted key order: "" < "  guarded " < "bad.." < "guarded" < "tier-*",
	// so the trimmed "  guarded " wins and the later "guarded" is a dup.
	if g := got.WriteCounters["guarded"]; g != (EvidenceWriteCount{Attempted: 10, Blocked: 10}) {
		t.Errorf("guarded = %+v, want blocked clamped to attempted", g)
	}
	for k := range got.WriteCounters {
		if !safeNamePattern.MatchString(k) {
			t.Errorf("tier key %q outside the safe name alphabet kept", k)
		}
	}
	if got.ShadowGate == nil || got.ShadowGate.Intent.Held != (EvidenceOutcome{N: 2, Merged: 2, Reverted: 2}) || got.ShadowGate.ReviewGate != (EvidenceShadowGate{}) {
		t.Errorf("shadow = %+v", got.ShadowGate)
	}
	if got := sanitizeEvidence(&HeartbeatEvidence{OutcomesByAuthorKind: map[string]EvidenceOutcome{"martian": {N: 1}}, WriteCounters: map[string]EvidenceWriteCount{"": {}}}); got.OutcomesByAuthorKind != nil || got.WriteCounters != nil {
		t.Errorf("all-dropped blocks must collapse to nil: %+v", got)
	}
}

// TestParseEvidenceWindow pins hours= / from= / to= handling for the hub
// evidence window selector (RFC #12062). Custom ranges are served from the
// smallest reported spoke window that reaches back to `from`.
func TestParseEvidenceWindow(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ok := []struct {
		q                string
		hours            int
		key              string
		approx, coverage bool
	}{
		{"", 2160, "", false, true},
		{"hours=0", 2160, "", false, true},
		{"hours=24", 24, "24h", false, false},
		{"hours=168", 168, "7d", false, false},
		{"hours=720", 720, "30d", false, false},
		{"hours=100", 2160, "", false, true},
		{"from=2026-10-16", 24, "24h", true, false},
		{"from=2026-10-12&to=2026-10-14", 168, "7d", true, false},
		{"from=2026-09-20T00:00:00Z", 720, "30d", true, false},
		{"from=2026-05-01&to=2026-10-01", 2160, "", true, true},
	}
	for _, tc := range ok {
		q, _ := url.ParseQuery(tc.q)
		win, err := parseEvidenceWindow(q, now)
		if err != nil {
			t.Errorf("%q: %v", tc.q, err)
			continue
		}
		if win.Hours != tc.hours || win.Key != tc.key || win.Approximate != tc.approx || win.Coverage != tc.coverage {
			t.Errorf("%q = %+v, want hours=%d key=%q approx=%v coverage=%v", tc.q, win, tc.hours, tc.key, tc.approx, tc.coverage)
		}
		if tc.approx && (win.From == "" || win.To == "" || win.Label == "") {
			t.Errorf("%q: custom window missing echo: %+v", tc.q, win)
		}
	}
	bad := []string{"hours=x", "hours=-1", "to=2026-10-01", "from=yesterday", "from=2026-10-10&to=nope",
		"from=2026-10-10&to=2026-10-09", "from=2025-01-01&to=2026-10-01"}
	for _, q := range bad {
		v, _ := url.ParseQuery(q)
		if _, err := parseEvidenceWindow(v, now); err == nil {
			t.Errorf("%q: want error", q)
		}
	}
}

// TestBuildEvidenceViewWindow pins per-window pooling: the selected spoke
// window block replaces the 90-day block, hives lacking it are flagged, and
// record coverage (a 90-day-only ratio) is withheld.
func TestBuildEvidenceViewWindow(t *testing.T) {
	merged := 40
	hives := []RegistryEntry{
		{ID: "new", PRsMerged90d: &merged, Evidence: &HeartbeatEvidence{Verdicts90d: 30, FalseApprove90d: 3, Merged90d: 20,
			Windows: map[string]*HeartbeatEvidence{"7d": {Verdicts90d: 12, FalseApprove90d: 1, Merged90d: 5}}}},
		{ID: "old", PRsMerged90d: &merged, Evidence: &HeartbeatEvidence{Verdicts90d: 50, Merged90d: 30}},
		{ID: "none"},
	}
	all := buildEvidenceView(hives)
	if all.Window.Hours != evidenceDefaultWindowHours || all.Reporting != 2 || all.WindowUnreported != 0 || all.RecordCoverage.N != 80 {
		t.Fatalf("90d view = %+v", all)
	}
	week := buildEvidenceViewWindow(hives, evidenceWindowFor(168))
	if week.Window.Key != "7d" || week.Reporting != 1 || week.WindowUnreported != 1 || week.RecordCoverage.N != 0 {
		t.Fatalf("7d view = %+v", week)
	}
	byID := map[string]EvidenceHive{}
	for _, h := range week.Hives {
		byID[h.ID] = h
	}
	if h := byID["new"]; !h.Reported || h.WindowUnreported || h.Counts == nil || h.Counts.Verdicts90d != 12 || h.Counts.Merged90d != 5 || h.Counts.Recorded90d != nil {
		t.Errorf("new = %+v counts=%+v", h, h.Counts)
	}
	if h := byID["old"]; h.Reported || !h.WindowUnreported {
		t.Errorf("old = %+v", h)
	}
	if h := byID["none"]; h.Reported || h.WindowUnreported {
		t.Errorf("none = %+v", h)
	}

	srv := newHubServerForTest(t)
	srv.registry.Hives = hives
	w := httptest.NewRecorder()
	srv.handleEvidence(w, httptest.NewRequest("GET", evidencePath+"?hours=168", nil))
	var got EvidenceView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Window.Key != "7d" || got.Window.Label != "last 7d" {
		t.Fatalf("hours=168: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.handleEvidence(w, httptest.NewRequest("GET", evidencePath+"?from=2026-10-10&to=2026-10-01", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("inverted range status = %d", w.Code)
	}
}

// TestSanitizeEvidenceWindows pins that nested windows are bounded like the
// 90-day block and flattened to one level.
func TestSanitizeEvidenceWindows(t *testing.T) {
	in := &HeartbeatEvidence{Verdicts90d: 5, Windows: map[string]*HeartbeatEvidence{
		"24h":   {Verdicts90d: -3, FalseApprove90d: 9, Windows: map[string]*HeartbeatEvidence{"7d": {}}},
		"bogus": {Verdicts90d: 1},
	}}
	out := sanitizeEvidence(in)
	if len(out.Windows) != 1 || out.Windows["24h"] == nil || out.Windows["24h"].Windows != nil {
		t.Fatalf("windows = %+v", out.Windows)
	}
	if w := out.Windows["24h"]; w.Verdicts90d != 0 || w.FalseApprove90d != 0 {
		t.Errorf("24h not bounded: %+v", w)
	}
	if sanitizeEvidence(&HeartbeatEvidence{}).Windows != nil {
		t.Error("empty windows should stay nil")
	}
}
