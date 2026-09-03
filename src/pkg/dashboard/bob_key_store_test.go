package dashboard

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// bobTestKey is a realistic-looking secret used across these tests. Every
// assertion that scans output for a leak looks for this exact value.
const bobTestKey = "bob-sk-supersecretvalue-abcdef123456"

// pointBobKeyAtTempDir redirects the PVC key path at a temp dir for the
// duration of a test and returns the path.
func pointBobKeyAtTempDir(t *testing.T) string {
	t.Helper()
	orig := writableBobKeyFile
	p := filepath.Join(t.TempDir(), "secrets", "bob_api_key")
	writableBobKeyFile = p
	t.Cleanup(func() { writableBobKeyFile = orig })
	return p
}

// newBobTestServer builds a Server with a live config whose SourcePath is
// empty, so saveConfig is a no-op and the handlers exercise everything else.
func newBobTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	s.deps = &Dependencies{Config: &config.Config{}}
	return s
}

// writeBobKeyFile creates the secrets dir and writes the key 0600 — the
// normal PVC path — and the resolver that #2202 established then finds it.
func TestWriteBobKeyFile_CreatesDirAndFileAndResolverFindsIt(t *testing.T) {
	path := pointBobKeyAtTempDir(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatalf("writeBobKeyFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key file: %v", err)
	}
	if string(got) != bobTestKey {
		t.Errorf("key file content mismatch")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != bobKeyFileMode {
		t.Errorf("key file mode = %v, want %v", info.Mode().Perm(), os.FileMode(bobKeyFileMode))
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != bobKeyDirMode {
		t.Errorf("secrets dir mode = %v, want %v", dirInfo.Mode().Perm(), os.FileMode(bobKeyDirMode))
	}

	// The whole point: the resolver #2202 added must pick this file up when
	// api_key_file points at it, with no restart and no new mechanism.
	bc := &config.BobConfig{APIKeyFile: path}
	if resolved := bc.ResolveAPIKey(); resolved != bobTestKey {
		t.Errorf("ResolveAPIKey did not return the stored key")
	}
	if src := bc.ResolveAPIKeySource(); src != "file:"+path {
		t.Errorf("ResolveAPIKeySource = %q, want %q", src, "file:"+path)
	}
}

// Overwriting an existing key (rotation) replaces the value and keeps 0600,
// and the atomic rename leaves no stray temp files behind.
func TestWriteBobKeyFile_RotationOverwritesAndLeavesNoTempFiles(t *testing.T) {
	path := pointBobKeyAtTempDir(t)

	if err := writeBobKeyFile("bob-sk-oldkey-000000"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bobTestKey {
		t.Errorf("rotated key not persisted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != bobKeyFileMode {
		t.Errorf("mode after rotation = %v, want %v", info.Mode().Perm(), os.FileMode(bobKeyFileMode))
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("leftover file in secrets dir: %s", e.Name())
		}
	}
}

// An unwritable secrets dir yields an actionable error that never contains
// the key value.
func TestWriteBobKeyFile_UnwritableDirYieldsActionableError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission model")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	base := t.TempDir()
	secretsDir := filepath.Join(base, "secrets")
	if err := os.Mkdir(secretsDir, 0o500); err != nil { // r-x, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secretsDir, 0o700) })

	orig := writableBobKeyFile
	writableBobKeyFile = filepath.Join(secretsDir, "bob_api_key")
	t.Cleanup(func() { writableBobKeyFile = orig })

	err := writeBobKeyFile(bobTestKey)
	if err == nil {
		t.Fatal("expected an error writing into an unwritable dir")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error should wrap os.ErrPermission, got: %v", err)
	}
	if strings.Contains(err.Error(), bobTestKey) {
		t.Fatal("error message leaks the key value")
	}
}

// Clearing removes the file and is idempotent, so a retry converges.
func TestClearBobKeyFile(t *testing.T) {
	path := pointBobKeyAtTempDir(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatalf("writeBobKeyFile: %v", err)
	}
	removed, err := clearBobKeyFile()
	if err != nil {
		t.Fatalf("clearBobKeyFile: %v", err)
	}
	if !removed {
		t.Error("expected removed=true for an existing key file")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("key file still present after clear")
	}
	// The resolver must now report nothing configured.
	bc := &config.BobConfig{APIKeyFile: path}
	if bc.ResolveAPIKey() != "" {
		t.Error("resolver still returns a key after clear")
	}

	// Idempotent: clearing again succeeds and reports nothing removed.
	removed, err = clearBobKeyFile()
	if err != nil {
		t.Fatalf("second clearBobKeyFile: %v", err)
	}
	if removed {
		t.Error("expected removed=false on the second clear")
	}
}

// The PUT handler stores the key, points api_key_file at it, and never echoes
// the value in the response body.
func TestHandleGovernorBobKey(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantStored bool
	}{
		{name: "valid key", body: `{"apiKey":"` + bobTestKey + `"}`, wantStatus: http.StatusOK, wantStored: true},
		{name: "empty key rejected", body: `{"apiKey":"   "}`, wantStatus: http.StatusBadRequest},
		{name: "missing field rejected", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "oversized key rejected", body: `{"apiKey":"` + strings.Repeat("x", bobKeyMaxLen+1) + `"}`, wantStatus: http.StatusBadRequest},
		{name: "malformed body rejected", body: `not json`, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := pointBobKeyAtTempDir(t)
			s := newBobTestServer(t)

			req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			markOwnerRequest(req)
			s.handleGovernorBobKey(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// No response body, on success OR failure, may contain the key.
			if strings.Contains(rec.Body.String(), bobTestKey) {
				t.Error("response body leaks the key value")
			}

			if !tc.wantStored {
				if _, err := os.Stat(path); err == nil {
					t.Error("key file written for a rejected request")
				}
				return
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("key file not written: %v", err)
			}
			if string(data) != bobTestKey {
				t.Error("stored key does not match the submitted value")
			}
			// hive.yaml records the PATH only — never the value.
			if got := s.deps.Config.Governor.Bob.APIKeyFile; got != path {
				t.Errorf("api_key_file = %q, want %q", got, path)
			}
			if s.deps.Config.Governor.Bob.APIKeyEnv == bobTestKey {
				t.Error("key value leaked into api_key_env")
			}
		})
	}
}

// The DELETE handler removes the stored key and reports it as unconfigured.
func TestHandleGovernorBobKeyClear(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.Bob.APIKeyFile = path

	req := httptest.NewRequest(http.MethodDelete, "/api/config/governor/bob", nil)
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKeyClear(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["configured"] != false {
		t.Errorf("configured = %v, want false", resp["configured"])
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("key file still present after clear")
	}
	// The dangling pointer must be dropped so the hive returns to the
	// documented "key required" state, not a half-configured one.
	if got := s.deps.Config.Governor.Bob.APIKeyFile; got != "" {
		t.Errorf("api_key_file = %q, want empty after clear", got)
	}
}

// The status endpoint reports presence and source but never the value.
func TestHandleGovernorBobStatus_NeverReturnsKeyValue(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	// Unconfigured.
	rec := httptest.NewRecorder()
	s.handleGovernorBobStatus(rec, httptest.NewRequest(http.MethodGet, "/api/config/governor/bob", nil))
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["configured"] != false {
		t.Errorf("configured = %v, want false before a key is set", resp["configured"])
	}

	// Configured.
	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Governor.Bob.APIKeyFile = path

	rec = httptest.NewRecorder()
	s.handleGovernorBobStatus(rec, httptest.NewRequest(http.MethodGet, "/api/config/governor/bob", nil))
	body := rec.Body.String()
	if strings.Contains(body, bobTestKey) {
		t.Fatal("status response leaks the key value")
	}
	resp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["configured"] != true {
		t.Errorf("configured = %v, want true", resp["configured"])
	}
	if resp["source"] != "file:"+path {
		t.Errorf("source = %v, want %q", resp["source"], "file:"+path)
	}
}

// A read-only role must not be able to set or clear the key. Authorization is
// the shared roleEnforcement middleware (any non-GET is refused for role
// "read"), so this asserts the wiring rather than a bespoke rule.
func TestBobKeyEndpoints_ReadOnlyRoleForbidden(t *testing.T) {
	path := pointBobKeyAtTempDir(t)
	s := newBobTestServer(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config/governor/bob", s.handleGovernorBobStatus)
	mux.HandleFunc("PUT /api/config/governor/bob", s.handleGovernorBobKey)
	mux.HandleFunc("DELETE /api/config/governor/bob", s.handleGovernorBobKeyClear)
	handler := s.roleEnforcement(mux)

	tests := []struct {
		name       string
		method     string
		role       string
		body       string
		wantStatus int
	}{
		{name: "read role cannot set", method: http.MethodPut, role: "read",
			body: `{"apiKey":"` + bobTestKey + `"}`, wantStatus: http.StatusForbidden},
		{name: "read role cannot clear", method: http.MethodDelete, role: "read",
			wantStatus: http.StatusForbidden},
		{name: "read role may view presence", method: http.MethodGet, role: "read",
			wantStatus: http.StatusOK},
		{name: "owner role may set", method: http.MethodPut, role: "owner",
			body: `{"apiKey":"` + bobTestKey + `"}`, wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest starts from no stored key so the "forbidden" cases
			// can prove nothing was written.
			_, _ = clearBobKeyFile()

			var bodyReader = strings.NewReader(tc.body)
			req := httptest.NewRequest(tc.method, "/api/config/governor/bob", bodyReader)
			req.Header.Set("X-Hive-Role", tc.role)
			if tc.role == "owner" {
				req.Header.Set(ownerRoleVerifiedHeader, "true")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), bobTestKey) {
				t.Error("response body leaks the key value")
			}
			if tc.wantStatus == http.StatusForbidden {
				if _, err := os.Stat(path); err == nil {
					t.Error("a read-only role managed to write the key file")
				}
			}
		})
	}
}

// --- UI location -----------------------------------------------------------
//
// The bob key entry UI moved from the per-agent AUTH field (#2218) to a
// hive-wide governor tab: governor.bob.api_key_file is a single hive-scoped
// setting resolving to one file, so a per-agent control implied a scope that
// does not exist. The markup is JS inside index.html and invisible to the Go
// compiler, so these tests are the only automated guard that the control stays
// in exactly one place.

// TestBobKeyUILivesOnGovernorTab asserts the governor Bob tab exists, is wired
// into the tab strip and the render dispatch, and drives the set/clear flow.
func TestBobKeyUILivesOnGovernorTab(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"tab name constant", "const GOVERNOR_BOB_TAB = 'Bob';"},
		{"tab is in the governor strip", "'Model Gateways', GOVERNOR_BOB_TAB, 'Variables', 'Security'"},
		{"tab is wired into render dispatch", "case GOVERNOR_BOB_TAB: return renderGovBob();"},
		{"tab renderer exists", "function renderGovBob() {"},
		{"status loader exists", "async function loadBobKeyStatus() {"},
		{"panel renderer exists", "function renderBobKeyPanel(host, configured, source, keyName) {"},
		{"dialog is opened from the panel", "openBobKeyDialog(configured, source, keyName)"},
		{"clear is reachable from the panel", "clearBtn.addEventListener('click', clearBobKey)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("index.html is missing %q — the governor Bob tab is not wired up", tc.snippet)
			}
		})
	}
}

// TestBobKeyUIRemovedFromAgentCard asserts the per-agent AUTH entry point from
// #2218 is gone, so there is exactly one place the key can be set. The AUTH
// field must fall through to the generic backend button for a bob agent.
func TestBobKeyUIRemovedFromAgentCard(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"no per-agent dispatch case", "case 'setBobKey':"},
		{"no per-agent data-action", `data-action="setBobKey"`},
		{"no per-agent isBob branch", "const isBob = a.cli === 'bob';"},
		{"no dead bob button styling", ".cli-login-btn.bob"},
		{"dialog takes no agent argument", "openBobKeyDialog(agentName)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(html, tc.snippet) {
				t.Errorf("index.html still contains %q — the per-agent bob entry point was not fully removed", tc.snippet)
			}
		})
	}
}

// TestBobKeyUIRestartCopyStaysAccurate pins the user-facing claim about
// restarts. The resolver re-reads the key file at every agent launch, so no pod
// restart is needed — and since handleGovernorBobKey now calls
// RelaunchBobAgentsAwaitingKey, an agent parked for a missing key IS relaunched
// by the save. Copy that drops either half sends users to the wrong remedy.
func TestBobKeyUIRestartCopyStaysAccurate(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"panel says no restart needed", "restart is needed — and any bob agent already parked for a missing key is"},
		{"panel says restart is automatic", "restarted automatically as part of the save"},
		// The fresh-session half is the actual fix: a relaunch into the stale
		// pane shell would not pick the key up (secrets reach the CLI only via
		// tmux set-environment, inherited by shells created afterwards).
		{"panel says the session is fresh", "in a fresh session that picks"},
		{"save toast reports the relaunched count", "Number(data.relaunched)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("index.html is missing %q — the restart copy is no longer accurate", tc.snippet)
			}
		})
	}
}

// TestBobKeyUINoLongerTellsUserToStartAgents guards the regression this change
// closes: the old copy told operators to go start paused bob agents by hand.
// Now that the save relaunches them, any resurfacing of that instruction (e.g.
// via a backport from mk/dd/v3, where this fix has not landed yet) is a lie.
func TestBobKeyUINoLongerTellsUserToStartAgents(t *testing.T) {
	html := indexHTML(t)
	stale := []string{
		"must be started once",
		"not\n            relaunched automatically",
		"start any bob agent that is paused for a missing key",
	}
	for _, s := range stale {
		if strings.Contains(html, s) {
			t.Errorf("index.html still contains stale copy %q — the key save now relaunches parked bob agents", s)
		}
	}
}

// TestBobKeyUINeverRendersTheKeyValue asserts the dashboard never puts a key
// value into markup. The panel is built from `configured` (bool) and the safe
// `source` string only; the input that carries the value is a password field
// that is cleared before the dialog node is removed.
func TestBobKeyUINeverRendersTheKeyValue(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, `<input id="bob-key-input" type="password"`) {
		t.Error("the bob key input must be type=password so the value is never shown")
	}
	if !strings.Contains(html, "// Drop the secret from the DOM before removing the node.") {
		t.Error("the dialog must clear the input value before removing the node")
	}
	// The panel must render presence + source + the safe key NAME only, never a
	// value field. keyName is an operator-chosen label, not the secret.
	if !strings.Contains(html, "renderBobKeyPanel(host, d.configured === true, d.source || '', d.keyName || '')") {
		t.Error("the panel must be fed only `configured`, the safe `source` string, and the safe `keyName` label")
	}
}

// TestIndexHTMLHasExactlyOneBodyTag guards the snapshot builder, which locates
// the document body with a plain indexOf("<body>"). A second occurrence — even
// inside a comment or a JS string — silently corrupts the build, so any edit to
// index.html has to keep this at exactly one.
func TestIndexHTMLHasExactlyOneBodyTag(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "<body>"); got != 1 {
		t.Errorf("index.html contains %d occurrences of the opening body tag, want exactly 1", got)
	}
}
