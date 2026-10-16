package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	findCriterion(t, efficiencyCriteria(in), "revert_rate")
	findCriterion(t, satisfactionCriteria(in), "human_override_rate")
	if got := satisfactionCriteria(quadrantInputs{evidence: &HeartbeatEvidence{Verdicts90d: 9}}); len(got) != 0 {
		t.Fatalf("<10 verdicts must be unscored: %v", got)
	}
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
