package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The admin Users section's collapse behaviour and the contact editor's close
// affordances live inside the dashboardHTML raw string, so there is no Go
// function to exercise directly. These tests guard the wiring: every handler
// the markup names must exist, and the invariants that are cheap to break in a
// refactor (an in-memory-only expand, a close that discards typed text, a save
// error that never reaches the admin) must stay asserted somewhere CI runs.

// TestDashboardAdminUsersCollapseSymbols asserts the collapse markup and its
// handlers agree. A typo here is invisible to the Go compiler and shows up only
// as a header that does not respond to clicks.
func TestDashboardAdminUsersCollapseSymbols(t *testing.T) {
	html := dashScript(t)

	for _, want := range []string{
		// Markup: the header is the toggle, and it is reachable by keyboard.
		`id="admin-users-header"`,
		`onclick="toggleAdminUsers()"`,
		`aria-controls="admin-users-body"`,
		`id="admin-users-body"`,
		`id="admin-users-toggle"`,
		`id="admin-users-count"`,
		// Handlers.
		"function toggleAdminUsers()",
		"function applyAdminUsersCollapsed()",
		"function persistAdminUsersCollapsed()",
		"function expandAdminUsersSection()",
		"function updateAdminUsersCount(",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard is missing admin-users collapse wiring: %s", want)
		}
	}

	// Enter/Space must activate the header: it is a <h2 role="button">, which
	// browsers do not give native keyboard activation.
	if !strings.Contains(html, "toggleAdminUsers();}") {
		t.Error("the admin Users header must be activatable from the keyboard")
	}
}

// TestDashboardAdminUsersCollapseDefaultsAndPersistence pins the two things a
// refactor most easily inverts: the default state, and the fact that the choice
// survives a reload.
func TestDashboardAdminUsersCollapseDefaultsAndPersistence(t *testing.T) {
	html := dashScript(t)

	// The key follows the hive-*-collapsed convention shared with the cluster
	// health and past requests panels.
	if !strings.Contains(html, `var ADMIN_USERS_COLLAPSED_KEY = 'hive-admin-users-collapsed';`) {
		t.Error("admin Users collapse must persist under the hive-*-collapsed key convention")
	}

	// Collapsed by default: only an explicit 'false' expands, so a first visit
	// and a corrupted value both land on collapsed. The roster runs to dozens of
	// records and would otherwise push every section below it off the fold.
	if !strings.Contains(html,
		`var _adminUsersCollapsed = localStorage.getItem(ADMIN_USERS_COLLAPSED_KEY) !== 'false';`) {
		t.Error("the admin Users section must default to collapsed (only 'false' expands)")
	}

	// The persisted state must actually be pushed onto the DOM once the admin
	// section becomes visible; without this the markup's static display:none
	// wins and the section can never appear expanded on reload.
	if !strings.Contains(html, "applyAdminUsersCollapsed();") {
		t.Error("the persisted collapse state must be applied after the admin check passes")
	}
}

// TestDashboardAdminUsersExpandPersists is the #2348 lesson applied to this
// section: anything that expands the section to reveal a row must ALSO write
// the new state, or the row is hidden again on the next reload.
func TestDashboardAdminUsersExpandPersists(t *testing.T) {
	html := dashScript(t)

	idx := strings.Index(html, "function expandAdminUsersSection()")
	if idx < 0 {
		t.Fatal("expandAdminUsersSection is missing")
	}
	// Bound the search to the function body so a persist call elsewhere in the
	// file cannot satisfy this assertion.
	end := strings.Index(html[idx:], "\n    }")
	if end < 0 {
		t.Fatal("could not delimit expandAdminUsersSection")
	}
	body := html[idx : idx+end]

	if !strings.Contains(body, "persistAdminUsersCollapsed()") {
		t.Error("expandAdminUsersSection must persist the expansion; an in-memory-only " +
			"expand silently re-collapses on reload and re-hides the target row (#2348)")
	}
	if !strings.Contains(body, "applyAdminUsersCollapsed()") {
		t.Error("expandAdminUsersSection must push the new state onto the DOM")
	}
}

// TestDashboardContactEditorCloseAffordances asserts the editor can actually be
// dismissed. Before this it had no close control at all: opening one left it
// open for the rest of the session.
func TestDashboardContactEditorCloseAffordances(t *testing.T) {
	html := dashScript(t)

	for _, want := range []string{
		"function closeContactPanel(",
		"function contactPanelHasUnsavedEdits(",
		"function commitContactPanelEdits(",
		"function openContactPanelUsername()",
		// Both the × and the Close button carry the same hook, so one binding
		// serves both.
		"data-contact-close=",
		// Bound as a listener, not an inline on* attribute: esc() leaves
		// apostrophes alone, so a username in an inline handler would be an
		// injection vector.
		"[data-contact-close]",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the contact editor is missing a close affordance: %s", want)
		}
	}

	// Escape must be handled in the CAPTURE phase so it can decide before the
	// global Escape handler, and it must stop propagation only when it acted —
	// otherwise it would swallow Escape for create-modal (#2321).
	if !strings.Contains(html, "closeContactPanel(username);\n    }, true);") {
		t.Error("Escape-to-close must be registered in the capture phase")
	}
	escIdx := strings.Index(html, "function openContactPanelUsername()")
	if escIdx < 0 {
		t.Fatal("openContactPanelUsername is missing")
	}
	// A stray Escape with focus outside the editor must fall through to the
	// global handler rather than being consumed here.
	if !strings.Contains(html, "if (!username) return;\n      e.stopPropagation();") {
		t.Error("Escape must fall through to the global handler when no contact editor has focus")
	}
	// While a confirm overlay is up it owns Escape; stealing the key there would
	// leave the overlay stranded.
	if !strings.Contains(html, "if (document.querySelector('.hive-confirm-overlay')) return;") {
		t.Error("Escape must not be stolen from an open hiveConfirm overlay")
	}
}

// TestDashboardContactEditorWarnsBeforeDiscarding asserts closing with typed but
// unsaved text prompts rather than dropping it. The fields save on blur, so
// unblurred text exists ONLY in the DOM node the close is about to hide.
func TestDashboardContactEditorWarnsBeforeDiscarding(t *testing.T) {
	html := dashScript(t)

	idx := strings.Index(html, "async function closeContactPanel(")
	if idx < 0 {
		t.Fatal("closeContactPanel is missing")
	}
	end := strings.Index(html[idx:], "\n    }")
	if end < 0 {
		t.Fatal("could not delimit closeContactPanel")
	}
	body := html[idx : idx+end]

	if !strings.Contains(body, "contactPanelHasUnsavedEdits(username)") {
		t.Error("closeContactPanel must check for unsaved edits before closing")
	}
	if !strings.Contains(body, "hiveConfirm(") {
		t.Error("closing with unsaved edits must warn rather than silently discard")
	}
	// A rejected save must leave the editor open with the text intact, so the
	// admin can read the reason and retry.
	if !strings.Contains(body, "if (!saved) return;") {
		t.Error("a failed save must keep the contact editor open")
	}
}

// TestDashboardUpdateUserSurfacesErrors guards the #2348 failure mode: a save
// whose response is never checked looks identical to a successful one.
func TestDashboardUpdateUserSurfacesErrors(t *testing.T) {
	html := dashScript(t)

	idx := strings.Index(html, "async function updateUser(")
	if idx < 0 {
		t.Fatal("updateUser is missing")
	}
	end := strings.Index(html[idx:], "\n    }")
	if end < 0 {
		t.Fatal("could not delimit updateUser")
	}
	body := html[idx : idx+end]

	if !strings.Contains(body, "if (!resp.ok)") {
		t.Error("updateUser must check the response status; an ignored failure " +
			"is indistinguishable from a successful save")
	}
	if !strings.Contains(body, "readErrorMessage(resp") {
		t.Error("updateUser must surface the server's reason, not a generic failure")
	}

	// readErrorMessage must read text() and parse defensively. resp.json() on a
	// non-JSON body throws, which is exactly how #2348 turned a real cause into
	// a generic "failed".
	rIdx := strings.Index(html, "async function readErrorMessage(")
	if rIdx < 0 {
		t.Fatal("readErrorMessage is missing")
	}
	rEnd := strings.Index(html[rIdx:], "\n    }")
	rBody := html[rIdx : rIdx+rEnd]
	if !strings.Contains(rBody, "resp.text()") {
		t.Error("readErrorMessage must use resp.text() so a non-JSON error body does not throw")
	}
	if !strings.Contains(rBody, "JSON.parse(") {
		t.Error("readErrorMessage must parse the JSON body when there is one")
	}
}

// TestAdminUpdateUserErrorsAreJSON is the server half of the same defect.
// http.Error forces Content-Type: text/plain even when the body is JSON, so a
// browser calling resp.json() on it throws and the real cause is lost.
func TestAdminUpdateUserErrorsAreJSON(t *testing.T) {
	dir := t.TempDir()
	old := saasUsersDir
	saasUsersDir = filepath.Join(dir, "users")
	t.Cleanup(func() { saasUsersDir = old })
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: slog.Default()}

	t.Run("unknown user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/users/nobody", strings.NewReader(`{}`))
		req.SetPathValue("username", "nobody")
		rec := httptest.NewRecorder()
		s.handleAdminUpdateUser(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertJSONError(t, rec, "user not found")
	})

	t.Run("malformed body", func(t *testing.T) {
		u := &SaaSUser{GitHubUsername: "someone"}
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/users/someone", strings.NewReader(`{not json`))
		req.SetPathValue("username", "someone")
		rec := httptest.NewRecorder()
		s.handleAdminUpdateUser(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		assertJSONError(t, rec, "invalid JSON")
	})
}

// assertJSONError checks a response is a JSON error the browser can actually
// read: the declared content type must be JSON (http.Error's text/plain is the
// bug being guarded) and the body must decode with a non-empty "error".
func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, wantMsg string) {
	t.Helper()

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json — http.Error forces "+
			"text/plain, which makes resp.json() throw in the dashboard", ct)
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not valid JSON (%v): %s", err, rec.Body.String())
	}
	if body.Error != wantMsg {
		t.Errorf("error = %q, want %q", body.Error, wantMsg)
	}
}
