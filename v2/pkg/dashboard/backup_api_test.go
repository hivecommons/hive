package dashboard

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// backupTestKey is a test-only AES-256 key. Production has no default and
// reads HIVE_BACKUP_KEY.
func backupTestKey() string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return hex.EncodeToString(k)
}

func backupTestServer() *Server {
	return &Server{
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// setOwnerHeaders sets both the role and the proxy-verified marker on r.
// requireOwnerRole checks both; tests that simulate a legitimate owner request
// must set both so they exercise the real auth path rather than the 403 branch.
func setOwnerHeaders(r *http.Request) {
	r.Header.Set("X-Hive-Role", "owner")
	r.Header.Set(ownerRoleVerifiedHeader, "true")
}

// TestBackupDeniedForNonOwners is the authorization gate: viewers and
// read-write members must not be able to pull an archive containing this
// hive's GitHub App private keys.
func TestBackupDeniedForNonOwners(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", backupTestKey())
	s := backupTestServer()

	for _, role := range []string{"read", "read-write", "write", "viewer"} {
		t.Run(role, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				fn   func(http.ResponseWriter, *http.Request)
				verb string
				path string
			}{
				{"download", s.handleBackupDownload, http.MethodPost, "/api/backup"},
				{"status", s.handleBackupStatus, http.MethodGet, "/api/backup/status"},
			} {
				req := httptest.NewRequest(tc.verb, tc.path, nil)
				req.Header.Set("X-Hive-Role", role)
				rec := httptest.NewRecorder()
				tc.fn(rec, req)

				if rec.Code != http.StatusForbidden {
					t.Errorf("%s as %q: status = %d, want 403", tc.name, role, rec.Code)
				}
				if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
					t.Errorf("%s as %q leaked key material", tc.name, role)
				}
			}
		})
	}
}

// TestBackupDeniedWithoutVerifiedHeader checks that a client sending only
// X-Hive-Role: owner (without the proxy-set ownerRoleVerifiedHeader) is
// rejected. This is the header-spoofing attack path the fix addresses.
func TestBackupDeniedWithoutVerifiedHeader(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", backupTestKey())
	s := backupTestServer()

	for _, tc := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		verb string
	}{
		{"download", s.handleBackupDownload, http.MethodPost},
		{"status", s.handleBackupStatus, http.MethodGet},
	} {
		req := httptest.NewRequest(tc.verb, "/api/backup", nil)
		req.Header.Set("X-Hive-Role", "owner")
		// ownerRoleVerifiedHeader intentionally NOT set — simulates a direct
		// client bypassing the proxy.
		rec := httptest.NewRecorder()
		tc.fn(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without verified header: status = %d, want 403", tc.name, rec.Code)
		}
	}
}

// TestBackupRefusesWithoutEncryptionKey — with no HIVE_BACKUP_KEY the endpoint
// must refuse rather than stream unencrypted credentials to a browser.
func TestBackupRefusesWithoutEncryptionKey(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	s := backupTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/backup", nil)
	setOwnerHeaders(req)
	rec := httptest.NewRecorder()
	s.handleBackupDownload(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	if ct := rec.Header().Get("Content-Disposition"); ct != "" {
		t.Errorf("must not offer a download when refusing: %q", ct)
	}
}

// TestBackupStatusReportsUnavailableWithoutKey lets the UI explain itself
// before the owner clicks, and must never echo a key value.
func TestBackupStatusReportsUnavailableWithoutKey(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	s := backupTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
	setOwnerHeaders(req)
	rec := httptest.NewRecorder()
	s.handleBackupStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"available":false`) {
		t.Errorf("expected available:false, got %s", body)
	}
}

// TestBackupStatusAvailableWithKey — the happy path for the UI probe.
func TestBackupStatusAvailableWithKey(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", backupTestKey())
	s := backupTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
	setOwnerHeaders(req)
	rec := httptest.NewRecorder()
	s.handleBackupStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"available":true`) {
		t.Errorf("expected available:true, got %s", body)
	}
	if strings.Contains(body, backupTestKey()) {
		t.Fatal("status response echoed the backup key")
	}
}

// TestBackupResponseIsNotCacheable — a credential-bearing body must not be
// stored by the browser cache or an intermediary.
func TestBackupResponseIsNotCacheable(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", backupTestKey())
	t.Setenv("HIVE_SPOKE_BACKUP_DATA_DIR", t.TempDir())
	s := backupTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/backup", nil)
	setOwnerHeaders(req)
	rec := httptest.NewRecorder()
	s.handleBackupDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".tar.gz.enc") {
		t.Errorf("Content-Disposition = %q, want an .enc attachment", cd)
	}
	if enc := rec.Header().Get("X-Hive-Backup-Encrypted"); enc != "aes-256-gcm" {
		t.Errorf("X-Hive-Backup-Encrypted = %q", enc)
	}
}
