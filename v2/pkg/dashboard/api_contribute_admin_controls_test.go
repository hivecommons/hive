package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #2534 — Operator admin controls mirrored into the Management & Operations tab.
//
// These tests pin two things:
//   1. The UI: the /contribute page must ship the (initially hidden) admin-controls
//      markup that mirrors the Governor Hub config — suspend/skip toggles, the
//      admission-filter editors, and the per-contributor action wiring — plus the
//      /api/role gate and the themed confirm modal. Rendering is gated CLIENT-side
//      by initAdmin() reading /api/role; a read viewer never enables it.
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
		`data-role="revoke"`,
		`data-role="remove"`,
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
		{"revoke", (*Server).handleContributorRevoke, http.MethodPost, "/api/contributors/alice/revoke", ``},
		{"delete", (*Server).handleContributorDelete, http.MethodDelete, "/api/contributors/alice", ``},
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

// TestContributorWrite_OwnerAndRWAllowed proves owner and read-write pass the gate
// (they are not blocked), and that an absent header falls back to owner (local/dev).
func TestContributorWrite_OwnerAndRWAllowed(t *testing.T) {
	for _, role := range []string{"owner", "read-write", ""} {
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
			if role != "" {
				req.Header.Set("X-Hive-Role", role)
			}
			w := httptest.NewRecorder()
			s.handleContributorTrust(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("role %q got %d, want 200 (body: %s)", role, w.Code, w.Body.String())
			}
			if p := findContributor("alice"); p == nil || p.TrustTier != "trusted" {
				t.Errorf("role %q: tier not promoted: %+v", role, p)
			}
		})
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
