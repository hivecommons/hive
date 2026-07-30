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

	"github.com/kubestellar/hive/v2/pkg/config"
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
		{name: "admin role may set", method: http.MethodPut, role: "admin",
			body: `{"apiKey":"` + bobTestKey + `"}`, wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest starts from no stored key so the "forbidden" cases
			// can prove nothing was written.
			_, _ = clearBobKeyFile()

			var bodyReader *strings.Reader = strings.NewReader(tc.body)
			req := httptest.NewRequest(tc.method, "/api/config/governor/bob", bodyReader)
			req.Header.Set("X-Hive-Role", tc.role)
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
