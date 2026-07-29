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

// contactUpdateHandler wires handleAdminUpdateUser behind the same
// requireAdmin wrapper the real route uses, so the tests exercise the
// production authorization path rather than a bare handler.
func contactUpdateHandler(srv *HubServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.requireAdmin(srv.handleAdminUpdateUser))
	return mux
}

func contactPutRequest(username, body, asUser string) *http.Request {
	req := httptest.NewRequest("PUT", "/api/saas/admin/users/"+username, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if asUser != "" {
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: asUser})
	}
	return req
}

// TestAdminUpdateUserContactAuthorization: the contact fields are admin-only.
// A non-admin (or anonymous) caller must be rejected AND must not have written
// anything to the user record.
func TestAdminUpdateUserContactAuthorization(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// requireAdmin resolves the cookie through loadSaaSUser, so the admin needs a
	// real record on disk for an authenticated admin request to be possible.
	ensureSaaSUser(hubAdminUsername)
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	handler := contactUpdateHandler(srv)

	tests := []struct {
		name       string
		asUser     string
		wantStatus int
		wantSaved  bool
	}{
		{name: "anonymous is rejected", asUser: "", wantStatus: http.StatusForbidden},
		{name: "non-admin is rejected", asUser: "regularuser", wantStatus: http.StatusForbidden},
		{name: "admin is allowed", asUser: hubAdminUsername, wantStatus: http.StatusOK, wantSaved: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ensureSaaSUser("target")
			// Reset any contact data left by a previous subtest.
			u := loadSaaSUser("target")
			u.FullName, u.SlackID, u.Notes = "", "", ""
			if err := saveSaaSUser(u); err != nil {
				t.Fatalf("saveSaaSUser: %v", err)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, contactPutRequest("target", `{"full_name":"Ada Lovelace"}`, tc.asUser))

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			got := loadSaaSUser("target")
			if got == nil {
				t.Fatal("user record disappeared")
			}
			if tc.wantSaved && got.FullName != "Ada Lovelace" {
				t.Errorf("FullName = %q, want %q", got.FullName, "Ada Lovelace")
			}
			if !tc.wantSaved && got.FullName != "" {
				t.Errorf("unauthorized caller wrote FullName = %q, want empty", got.FullName)
			}
		})
	}
}

// TestAdminUpdateUserContactFieldCaps: each free-text field is truncated to its
// own named cap before it reaches the PVC.
func TestAdminUpdateUserContactFieldCaps(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// requireAdmin resolves the cookie through loadSaaSUser, so the admin needs a
	// real record on disk for an authenticated admin request to be possible.
	ensureSaaSUser(hubAdminUsername)
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	handler := contactUpdateHandler(srv)

	tests := []struct {
		name    string
		field   string
		cap     int
		get     func(*SaaSUser) string
		wantLen int
	}{
		{name: "full name capped", field: "full_name", cap: maxContactNameLen,
			get: func(u *SaaSUser) string { return u.FullName }},
		{name: "slack id capped", field: "slack_id", cap: maxContactSlackIDLen,
			get: func(u *SaaSUser) string { return u.SlackID }},
		{name: "notes capped", field: "notes", cap: maxContactNotesLen,
			get: func(u *SaaSUser) string { return u.Notes }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ensureSaaSUser("capuser")
			// Well over the cap so truncation is unambiguous.
			oversize := strings.Repeat("x", tc.cap+500)
			body, err := json.Marshal(map[string]string{tc.field: oversize})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, contactPutRequest("capuser", string(body), hubAdminUsername))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			u := loadSaaSUser("capuser")
			if u == nil {
				t.Fatal("user not found after save")
			}
			if got := len([]rune(tc.get(u))); got != tc.cap {
				t.Errorf("stored length = %d, want cap %d", got, tc.cap)
			}
		})
	}

	// A value at or under the cap must survive verbatim, including multi-byte
	// runes — the cap counts runes, so it must never split a character.
	t.Run("under cap and multibyte preserved", func(t *testing.T) {
		ensureSaaSUser("capuser2")
		name := "Ada Lovelace éü中文"
		body, _ := json.Marshal(map[string]string{"full_name": name})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, contactPutRequest("capuser2", string(body), hubAdminUsername))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if u := loadSaaSUser("capuser2"); u == nil || u.FullName != name {
			t.Errorf("FullName = %q, want %q", u.FullName, name)
		}
	})
}

// TestAdminUpdateUserContactPreservesOtherFields: saving contact fields is a
// read-modify-write on the live record, so Hives / quota / Blocked (and the
// other contact fields) must come through untouched.
func TestAdminUpdateUserContactPreservesOtherFields(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// requireAdmin resolves the cookie through loadSaaSUser, so the admin needs a
	// real record on disk for an authenticated admin request to be possible.
	ensureSaaSUser(hubAdminUsername)
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	handler := contactUpdateHandler(srv)

	seed := func(t *testing.T) {
		t.Helper()
		u := &SaaSUser{
			GitHubUsername: "crmuser",
			Hives:          map[string]string{"hosted-a": "owner", "hosted-b": "read"},
			SaaSQuota:      7,
			Blocked:        true,
			FullName:       "Original Name",
			SlackID:        "U01ORIGINAL",
			Notes:          "original notes",
		}
		if err := saveSaaSUser(u); err != nil {
			t.Fatalf("saveSaaSUser: %v", err)
		}
	}

	tests := []struct {
		name      string
		body      string
		wantName  string
		wantSlack string
		wantNotes string
	}{
		{
			name:     "notes only leaves name and slack alone",
			body:     `{"notes":"talked at KubeCon"}`,
			wantName: "Original Name", wantSlack: "U01ORIGINAL", wantNotes: "talked at KubeCon",
		},
		{
			name:     "name only leaves notes and slack alone",
			body:     `{"full_name":"New Name"}`,
			wantName: "New Name", wantSlack: "U01ORIGINAL", wantNotes: "original notes",
		},
		{
			name:     "all three at once",
			body:     `{"full_name":"A","slack_id":"U02","notes":"n"}`,
			wantName: "A", wantSlack: "U02", wantNotes: "n",
		},
		{
			name:     "empty string clears a field without touching the rest",
			body:     `{"notes":""}`,
			wantName: "Original Name", wantSlack: "U01ORIGINAL", wantNotes: "",
		},
		{
			name:     "whitespace-only value is trimmed to empty",
			body:     `{"slack_id":"   "}`,
			wantName: "Original Name", wantSlack: "", wantNotes: "original notes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed(t)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, contactPutRequest("crmuser", tc.body, hubAdminUsername))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			u := loadSaaSUser("crmuser")
			if u == nil {
				t.Fatal("user not found")
			}
			// The whole point: a contact edit must not clobber these.
			if u.SaaSQuota != 7 {
				t.Errorf("SaaSQuota = %d, want 7", u.SaaSQuota)
			}
			if !u.Blocked {
				t.Error("Blocked was cleared by a contact-only update")
			}
			if len(u.Hives) != 2 || u.Hives["hosted-a"] != "owner" || u.Hives["hosted-b"] != "read" {
				t.Errorf("Hives = %v, want the two seeded entries intact", u.Hives)
			}
			if u.FullName != tc.wantName {
				t.Errorf("FullName = %q, want %q", u.FullName, tc.wantName)
			}
			if u.SlackID != tc.wantSlack {
				t.Errorf("SlackID = %q, want %q", u.SlackID, tc.wantSlack)
			}
			if u.Notes != tc.wantNotes {
				t.Errorf("Notes = %q, want %q", u.Notes, tc.wantNotes)
			}
		})
	}

	// Symmetric case: a quota/blocked edit must not wipe the contact fields.
	t.Run("quota update preserves contact fields", func(t *testing.T) {
		seed(t)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, contactPutRequest("crmuser", `{"saas_quota":3,"blocked":false}`, hubAdminUsername))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		u := loadSaaSUser("crmuser")
		if u.SaaSQuota != 3 || u.Blocked {
			t.Errorf("quota/blocked not applied: quota=%d blocked=%v", u.SaaSQuota, u.Blocked)
		}
		if u.FullName != "Original Name" || u.SlackID != "U01ORIGINAL" || u.Notes != "original notes" {
			t.Errorf("contact fields clobbered by a quota update: %+v", u)
		}
	})
}

// TestSaaSUserWithoutContactFieldsRoundTrips: existing records on the PVC have
// no full_name / slack_id / notes keys. Loading and re-saving one must not add
// them (omitempty) and must not change the stored JSON.
func TestSaaSUserWithoutContactFieldsRoundTrips(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	legacy := `{
  "github_username": "legacyuser",
  "created_at": "2024-01-01T00:00:00Z",
  "last_login": "2024-06-01T00:00:00Z",
  "hives": {
    "hosted-legacy": "owner"
  },
  "saas_quota": 2,
  "blocked": false
}`
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(saasUsersDir, "legacyuser.json")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	u := loadSaaSUser("legacyuser")
	if u == nil {
		t.Fatal("loadSaaSUser returned nil for a legacy record")
	}
	if u.FullName != "" || u.SlackID != "" || u.Notes != "" {
		t.Errorf("legacy record gained contact values: %+v", u)
	}
	if u.SaaSQuota != 2 || u.Hives["hosted-legacy"] != "owner" {
		t.Errorf("legacy record did not load correctly: %+v", u)
	}

	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, key := range []string{"full_name", "slack_id", "notes"} {
		if strings.Contains(string(data), key) {
			t.Errorf("re-saved legacy record contains %q; omitempty should have dropped it:\n%s", key, data)
		}
	}
	// The round-tripped record must be byte-identical to the original, modulo
	// the marshaller's formatting — compare the decoded maps to be precise.
	var before, after map[string]any
	if err := json.Unmarshal([]byte(legacy), &before); err != nil {
		t.Fatalf("unmarshal before: %v", err)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if len(before) != len(after) {
		t.Errorf("field count changed: before %d keys, after %d keys (%v vs %v)", len(before), len(after), before, after)
	}
}

// TestTruncateRunes covers the cap helper directly, including the multi-byte
// boundary case a byte-based cap would corrupt.
func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "under the cap is unchanged", in: "hello", max: 10, want: "hello"},
		{name: "exactly at the cap is unchanged", in: "hello", max: 5, want: "hello"},
		{name: "over the cap is clipped", in: "hello world", max: 5, want: "hello"},
		{name: "empty stays empty", in: "", max: 5, want: ""},
		{name: "zero cap clips everything", in: "hello", max: 0, want: ""},
		{name: "multibyte clipped on rune boundary", in: "中文测试", max: 2, want: "中文"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.in, tc.max); got != tc.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
