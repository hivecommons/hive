package dashboard

// Tests for the governor-config backup encryption key (#4129). Hosted spoke
// owners have no deployment-env access, so the key must be settable through
// governor config — while the fail-closed rule (no key ⇒ no backup) and the
// no-leak rule (the value never appears in a response, config, or log-facing
// struct) both survive.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hubbackup"
)

// pointBackupKeyAtTempDir redirects the PVC key path at a temp dir for the
// duration of a test and returns the path.
func pointBackupKeyAtTempDir(t *testing.T) string {
	t.Helper()
	orig := writableBackupKeyFile
	p := filepath.Join(t.TempDir(), "secrets", "backup_encryption_key")
	writableBackupKeyFile = p
	t.Cleanup(func() { writableBackupKeyFile = orig })
	return p
}

// newBackupKeyTestServer builds a Server with a live config whose SourcePath
// is empty, so saveConfig is a no-op and the handlers exercise everything else.
func newBackupKeyTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	s.deps = &Dependencies{Config: &config.Config{}}
	return s
}

func putBackupKey(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleBackupKeySet(rec, req)
	return rec
}

// TestBackupKeySetThroughGovernorConfigEnablesBackup is the acceptance
// criterion: with NO HIVE_BACKUP_KEY on the deployment, an owner sets the key
// through governor config and the backup then succeeds.
func TestBackupKeySetThroughGovernorConfigEnablesBackup(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	t.Setenv("HIVE_SPOKE_BACKUP_DATA_DIR", t.TempDir())
	keyPath := pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	// Before: refused.
	req := httptest.NewRequest(http.MethodPost, "/api/backup", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleBackupDownload(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("pre-config backup status = %d, want 412", rec.Code)
	}

	// Configure the key the way a hosted owner does — through the API only.
	if rec := putBackupKey(t, s, `{"encryptionKey":"`+backupTestKey()+`","keyName":"escrowed"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT key status = %d, body %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.Backup.KeyFile != keyPath {
		t.Fatalf("key_file = %q, want %q", s.deps.Config.Governor.Backup.KeyFile, keyPath)
	}

	// After: the same request succeeds and the archive is encrypted.
	req = httptest.NewRequest(http.MethodPost, "/api/backup", nil)
	markOwnerRequest(req)
	rec = httptest.NewRecorder()
	s.handleBackupDownload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-config backup status = %d, body %s", rec.Code, rec.Body.String())
	}
	if enc := rec.Header().Get("X-Hive-Backup-Encrypted"); enc != "aes-256-gcm" {
		t.Errorf("X-Hive-Backup-Encrypted = %q, want aes-256-gcm", enc)
	}
	// The archive must actually open with the configured key — proof it was
	// sealed with the governor-config key and not left in the clear.
	if _, err := hubbackup.Open(mustDecodeBackupTestKey(t), rec.Body.Bytes()); err != nil {
		t.Fatalf("archive does not decrypt with the configured key: %v", err)
	}
}

// TestBackupKeyStatusNeverEchoesKey — presence-only status, for the UI.
func TestBackupKeyStatusNeverEchoesKey(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	get := func() map[string]interface{} {
		req := httptest.NewRequest(http.MethodGet, "/api/config/governor/backup", nil)
		markOwnerRequest(req)
		rec := httptest.NewRecorder()
		s.handleBackupKeyStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), backupTestKey()) {
			t.Fatal("status response echoed the backup key")
		}
		var d map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		return d
	}

	if d := get(); d["configured"] != false || d["usable"] != false {
		t.Errorf("unconfigured status = %v, want configured/usable false", d)
	}

	if rec := putBackupKey(t, s, `{"encryptionKey":"`+backupTestKey()+`","keyName":"escrowed"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT key status = %d, body %s", rec.Code, rec.Body.String())
	}
	d := get()
	if d["configured"] != true || d["usable"] != true {
		t.Errorf("configured status = %v, want configured/usable true", d)
	}
	if d["keyName"] != "escrowed" {
		t.Errorf("keyName = %v, want the operator label", d["keyName"])
	}
	if src, _ := d["source"].(string); !strings.HasPrefix(src, "file:") {
		t.Errorf("source = %q, want a file: label (path only, never the value)", src)
	}
}

// TestBackupKeyNeverLandsInConfigOrResponse locks the no-leak rule: the value
// goes to a 0600 file, and the config struct (which is serialized to
// hive.yaml and rendered by config APIs) holds only the path.
func TestBackupKeyNeverLandsInConfigOrResponse(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	keyPath := pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	rec := putBackupKey(t, s, `{"encryptionKey":"`+backupTestKey()+`","keyName":"escrowed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT key status = %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), backupTestKey()) {
		t.Fatal("PUT response echoed the backup key")
	}
	blob, err := json.Marshal(s.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), backupTestKey()) {
		t.Fatal("the key value was stored in the hive config")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
}

// TestBackupKeyRejectsMalformedValues — a key that cannot seal an archive must
// be refused at save time, not discovered during a backup.
func TestBackupKeyRejectsMalformedValues(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	keyPath := pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	for _, tc := range []struct{ name, body string }{
		{"empty", `{"encryptionKey":"  "}`},
		{"missing", `{}`},
		{"too short", `{"encryptionKey":"abcd"}`},
		{"non-hex", `{"encryptionKey":"zz23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := putBackupKey(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(keyPath); err == nil {
				t.Fatal("a rejected key must not be written to disk")
			}
			if s.deps.Config.Governor.Backup.KeyFile != "" {
				t.Fatal("a rejected key must not be recorded in config")
			}
		})
	}
}

// TestBackupKeyClearRefusesBackupsAgain — fail-closed after a revoke.
func TestBackupKeyClearRefusesBackupsAgain(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	t.Setenv("HIVE_SPOKE_BACKUP_DATA_DIR", t.TempDir())
	keyPath := pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	if rec := putBackupKey(t, s, `{"encryptionKey":"`+backupTestKey()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT key status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/config/governor/backup", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleBackupKeyClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("key file still present after clear: %v", err)
	}
	if s.deps.Config.Governor.Backup.KeyFile != "" {
		t.Error("key_file still recorded after clear")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/backup", nil)
	markOwnerRequest(req)
	rec = httptest.NewRecorder()
	s.handleBackupDownload(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("backup after clear = %d, want 412 (fail-closed)", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("must not offer a download when refusing: %q", cd)
	}
}

// TestBackupKeyEndpointsDenyNonOwners — the key decrypts an archive holding
// this hive's GitHub App private keys, so only the owner may touch it.
func TestBackupKeyEndpointsDenyNonOwners(t *testing.T) {
	pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	for _, role := range []string{"read", "read-write", "write", "viewer"} {
		for _, tc := range []struct {
			name string
			fn   func(http.ResponseWriter, *http.Request)
			verb string
		}{
			{"status", s.handleBackupKeyStatus, http.MethodGet},
			{"set", s.handleBackupKeySet, http.MethodPut},
			{"clear", s.handleBackupKeyClear, http.MethodDelete},
		} {
			req := httptest.NewRequest(tc.verb, "/api/config/governor/backup",
				strings.NewReader(`{"encryptionKey":"`+backupTestKey()+`"}`))
			req.Header.Set("X-Hive-Role", role)
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s as %q: status = %d, want 403", tc.name, role, rec.Code)
			}
		}
	}
}

// TestBackupStatusAvailableFromGovernorConfig — the UI probe must report the
// menu entry as usable when the key came from governor config, not only from
// the deployment environment.
func TestBackupStatusAvailableFromGovernorConfig(t *testing.T) {
	t.Setenv("HIVE_BACKUP_KEY", "")
	pointBackupKeyAtTempDir(t)
	s := newBackupKeyTestServer(t)

	if rec := putBackupKey(t, s, `{"encryptionKey":"`+backupTestKey()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT key status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
	markOwnerRequest(req)
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
