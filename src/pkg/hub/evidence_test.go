package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func storedEvidence(t *testing.T, s *HubServer, id string) *HeartbeatEvidence {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h.Evidence
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return nil
}

func TestHeartbeatEvidence_StoredSanitized(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{"hive_id":"h1","org":"hivecommons","evidence":{"verdicts_90d":20,"false_approve_90d":99,"false_block_90d":-3,` +
		`"merged_90d":5,"reverted_14d_90d":9,"automerge_self_authored":true,` +
		`"reviewer_families":[" copilot ","copilot","","claude"],"calibration":[{"n":4,"bad":8}]}}`
	if rec := postHeartbeat(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	e := storedEvidence(t, s, "h1")
	if e == nil {
		t.Fatal("evidence not stored")
	}
	if e.FalseApprove90d != 20 || e.FalseBlock90d != 0 || e.Reverted14d90d != 5 || e.Calibration[0].Bad != 4 {
		t.Fatalf("stored evidence not clamped to its totals: %+v", e)
	}
	if e.AutomergeSelfAuthored == nil || !*e.AutomergeSelfAuthored {
		t.Fatalf("automerge_self_authored lost: %+v", e.AutomergeSelfAuthored)
	}
	if len(e.ReviewerFamilies) != 2 || e.ReviewerFamilies[0] != "copilot" || e.ReviewerFamilies[1] != "claude" {
		t.Fatalf("families not trimmed/deduped: %v", e.ReviewerFamilies)
	}
}

func TestHeartbeatEvidence_CarryForward(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons","evidence":{"verdicts_90d":12,"false_block_90d":2,"merged_90d":7}}`)

	// A beat with no evidence key (old spoke / minimal beat) keeps the summary.
	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons"}`)
	if e := storedEvidence(t, s, "h1"); e == nil || e.Verdicts90d != 12 || e.FalseBlock90d != 2 {
		t.Fatalf("evidence not carried forward: %+v", e)
	}

	// A fresh summary replaces the stored one rather than merging with it.
	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons","evidence":{"verdicts_90d":30,"merged_90d":9}}`)
	if e := storedEvidence(t, s, "h1"); e == nil || e.Verdicts90d != 30 || e.FalseBlock90d != 0 {
		t.Fatalf("evidence not replaced by the new beat: %+v", e)
	}
}

// TestRegistryOmitsEvidence pins the owner/admin-only contract on the public,
// unauthenticated registry: the per-hive counts must not leak there even
// though they live on RegistryEntry so saveRegistryNow can persist them.
func TestRegistryOmitsEvidence(t *testing.T) {
	srv := newHubServerForTest(t)
	yes := true
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID: "public-1", IsPublic: true, Online: true,
		LastHeartbeat: time.Now().Format(time.RFC3339),
		Evidence:      &HeartbeatEvidence{Verdicts90d: 20, FalseBlock90d: 5, AutomergeSelfAuthored: &yes},
	}}
	srv.mu.Unlock()

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/registry", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"evidence"`) {
		t.Fatalf("public registry leaked the evidence summary: %s", w.Body.String())
	}
	// The strip is on the response copy only; the stored entry still has it.
	if e := storedEvidence(t, srv, "public-1"); e == nil || e.Verdicts90d != 20 {
		t.Fatalf("stored evidence was mutated by the handler: %+v", e)
	}
}

// TestMyHivesEvidenceOwnerOnly pins the row-level rule on /api/saas/my-hives:
// an owner sees their hive's evidence summary, a read-only member sees the
// scored card but not the counts behind it.
func TestMyHivesEvidenceOwnerOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	cleanupOwner := helperSetupAuthUser(t, "ghp_evidence_owner", "evidence-owner")
	defer cleanupOwner()
	cleanupMember := helperSetupAuthUser(t, "ghp_evidence_member", "evidence-member")
	defer cleanupMember()
	saveSaaSUser(&SaaSUser{GitHubUsername: "evidence-member", Hives: map[string]string{"h-ev": "read"}})

	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID: "h-ev", Name: "acme/widget", Owner: "evidence-owner", Online: true,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		Evidence:      &HeartbeatEvidence{Verdicts90d: 20, FalseBlock90d: 5},
	}}
	srv.mu.Unlock()

	fetch := func(token string) MyHiveEntry {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/saas/my-hives", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.handleMyHives(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("my-hives status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Hives []MyHiveEntry `json:"hives"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, h := range resp.Hives {
			if h.ID == "h-ev" {
				return h
			}
		}
		t.Fatalf("hive h-ev missing from response: %s", rec.Body.String())
		return MyHiveEntry{}
	}

	if got := fetch("ghp_evidence_owner"); got.Evidence == nil || got.Evidence.Verdicts90d != 20 {
		t.Fatalf("owner must see the evidence summary: %+v", got.Evidence)
	}
	if got := fetch("ghp_evidence_member"); got.Evidence != nil {
		t.Fatalf("read-only member must not see the evidence summary: %+v", got.Evidence)
	}
}
