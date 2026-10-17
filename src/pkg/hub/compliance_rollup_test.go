package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func storedCompliance(t *testing.T, s *HubServer, id string) *HeartbeatCompliance {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h.Compliance
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return nil
}

func TestHeartbeatCompliance_StoredAndSanitized(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{"hive_id":"h1","org":"hivecommons","compliance":{"frameworks":[" soc2-type2 ","soc2-type2","","hipaa"],"posture":{"pass":9,"fail":-4,"lastRun":"2026-10-01T12:00:00Z"}}}`
	if rec := postHeartbeat(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	c := storedCompliance(t, s, "h1")
	if c == nil || len(c.Frameworks) != 2 || c.Frameworks[0] != "soc2-type2" || c.Frameworks[1] != "hipaa" {
		t.Fatalf("frameworks not deduped/trimmed: %+v", c)
	}
	if c.Posture == nil || c.Posture.Pass != 9 || c.Posture.Fail != 0 || c.Posture.LastRun != "2026-10-01T12:00:00Z" {
		t.Fatalf("posture not stored/clamped: %+v", c.Posture)
	}
}

func TestHeartbeatCompliance_CarryForwardAndClear(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons","compliance":{"frameworks":["soc2-type2"],"posture":{"pass":3,"fail":1}}}`)

	// A beat with no compliance key (old spoke / minimal beat) keeps the profile.
	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons"}`)
	if c := storedCompliance(t, s, "h1"); c == nil || len(c.Frameworks) != 1 {
		t.Fatalf("profile not carried forward: %+v", c)
	}

	// An empty block means compliance was unconfigured and clears the profile.
	postHeartbeat(t, s, `{"hive_id":"h1","org":"hivecommons","compliance":{}}`)
	if c := storedCompliance(t, s, "h1"); c == nil || len(c.Frameworks) != 0 || c.Posture != nil {
		t.Fatalf("profile not cleared: %+v", c)
	}
}

func TestSanitizeCompliance_Bounds(t *testing.T) {
	if sanitizeCompliance(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	in := &HeartbeatCompliance{Posture: &CompliancePosture{Pass: 5}}
	for i := 0; i < maxComplianceFrameworks+5; i++ {
		in.Frameworks = append(in.Frameworks, strings.Repeat("a", maxComplianceFrameworkRunes+10)+string(rune('a'+i%26))+strings.Repeat("b", i))
	}
	out := sanitizeCompliance(in)
	if len(out.Frameworks) > maxComplianceFrameworks {
		t.Errorf("frameworks = %d, want <= %d", len(out.Frameworks), maxComplianceFrameworks)
	}
	for _, f := range out.Frameworks {
		if len([]rune(f)) > maxComplianceFrameworkRunes {
			t.Errorf("framework not clipped: %d runes", len([]rune(f)))
		}
	}
	if out := sanitizeCompliance(&HeartbeatCompliance{Posture: &CompliancePosture{Pass: 1}}); out.Posture != nil {
		t.Error("posture kept for a spoke with no frameworks")
	}
	out = sanitizeCompliance(&HeartbeatCompliance{Frameworks: []string{"x"}, Posture: &CompliancePosture{Pass: 1 << 40, LastRun: "garbage"}})
	if out.Posture.Pass != maxCompliancePostureCount || out.Posture.LastRun != "" {
		t.Errorf("posture not clamped: %+v", out.Posture)
	}
}

func TestBuildComplianceRollup(t *testing.T) {
	hives := []RegistryEntry{
		{ID: "b", Name: "B", Online: true, DashboardURL: "https://b.example", Compliance: &HeartbeatCompliance{
			Frameworks: []string{"soc2-type2"}, Posture: &CompliancePosture{Pass: 8, Fail: 2, LastRun: "2026-10-01T12:00:00Z"}}},
		{ID: "a", Compliance: &HeartbeatCompliance{Frameworks: []string{"soc2-type2", "hipaa"}}},
		{ID: "c", Compliance: &HeartbeatCompliance{}},
		{ID: "d"},
	}
	r := buildComplianceRollup(hives)
	if r.Spokes != 4 || r.ConfiguredSpokes != 2 {
		t.Fatalf("spokes/configured = %d/%d, want 4/2", r.Spokes, r.ConfiguredSpokes)
	}
	if r.Pass != 8 || r.Fail != 2 || r.PassRate == nil || *r.PassRate != 80 {
		t.Fatalf("totals = %d/%d rate=%v, want 8/2 80", r.Pass, r.Fail, r.PassRate)
	}
	if len(r.Frameworks) != 2 || r.Frameworks[0].Framework != "hipaa" || r.Frameworks[0].Spokes != 1 || r.Frameworks[1].Framework != "soc2-type2" || r.Frameworks[1].Spokes != 2 {
		t.Fatalf("frameworks = %+v", r.Frameworks)
	}
	if len(r.Hives) != 2 || r.Hives[0].ID != "a" || r.Hives[0].PassRate != nil || r.Hives[1].PassRate == nil || *r.Hives[1].PassRate != 80 {
		t.Fatalf("hives = %+v", r.Hives)
	}
	if empty := buildComplianceRollup(nil); empty.PassRate != nil || empty.Frameworks == nil || empty.Hives == nil {
		t.Fatalf("empty rollup = %+v", empty)
	}
}

func TestComplianceRollupEndpoint(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.registry.Hives = []RegistryEntry{{ID: "h1", Compliance: &HeartbeatCompliance{
		Frameworks: []string{"soc2-type2"}, Posture: &CompliancePosture{Pass: 1, Fail: 1}}}}

	handler := srv.requireAdmin(srv.handleComplianceRollup)
	req := httptest.NewRequest("GET", complianceRollupPath, nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "regularuser"})
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	srv.handleComplianceRollup(w, httptest.NewRequest("GET", complianceRollupPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got ComplianceRollup
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ConfiguredSpokes != 1 || got.PassRate == nil || *got.PassRate != 50 {
		t.Fatalf("rollup = %+v", got)
	}
}

// TestRegistryOmitsCompliance pins the owner/admin-only contract on the public,
// unauthenticated registry: the compliance profile must not leak there even
// though it lives on RegistryEntry so saveRegistryNow can persist it.
func TestRegistryOmitsCompliance(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID: "public-1", IsPublic: true, Online: true,
		LastHeartbeat: time.Now().Format(time.RFC3339),
		Compliance: &HeartbeatCompliance{
			Frameworks: []string{"soc2-type2"},
			Posture:    &CompliancePosture{Pass: 5, Fail: 1},
		},
	}}
	srv.mu.Unlock()

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/registry", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"compliance"`) {
		t.Fatalf("public registry leaked the compliance summary: %s", w.Body.String())
	}
	// The strip is on the response copy only; the stored entry still has it.
	if c := storedCompliance(t, srv, "public-1"); c == nil || len(c.Frameworks) != 1 {
		t.Fatalf("stored compliance was mutated by the handler: %+v", c)
	}
}

// TestMyHivesComplianceOwnerOnly pins the row-level rule on /api/saas/my-hives:
// an owner sees their hive's compliance summary, a read-only member does not.
func TestMyHivesComplianceOwnerOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	cleanupOwner := helperSetupAuthUser(t, "ghp_compliance_owner", "compliance-owner")
	defer cleanupOwner()
	cleanupMember := helperSetupAuthUser(t, "ghp_compliance_member", "compliance-member")
	defer cleanupMember()
	saveSaaSUser(&SaaSUser{GitHubUsername: "compliance-member", Hives: map[string]string{"h-comp": "read"}})

	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID: "h-comp", Name: "acme/widget", Owner: "compliance-owner", Online: true,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		Compliance: &HeartbeatCompliance{
			Frameworks: []string{"soc2-type2"},
			Posture:    &CompliancePosture{Pass: 8, Fail: 2},
		},
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
			if h.ID == "h-comp" {
				return h
			}
		}
		t.Fatalf("hive h-comp missing from response: %s", rec.Body.String())
		return MyHiveEntry{}
	}

	if got := fetch("ghp_compliance_owner"); got.Compliance == nil || len(got.Compliance.Frameworks) != 1 {
		t.Fatalf("owner must see the compliance summary: %+v", got.Compliance)
	}
	if got := fetch("ghp_compliance_member"); got.Compliance != nil {
		t.Fatalf("read-only member must not see the compliance summary: %+v", got.Compliance)
	}
}
