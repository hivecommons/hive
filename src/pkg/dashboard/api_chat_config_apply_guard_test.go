package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Phase 3b requirements (#19985): owner-only Apply checked server-side, the
// Apply's own allowlist (no seed-owned or secret key), the handler's own
// validation and strict save, and fingerprinted Apply and Undo audit lines.

const chatConfigCadencesPath = "/api/config/agent/scanner/cadences"

// chatConfigInjectProposal stores a pending proposal of the default user
// ("local") bound to the scanner agent as it is now, as if the turn had made
// it, so the Apply's own checks can be driven past the proposal-time ones.
func chatConfigInjectProposal(t *testing.T, s *Server, section, field string, body map[string]any) string {
	t.Helper()
	hash, ok := s.chatConfigCurrentHash("scanner")
	if !ok {
		t.Fatal("no scanner agent")
	}
	id := newChatConfigProposalID()
	now := time.Now()
	p := &chatConfigProposal{
		ID: id, User: "local", Agent: "scanner", Section: section, Field: field,
		Body: body, Hash: hash, Expires: now.Add(chatConfigProposalTTL),
	}
	if err := s.chatConfigProposals.add(p, now); err != nil {
		t.Fatalf("add proposal: %v", err)
	}
	return id
}

// chatConfigApplyAs is an Apply PUT with an explicit role, verified or not.
func chatConfigApplyAs(s *Server, path, role string, verified bool, id string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	req := httptest.NewRequest(http.MethodPut, path, &b)
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	req.Header.Set(chatConfigProposalHeader, id)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func chatConfigAuditEntries(s *Server, action string) []AuditEntry {
	var out []AuditEntry
	for _, e := range s.audit.Recent(200) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func chatConfigProposalPending(s *Server, id string) bool {
	_, ok := s.chatConfigProposals.lookup(id, "local", time.Now())
	return ok
}

// A non-owner cannot Apply or Undo, whatever the UI shows: the server refuses
// a viewer, a merger, and an owner role without the verification marker, and
// the proposal stays usable by the owner.
func TestChatConfigApply_NonOwnerRefused(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	body := map[string]any{"idle": 1800}
	cases := []struct {
		name     string
		role     string
		verified bool
	}{
		{"no role", "", false},
		{"read", config.RoleRead, false},
		{"read-write", config.RoleReadWrite, false},
		{"merger", config.RoleMerger, false},
		{"unverified owner", config.RoleOwner, false},
	}
	for _, c := range cases {
		if rec := chatConfigApplyAs(s, chatConfigCadencesPath, c.role, c.verified, id, body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s apply: %d %s, want 403", c.name, rec.Code, rec.Body.String())
		}
		if got := chatConfigIdleCadence(s); got != "15m" {
			t.Fatalf("%s apply changed the cadence to %q", c.name, got)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/chat/config-proposals/"+id+"/undo", nil)
		if c.role != "" {
			req.Header.Set("X-Hive-Role", c.role)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s undo: %d, want 403", c.name, rec.Code)
		}
	}
	if !chatConfigProposalPending(s, id) {
		t.Fatal("a refused non-owner Apply consumed the proposal")
	}
	if rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, body); rec.Code != http.StatusOK {
		t.Fatalf("owner apply after refusals: %d %s", rec.Code, rec.Body.String())
	}
}

// The binding check itself refuses a non-owner too, not only the section
// handler's role gate in front of it.
func TestChatConfigApplyBegin_RequiresVerifiedOwner(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	req := httptest.NewRequest(http.MethodPut, chatConfigCadencesPath, nil)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(chatConfigProposalHeader, id)
	rec := httptest.NewRecorder()
	s.agentConfigMu.Lock()
	apply, ok := s.chatConfigApplyBegin(rec, req, "scanner", "cadences", map[string]any{"idle": 1800})
	s.agentConfigMu.Unlock()
	if ok || apply != nil || rec.Code != http.StatusForbidden {
		t.Fatalf("unverified owner: ok=%v apply=%v code=%d, want refused 403", ok, apply, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "owner") {
		t.Fatalf("refusal = %q, want an owner-only message", rec.Body.String())
	}
	if !chatConfigProposalPending(s, id) {
		t.Fatal("refused Apply consumed the proposal")
	}
}

// The Apply runs the section handler's own validation: a body that fails it
// is a 400, changes nothing, is audited, and leaves the proposal pending.
func TestChatConfigApply_ValidationFailure(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	body := map[string]any{"idle": true}
	id := chatConfigInjectProposal(t, s, "cadences", "idle", body)
	rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid apply: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("invalid apply changed the cadence to %q", got)
	}
	d := agentConfigAuditDetail(t, s, "chat_config_apply_failed")
	if !strings.Contains(d, "proposal="+id) || !strings.Contains(d, "status=400") || !strings.Contains(d, "invalid cadence") {
		t.Fatalf("apply_failed detail = %q", d)
	}
	if len(chatConfigAuditEntries(s, "chat_config_applied")) != 0 {
		t.Fatal("chat_config_applied written for a refused Apply")
	}
	if !chatConfigProposalPending(s, id) {
		t.Fatal("a failed Apply did not return the proposal to pending")
	}
}

// #20079: a failed save is an error, not a 200. The Apply surfaces it the same
// way: non-2xx to the card, nothing left in memory, an apply_failed line, and
// the proposal back to pending so a retry after the fix works.
func TestChatConfigApply_FailedSaveSurfaced(t *testing.T) {
	s, deps, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	body := map[string]any{"idle": 1800}

	src, runtimeFile, overlayFile := deps.Config.SourcePath, config.RuntimeConfigFile, config.DashboardOverlayFile
	unwritableConfig(t, deps.Config)
	rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, body)
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("apply with a failing save: %d %s, want an error status", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("failed save left cadence %q in memory, want 15m", got)
	}
	d := agentConfigAuditDetail(t, s, "chat_config_apply_failed")
	if !strings.Contains(d, "proposal="+id) || !strings.Contains(d, "status=") || strings.Contains(d, "status=200") {
		t.Fatalf("apply_failed detail = %q", d)
	}
	if len(chatConfigAuditEntries(s, "chat_config_applied")) != 0 {
		t.Fatal("chat_config_applied written for a failed save")
	}
	if !chatConfigProposalPending(s, id) {
		t.Fatal("a failed save did not return the proposal to pending")
	}

	deps.Config.SourcePath, config.RuntimeConfigFile, config.DashboardOverlayFile = src, runtimeFile, overlayFile
	if rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, body); rec.Code != http.StatusOK {
		t.Fatalf("retry after the save is fixed: %d %s", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "30m" {
		t.Fatalf("cadence = %q after retry, want 30m", got)
	}
}

// Seed-owned keys and secrets are refused by the Apply's own allowlist with
// 403, even when such a proposal sits in the store, and the proposal is
// dropped. A body that smuggles one next to an allowlisted field is refused
// too.
func TestChatConfigApply_SeedAndSecretKeysRefused(t *testing.T) {
	s, deps, _ := chatConfigServer(t, chatConfigIdleAnswer)
	generalPath := "/api/config/agent/scanner/general"
	wasEnabled := deps.Config.Agents["scanner"].Enabled
	cases := []struct {
		name  string
		field string
		body  map[string]any
	}{
		{"github", "github", map[string]any{"github": map[string]any{"app_id": 1}}},
		{"hub", "hub", map[string]any{"hub": map[string]any{"url": "https://example.invalid"}}},
		{"variables", "variables", map[string]any{"variables": map[string]any{"x": "y"}}},
		{"acmm_level", "acmm_level", map[string]any{"acmm_level": 6}},
		{"hive_id", "hive_id", map[string]any{"hive_id": "other"}},
		{"secret field", "api_token", map[string]any{"api_token": "not-a-real-value"}},
		{"smuggled seed key", "enabled", map[string]any{"enabled": false, "hub": map[string]any{}}},
		{"smuggled secret", "enabled", map[string]any{"enabled": false, "webhook_secret": "x"}},
	}
	for _, c := range cases {
		id := chatConfigInjectProposal(t, s, "general", c.field, c.body)
		rec := chatConfigApplyPut(s, generalPath, "", id, c.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s, want 403", c.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "cannot be changed from chat") {
			t.Fatalf("%s: refusal = %q, want a clear message", c.name, rec.Body.String())
		}
		if _, err := s.chatConfigProposals.claim(id, "local", time.Now()); err != errChatConfigNoProposal {
			t.Fatalf("%s: refused proposal still in the store (claim err %v)", c.name, err)
		}
		if deps.Config.Agents["scanner"].Enabled != wasEnabled {
			t.Fatalf("%s: agent config changed", c.name)
		}
	}
	if got := len(chatConfigAuditEntries(s, "chat_config_apply_failed")); got != len(cases) {
		t.Fatalf("chat_config_apply_failed lines = %d, want %d", got, len(cases))
	}
	for _, e := range chatConfigAuditEntries(s, "chat_config_apply_failed") {
		if strings.Contains(e.Detail, "not-a-real-value") {
			t.Fatalf("audit leaked a secret value: %q", e.Detail)
		}
	}
}

func TestChatConfigApplyFieldAllowed(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	ok := []struct{ section, field string }{
		{"models", "model"}, {"models", "reasoning_effort"}, {"cadences", "idle"}, {"general", "enabled"},
	}
	for _, c := range ok {
		if err := s.chatConfigApplyFieldAllowed(c.section, c.field, map[string]any{c.field: "v"}); err != nil {
			t.Errorf("%s.%s refused: %v", c.section, c.field, err)
		}
	}
	refused := []struct {
		section, field string
		body           map[string]any
	}{
		{"models", "backend", map[string]any{"backend": "codex"}},
		{"cadences", "no_such_mode", map[string]any{"no_such_mode": 60}},
		{"general", "prompt", map[string]any{"prompt": "x"}},
		{"hooks", "pre", map[string]any{"pre": "x"}},
		{"general", "enabled", map[string]any{}},
		{"general", "enabled", map[string]any{"enabled": false, "model": "x"}},
		{"general", "password", map[string]any{"password": "x"}},
		{"github", "app_id", map[string]any{"app_id": 1}},
	}
	for _, c := range refused {
		if err := s.chatConfigApplyFieldAllowed(c.section, c.field, c.body); err == nil {
			t.Errorf("%s.%s %v allowed, want refused", c.section, c.field, c.body)
		}
	}
	for _, k := range []string{"github", "hub", "variables", "acmm_level", "hive_id"} {
		if !config.IsSeedOwnedKey(k) {
			t.Errorf("%s must be seed-owned", k)
		}
	}
	if config.IsSeedOwnedKey("agents") {
		t.Error("agents is not seed-owned")
	}
}

// Every Apply and Undo is audited with who, which key, and old and new value
// fingerprints; the values themselves are not in those lines.
func TestChatConfigApply_AuditFingerprints(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	if rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	applied := chatConfigAuditEntries(s, "chat_config_applied")
	if len(applied) != 1 {
		t.Fatalf("chat_config_applied lines = %d, want 1", len(applied))
	}
	a := applied[0]
	wantFP := func(d, before, after string) {
		t.Helper()
		for _, want := range []string{
			"section=cadences", "field=idle",
			"before_fp=" + chatConfigValueFingerprint(before),
			"after_fp=" + chatConfigValueFingerprint(after),
		} {
			if !strings.Contains(d, want) {
				t.Errorf("detail %q does not contain %q", d, want)
			}
		}
		if strings.Contains(d, "=15m") || strings.Contains(d, "=30m") || strings.Contains(d, " -> ") {
			t.Errorf("detail %q carries a raw value", d)
		}
	}
	if a.User != "local" || a.Agent != "scanner" || !strings.Contains(a.Detail, "proposal="+id) {
		t.Fatalf("applied entry = %+v", a)
	}
	wantFP(a.Detail, "15m", "30m")

	rev := decodeJSON(t, chatConfigUndo(t, s, id))
	revID := chatConfigProposalID(t, rev)
	body, _ := rev["config_proposal"].(map[string]any)["body"].(map[string]any)
	if rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", revID, body); rec.Code != http.StatusOK {
		t.Fatalf("undo apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("cadence = %q after undo, want the exact previous 15m", got)
	}
	undo := chatConfigAuditEntries(s, "chat_config_undo")
	if len(undo) != 1 {
		t.Fatalf("chat_config_undo lines = %d, want 1", len(undo))
	}
	if undo[0].User != "local" || !strings.Contains(undo[0].Detail, "reverses="+id) {
		t.Fatalf("undo entry = %+v", undo[0])
	}
	wantFP(undo[0].Detail, "30m", "15m")
	// The undo's own Apply is not double-counted as a forward Apply.
	if got := len(chatConfigAuditEntries(s, "chat_config_applied")); got != 1 {
		t.Fatalf("chat_config_applied lines = %d after undo, want 1", got)
	}
}

func TestChatConfigValueFingerprint(t *testing.T) {
	if got := chatConfigValueFingerprint(""); got != auditUnset {
		t.Fatalf("unset fingerprint = %q", got)
	}
	a, b := chatConfigValueFingerprint("30m"), chatConfigValueFingerprint("15m")
	if a == b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+chatConfigFingerprintLen || strings.Contains(a, "30m") {
		t.Fatalf("fingerprints %q, %q", a, b)
	}
	if chatConfigValueFingerprint("30m") != a {
		t.Fatal("fingerprint is not stable")
	}
}

// After the store dropped the applied proposal (restart or TTL), Undo from the
// audit log still refuses when the section was written after the Apply, even
// if the field reads the same again: restoring the old value would overwrite
// a newer, deliberate change.
func TestChatConfigUndo_FromAuditRefusesAfterInterveningWrite(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	if rec := chatConfigApplyPut(s, chatConfigCadencesPath, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	for _, secs := range []int{600, 1800} {
		if rec := chatConfigPut(s, chatConfigCadencesPath, "", map[string]any{"idle": secs}); rec.Code != http.StatusOK {
			t.Fatalf("dialog save %d: %d", secs, rec.Code)
		}
	}
	if got := chatConfigIdleCadence(s); got != "30m" {
		t.Fatalf("cadence = %q, want 30m again", got)
	}
	s.chatConfigProposals.mu.Lock()
	s.chatConfigProposals.byID = nil
	s.chatConfigProposals.mu.Unlock()

	rec := chatConfigUndo(t, s, id)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since") {
		t.Fatalf("stale undo from audit: %d %s, want 409 with a clear message", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "30m" {
		t.Fatalf("stale undo changed the cadence to %q", got)
	}
}

// An in-flight proposal is not swept at its expiry: its Apply settles it.
func TestChatConfigProposalStore_InFlightNotSwept(t *testing.T) {
	var st chatConfigProposalStore
	now := time.Now()
	if err := st.add(&chatConfigProposal{ID: "p1", User: "alice", Expires: now.Add(time.Second)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.claim("p1", "alice", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	later := now.Add(time.Minute)
	st.mu.Lock()
	st.sweepLocked(later)
	st.mu.Unlock()
	st.markApplied("p1", "h", later)
	if p, ok := st.applied("p1", "alice", later); !ok || p.AppliedHash != "h" {
		t.Fatalf("applied after a slow Apply = %+v, %v", p, ok)
	}
}
