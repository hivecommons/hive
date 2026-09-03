package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #2534 — Operator admin controls. Originally in a single "Management & Operations"
// tab; that tab is now split into "Management" (the admin CONTROLS block below) and
// "Operations" (monitoring + the per-clanker controls). The gating is unchanged and
// applies to controls in BOTH tabs.
//
// These tests pin two things:
//   1. The UI: the /contribute page must ship the (initially hidden) admin-controls
//      markup that mirrors the Governor Hub config — suspend/skip toggles, the
//      admission-filter editors, and the per-contributor action wiring — plus the
//      /api/role gate and the themed confirm modal. Rendering is gated CLIENT-side
//      by initAdmin() reading /api/role; a read viewer never enables it. The SAME
//      gate (adminEnabled) governs the per-clanker controls now under Operations.
//   2. The server boundary: the contributor mutation endpoints the tab drives
//      (trust / revoke / delete) must reject a "read" viewer with 403 and allow
//      owner + read-write — because roleEnforcement EXEMPTS the /api/contribute*
//      path prefix, so hiding in the UI is not the boundary.

// TestOpsTabHasAdminControlsMarkup asserts the admin-controls area, the filter
// editors, the /api/role gate, and the themed confirm modal are all present in
// the rendered /contribute page, and that the read-only-only display panels from
// #2542/#2558 remain intact.
func TestOpsTabHasAdminControlsMarkup(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Admin controls area + the role gate that only shows it for owner/read-write.
	for _, want := range []string{
		`id="ops-admin"`,                      // the admin card
		`class="ops-card ops-admin"`,          // hidden until enabled
		`Operator admin controls`,             // heading
		`initAdmin`,                           // gate function
		`/api/role`,                           // reads the viewer role
		`role!=='owner'&&role!=='read-write'`, // owner + read-write only
		// Suspend / skip immediate toggles (mirror the Governor Hub switches).
		`id="admin-suspend-switch"`,
		`data-key="contribute_suspended"`,
		`id="admin-skip-switch"`,
		`data-key="contribute_skip_assigned_to_others"`,
		// Admission filters — the priority controls, mirrored from Governor Hub.
		`id="admin-filter-titles"`,
		`id="admin-filter-authors"`,
		`id="admin-filter-labels"`,
		`id="admin-allow-models"`,
		`contribute_deny_titles`,
		`contribute_deny_authors`,
		`contribute_deny_labels`,
		`contribute_allow_models`,
		// Filters persist through the SAME endpoint the Governor dialog uses, and
		// read the same source the Governor dialog reads.
		`/api/config/governor/hub`,
		`/api/config/governor`,
		// Per-contributor actions wired to the EXISTING endpoints.
		`/trust`,
		`/revoke`,
		`data-role="tier"`,
		`data-role="agent-role"`,
		`Acting as`,
		`none (general work)`,
		`/agent-role`,
		`data-role="revoke"`,
		`data-role="remove"`,
		`data-role="agent-role-add"`,
		`data-role="agent-role-remove"`,
		`/agent-role-grants`,
		`Agent roles`,
		`&middot; as '+esc(c.role)`,
		`HIVE_AGENT_ROLE=scanner`,
		// Themed confirm modal (never native window.confirm).
		`id="admin-confirm-back"`,
		`function adminConfirm(`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("admin controls markup missing %q", want)
		}
	}

	// The house rule: no native confirm() in the admin flow.
	if strings.Contains(body, "window.confirm(") {
		t.Error("admin controls must use the themed modal, not native window.confirm")
	}

	// The read-only display panels from #2542/#2558 must remain intact.
	for _, want := range []string{
		`Connected clankers`,
		`id="work-list"`,
		`/api/contribute/fleet`,
		`Prompt preview`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("read-only ops panel content missing %q (must remain intact)", want)
		}
	}

	// Split placement: the admin CONTROLS block moved to the Management panel;
	// the per-clanker controls + monitoring stayed under Operations.
	iManage := strings.Index(body, `id="tab-manage"`)
	iAdmin := strings.Index(body, `id="ops-admin"`)
	iOps := strings.Index(body, `id="tab-ops"`)
	iClankerCtl := strings.Index(body, `data-role="revoke"`)
	if iManage < 0 || iAdmin < 0 || iOps < 0 || iClankerCtl < 0 {
		t.Fatalf("anchors missing: manage=%d admin=%d ops=%d clankerCtl=%d", iManage, iAdmin, iOps, iClankerCtl)
	}
	if iManage >= iAdmin || iAdmin >= iOps {
		t.Errorf("admin controls must be under Management, before Operations: manage=%d admin=%d ops=%d", iManage, iAdmin, iOps)
	}
	if iClankerCtl < iOps {
		t.Errorf("per-clanker controls must stay under Operations (after tab-ops): ops=%d clankerCtl=%d", iOps, iClankerCtl)
	}

	// One gate for both tabs: the per-clanker controls are emitted only when
	// adminEnabled is set, which initAdmin() sets solely for owner/read-write.
	// This is the same gate as the Management admin block — the split must not
	// give the Operations controls a weaker (or absent) gate.
	if !strings.Contains(body, `if(adminEnabled&&c.contributor_id){`) {
		t.Error("per-clanker controls must remain gated on adminEnabled (owner/read-write only)")
	}
	// initAdmin() must run for the Management OR the Operations tab, so a viewer
	// landing straight on Operations still resolves their role for the per-row
	// controls (and read viewers still get none).
	if !strings.Contains(body, `dp==='tab-ops'||dp==='tab-manage'`) {
		t.Error("initAdmin() must be wired to run when either the Management or Operations tab is opened")
	}
}

func TestGovernorHubDelegatableRolesUIAndRoundTrip(t *testing.T) {
	static, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	body := string(static)
	for _, want := range []string{
		"Contributor Agent Roles",
		"Delegatable to contributors",
		"toggleDelegatableAgentRole",
		"contribute_delegatable_roles",
		"scanner','quality','outreach",
		"ci-maintainer",
		"sec-check",
		"architect",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Governor Hub agent-role UI missing %q", want)
		}
	}

	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/hub", map[string]any{
		"contribute_delegatable_roles": []string{"scanner", "quality", "outreach", "sec-check", "supervisor"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config/governor/hub got %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.Hub.IsContributeRoleDelegatable("supervisor") {
		t.Fatal("supervisor must not become delegatable")
	}
	if !deps.Config.Hub.IsContributeRoleDelegatable("scanner") || !deps.Config.Hub.IsContributeRoleDelegatable("sec-check") {
		t.Fatalf("delegatable roles not persisted/resolved: %+v", deps.Config.Hub.ContributeDelegatableRoles)
	}
	got := doGet(s, "/api/config/governor")
	if got.Code != http.StatusOK {
		t.Fatalf("GET governor got %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), `"contribute_delegatable_roles"`) || !strings.Contains(got.Body.String(), `"sec-check"`) {
		t.Fatalf("GET did not round-trip delegatable roles: %s", got.Body.String())
	}
}

// contributorWriteCase is one mutation endpoint exercised by the tab.
type contributorWriteCase struct {
	name   string
	invoke func(s *Server, w http.ResponseWriter, r *http.Request)
	method string
	target string // request path (only the role gate is under test here)
	body   string
}

func contributorWriteCases() []contributorWriteCase {
	return []contributorWriteCase{
		{"trust", (*Server).handleContributorTrust, http.MethodPut, "/api/contributors/alice/trust", `{"tier":"trusted"}`},
		{"agent_role", (*Server).handleContributorAgentRole, http.MethodPut, "/api/contributors/alice/agent-role", `{"agent_role":"scanner"}`},
		{"agent_role_grants", (*Server).handleContributorAgentRoleGrants, http.MethodPut, "/api/contributors/alice/agent-role-grants", `{"agent_role_grants":["ci-maintainer"]}`},
		{"revoke", (*Server).handleContributorRevoke, http.MethodPost, "/api/contributors/alice/revoke", ``},
		{"requeue", (*Server).handleContributorRequeue, http.MethodPost, "/api/contributors/alice/requeue", ``},
		{"delete", (*Server).handleContributorDelete, http.MethodDelete, "/api/contributors/alice", ``},
	}
}

func contributorWriteRoleGateCases() []contributorWriteCase {
	return []contributorWriteCase{
		{"queue_order", (*Server).handleContributeQueueOrder, http.MethodPut, "/api/contribute/queue/order", `{"order":[]}`},
		{"queue_hold", (*Server).handleContributeQueueHold, http.MethodPost, "/api/contribute/queue/hold", `{"key":"myorg/repo1#1","held":true}`},
		{"queue_hold_clear", (*Server).handleContributeQueueHoldClear, http.MethodPost, "/api/contribute/queue/hold/clear", ``},
		{"agent_role", (*Server).handleContributorAgentRole, http.MethodPut, "/api/contributors/alice/agent-role", `{"agent_role":"scanner"}`},
		{"requeue", (*Server).handleContributorRequeue, http.MethodPost, "/api/contributors/alice/requeue", ``},
	}
}

func TestContributorWrite_HubProxiedAnonymousForbidden(t *testing.T) {
	for _, tc := range contributorWriteRoleGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContributeEnv(t)
			if err := saveContributorProfile(&ContributorProfile{
				GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted",
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			s, deps := apiServer(t)
			deps.Config.Dashboard.HubProxied = true

			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", "alice")
			w := httptest.NewRecorder()
			tc.invoke(s, w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("hub-proxied anonymous got %d, want 403 (body: %s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestContributorWrite_ReadViewerForbidden proves a "read" viewer is rejected with
// 403 on every contributor mutation endpoint the ops tab surfaces — the boundary
// the UI hiding merely shadows. roleEnforcement exempts /api/contribute*, so this
// must be enforced in-handler.
func TestContributorWrite_ReadViewerForbidden(t *testing.T) {
	for _, tc := range contributorWriteCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContributeEnv(t)
			// Seed a real contributor so a *missing role check* would 200/404, not 403.
			if err := saveContributorProfile(&ContributorProfile{
				GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer",
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			s := NewServer(0, slog.Default())
			s.registerContributeRoutes()

			var reqBody *strings.Reader
			if tc.body != "" {
				reqBody = strings.NewReader(tc.body)
			} else {
				reqBody = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.target, reqBody)
			req.Header.Set("X-Hive-Role", "read")
			w := httptest.NewRecorder()
			tc.invoke(s, w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("read viewer got %d, want 403 (body: %s)", w.Code, w.Body.String())
			}
			// The read viewer must not have mutated anything.
			if p := findContributor("c-alice"); p == nil || p.TrustTier != "newcomer" {
				t.Errorf("read viewer mutated the profile: %+v", p)
			}
		})
	}
}

// TestContributorTrustIsOwnerOnly: contributor trust is OWNER-ONLY (audit F14).
//
// This test previously asserted that read-write ALSO passed the gate — it encoded
// the vulnerability. #3460 gated these handlers with requireOwnerRole; a v2->v4
// sync merge reverted that, and F14 (2026-08-13) re-reproduced it. Setting a
// contributor's trust tier is an owner action, so read-write must now be denied.
//
// The owner case also needs ownerRoleVerifiedHeader: requireOwnerRole demands
// BOTH the role header and the verified marker, so that a caller who can spoof
// X-Hive-Role alone cannot mint owner authority.
func TestContributorTrustIsOwnerOnly(t *testing.T) {
	for _, role := range []string{"owner", "read-write"} {
		t.Run("trust_"+role, func(t *testing.T) {
			setupContributeEnv(t)
			if err := saveContributorProfile(&ContributorProfile{
				GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer",
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			s := NewServer(0, slog.Default())
			s.registerContributeRoutes()

			req := httptest.NewRequest(http.MethodPut, "/api/contributors/alice/trust", strings.NewReader(`{"tier":"trusted"}`))
			req.SetPathValue("id", "alice")
			if role == "owner" {
				markOwnerRequest(req)
			} else if role != "" {
				req.Header.Set("X-Hive-Role", role)
			}
			w := httptest.NewRecorder()
			s.handleContributorTrust(w, req)

			wantCode := http.StatusOK
			if role != "owner" {
				wantCode = http.StatusForbidden
			}
			if w.Code != wantCode {
				t.Fatalf("role %q got %d, want %d (body: %s)", role, w.Code, wantCode, w.Body.String())
			}
			if role != "owner" {
				return // denied as intended; nothing further to assert
			}
			if p := findContributor("alice"); p == nil || p.TrustTier != "trusted" {
				t.Errorf("role %q: tier not promoted: %+v", role, p)
			}
		})
	}
}

// TestContributorWrite_MissingRoleForbidden pins the C5 fail-closed fix: a request
// with NO X-Hive-Role header (exactly what an unauthenticated caller reaching the
// pod directly presents) must be REJECTED with 403 on every contributor mutation
// endpoint, and must not mutate anything. Before the fix an absent header defaulted
// to "owner", so anonymous callers could promote/revoke/delete/requeue contributors.
func TestContributorWrite_MissingRoleForbidden(t *testing.T) {
	for _, tc := range contributorWriteCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupContributeEnv(t)
			if err := saveContributorProfile(&ContributorProfile{
				GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer",
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			s := NewServer(0, slog.Default())
			s.registerContributeRoutes()

			var reqBody *strings.Reader
			if tc.body != "" {
				reqBody = strings.NewReader(tc.body)
			} else {
				reqBody = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.target, reqBody)
			// Deliberately set NO X-Hive-Role header.
			w := httptest.NewRecorder()
			tc.invoke(s, w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("missing-role request got %d, want 403 (body: %s)", w.Code, w.Body.String())
			}
			if p := findContributor("c-alice"); p == nil || p.TrustTier != "newcomer" {
				t.Errorf("missing-role request mutated the profile: %+v", p)
			}
		})
	}
}

// TestContributorAdminRoutes_AnonymousDenied_ContributeFlowReachable is the
// end-to-end C5 regression, driven through the full Handler() middleware chain on a
// spoke that HAS an auth boundary (a shared authToken, i.e. a hosted/direct-route
// hive — not local/dev). It proves:
//
//  1. The /api/contributors/{id}/... admin mutation routes are NOT public: an
//     anonymous caller is stopped by authenticate() with 401. Before the fix the
//     bare HasPrefix("/api/contribute") in isPublicPath also matched
//     "/api/contributors/...", exempting these routes from auth entirely.
//  2. The real contribute-flow public routes stay reachable (NOT 401): the WS
//     upgrade path and POST /api/contribute/register still transit auth as public.
func TestContributorAdminRoutes_AnonymousDenied_ContributeFlowReachable(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.authToken = "shared-secret-token" // gives the spoke a real auth boundary
	s.registerContributeRoutes()
	h := s.Handler()

	// 1. Anonymous (no auth, no role) mutation attempts must be blocked by auth.
	adminMutations := []struct {
		name, method, path, body string
	}{
		{"trust", http.MethodPut, "/api/contributors/alice/trust", `{"tier":"trusted"}`},
		{"revoke", http.MethodPost, "/api/contributors/alice/revoke", ``},
		{"requeue", http.MethodPost, "/api/contributors/alice/requeue", ``},
		{"delete", http.MethodDelete, "/api/contributors/alice", ``},
	}
	for _, m := range adminMutations {
		t.Run("anon_"+m.name, func(t *testing.T) {
			req := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			// Must be denied — 401 (blocked at authenticate) or 403 (in-handler gate).
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Fatalf("anonymous %s %s got %d, want 401/403 (body: %s)", m.method, m.path, w.Code, w.Body.String())
			}
		})
	}
	// The anonymous attempts must not have mutated the profile.
	if p := findContributor("c-alice"); p == nil || p.TrustTier != "newcomer" {
		t.Errorf("anonymous admin calls mutated the profile: %+v", p)
	}

	// 2. The genuine public contribute-flow routes must NOT be blocked by auth.
	//    A 401 here would mean the boundary fix over-narrowed and broke onboarding.
	regReq := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{}`))
	regW := httptest.NewRecorder()
	h.ServeHTTP(regW, regReq)
	if regW.Code == http.StatusUnauthorized {
		t.Errorf("public POST /api/contribute/register got 401 — the boundary fix broke onboarding")
	}

	wsReq := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	wsW := httptest.NewRecorder()
	h.ServeHTTP(wsW, wsReq)
	if wsW.Code == http.StatusUnauthorized {
		t.Errorf("public GET /api/contribute/ws got 401 — the WS upgrade path must stay public")
	}
}

func TestContributorAgentRoleGrantsAuthzAndRoundTrip(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	deps.Config.Agents["ci-maintainer"] = config.AgentConfig{Backend: "claude", Model: "sonnet", Enabled: true}
	deps.Config.Agents["sec-check"] = config.AgentConfig{Backend: "claude", Model: "sonnet", Enabled: true}
	deps.Config.Hub.ContributeDelegatableRoles = []string{"scanner", "ci-maintainer", "sec-check", "supervisor"}

	readReq := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":["ci-maintainer"]}`))
	readReq.Header.Set("Content-Type", "application/json")
	readReq.Header.Set("X-Hive-Role", "read")
	readRec := httptest.NewRecorder()
	s.mux.ServeHTTP(readRec, readReq)
	if readRec.Code != http.StatusForbidden {
		t.Fatalf("read viewer got %d, want 403 (body: %s)", readRec.Code, readRec.Body.String())
	}
	if p := findContributor("c-alice"); p == nil || len(p.AgentRoleGrants) != 0 {
		t.Fatalf("read viewer mutated grants: %+v", p)
	}

	ownerReq := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":["sec-check","ci-maintainer","ci-maintainer"]}`))
	ownerReq.Header.Set("Content-Type", "application/json")
	ownerReq.Header.Set("X-Hive-Role", "owner")
	ownerReq.Header.Set(ownerRoleVerifiedHeader, "true")
	ownerRec := httptest.NewRecorder()
	s.mux.ServeHTTP(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner got %d, want 200 (body: %s)", ownerRec.Code, ownerRec.Body.String())
	}
	p := findContributor("c-alice")
	if p == nil || strings.Join(p.AgentRoleGrants, ",") != "ci-maintainer,sec-check" {
		t.Fatalf("grant round-trip mismatch: %+v", p)
	}
	if body := ownerRec.Body.String(); !strings.Contains(body, `"agent_role_grants":["ci-maintainer","sec-check"]`) {
		t.Fatalf("response did not echo normalized grants: %s", body)
	}

	badReq := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":["scanner"]}`))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("X-Hive-Role", "owner")
	badReq.Header.Set(ownerRoleVerifiedHeader, "true")
	badRec := httptest.NewRecorder()
	s.mux.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("default non-privileged role got %d, want 400", badRec.Code)
	}
}

// TestGovernorHubAcceptsSkipAssigned pins the bidirectional-parity addition: the
// Governor Hub config write path (the canonical editor, and the one the ops-tab
// toggle drives) now persists contribute_skip_assigned_to_others, and GET /api/config
// reflects it — so both surfaces edit the same underlying Config.Hub.* state.
func TestGovernorHubAcceptsSkipAssigned(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeSkipAssignedToOthers = false

	req := httptest.NewRequest(http.MethodPut, "/api/governor/hub",
		strings.NewReader(`{"contribute_skip_assigned_to_others":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorHub(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("governor hub update got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !deps.Config.Hub.ContributeSkipAssignedToOthers {
		t.Error("contribute_skip_assigned_to_others not persisted by handleGovernorHub")
	}

	// GET /api/config/governor must expose it so the mirror UI can reflect current
	// state (this is the endpoint that carries hub.contribute_* — the same source
	// the Governor Hub dialog reads).
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config/governor got %d", rec.Code)
	}
	var cfg struct {
		Hub map[string]any `json:"hub"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("config json: %v", err)
	}
	if got, ok := cfg.Hub["contribute_skip_assigned_to_others"]; !ok || got != true {
		t.Errorf("config hub missing/false contribute_skip_assigned_to_others: %v (ok=%v)", got, ok)
	}
}

// TestGovernorHubSave_ReadViewerForbidden proves the Management-tab filter-save
// boundary: a "read" viewer's PUT /api/config/governor/hub is 403'd by the
// roleEnforcement middleware, independent of the UI hiding. This is the server
// gate behind the Management admin block after the tab split — the split must not
// weaken it. Exercised through the full handler chain (Handler()) so the
// middleware actually runs.
func TestGovernorHubSave_ReadViewerForbidden(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeSuspended = false

	h := s.Handler()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub",
		strings.NewReader(`{"contribute_suspended":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("read viewer PUT /api/config/governor/hub got %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if deps.Config.Hub.ContributeSuspended {
		t.Error("read viewer mutated Config.Hub.ContributeSuspended through the filter-save endpoint")
	}
}

// TestLeaderboardTabPublic_ControlsStayGated proves the invariant for the new
// (4th) Leaderboard tab: the whole /contribute page — including the Leaderboard
// tab and its /api/leaderboard data — is PUBLIC (isPublicPath exempts them), so
// an unauthenticated/read viewer sees the leaderboard freely; but the Management
// controls and the Operations per-clanker admin buttons stay gated behind the
// /api/role check (client-side) and 403 on the mutation endpoints (server-side).
// Adding the read-only Leaderboard tab must not weaken that boundary.
func TestLeaderboardTabPublic_ControlsStayGated(t *testing.T) {
	setupContributeEnv(t)
	seedContributor(t, "alice", 7, 1)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	h := s.Handler()

	// 1. /contribute is reachable with NO auth (public) and carries the
	//    Leaderboard tab inline. The page renders through the full Handler(),
	//    which runs the isPublicPath exemption — a 200 here proves it is public.
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous GET /contribute got %d, want 200 (page must be public)", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-panel="tab-leaderboard"`, `id="tab-leaderboard"`,
		`id="leaderboard-list"`, `function loadLeaderboard`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("public /contribute page missing Leaderboard tab markup %q", want)
		}
	}

	// The admin controls block is still present in markup but gated CLIENT-side:
	// it only ever enables for owner/read-write via /api/role. The Leaderboard tab
	// must NOT be wired into that gate (it hydrates with no role check).
	if !strings.Contains(body, `id="ops-admin"`) {
		t.Error("admin controls markup missing (must remain, gated by /api/role)")
	}
	if !strings.Contains(body, `role!=='owner'&&role!=='read-write'`) {
		t.Error("admin controls role gate (owner/read-write only) must remain intact")
	}
	// The Leaderboard hydration must be independent of the admin/role gate.
	if strings.Contains(body, `dp==='tab-leaderboard'`) &&
		!strings.Contains(body, `dp==='tab-leaderboard'&&!lbStarted`) {
		t.Error("Leaderboard tab must hydrate without an admin/role gate")
	}

	// 2. /api/leaderboard (the tab's data source) is public — reachable with no auth.
	apiReq := httptest.NewRequest(http.MethodGet, "/api/leaderboard", nil)
	apiW := httptest.NewRecorder()
	h.ServeHTTP(apiW, apiReq)
	if apiW.Code != http.StatusOK {
		t.Fatalf("anonymous GET /api/leaderboard got %d, want 200 (data must be public)", apiW.Code)
	}
	if !strings.Contains(apiW.Body.String(), "alice") {
		t.Errorf("public /api/leaderboard should surface the seeded contributor; body: %s", apiW.Body.String())
	}

	// 3. The control mutation endpoints must STILL 403 a read viewer — the
	//    boundary the UI hiding merely shadows, unchanged by this feature.
	for _, tc := range contributorWriteCases() {
		mReq := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		mReq.Header.Set("X-Hive-Role", "read")
		mW := httptest.NewRecorder()
		tc.invoke(s, mW, mReq)
		if mW.Code != http.StatusForbidden {
			t.Errorf("read viewer %s %s got %d, want 403 (control must stay gated)", tc.method, tc.target, mW.Code)
		}
	}
	// And the Governor Hub filter-save endpoint (the Management admin controls'
	// write path) must 403 a read viewer via roleEnforcement.
	ghReq := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub",
		strings.NewReader(`{"contribute_suspended":true}`))
	ghReq.Header.Set("Content-Type", "application/json")
	ghReq.Header.Set("X-Hive-Role", "read")
	ghW := httptest.NewRecorder()
	h.ServeHTTP(ghW, ghReq)
	if ghW.Code != http.StatusForbidden {
		t.Errorf("read viewer PUT /api/config/governor/hub got %d, want 403", ghW.Code)
	}
}
