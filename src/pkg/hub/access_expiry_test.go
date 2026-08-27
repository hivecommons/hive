package hub

// Tests for time-limited access grants (#4150): expiry parsing, read-time
// pruning in loadSaaSUser, the persistence sweep, and the handleAccessAdd
// set/preserve/clear semantics. Reuses the temp-dir and cookie fixtures from
// grantable_users_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseAccessExpiry(t *testing.T) {
	t.Run("bare date is valid through that day UTC", func(t *testing.T) {
		got, err := parseAccessExpiry("2030-06-15")
		if err != nil {
			t.Fatalf("parseAccessExpiry: %v", err)
		}
		want := time.Date(2030, 6, 16, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("rfc3339 passes through in UTC", func(t *testing.T) {
		got, err := parseAccessExpiry("2030-06-15T12:30:00+02:00")
		if err != nil {
			t.Fatalf("parseAccessExpiry: %v", err)
		}
		want := time.Date(2030, 6, 15, 10, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	for _, bad := range []string{"", "  ", "not-a-date", "2030-13-45"} {
		if _, err := parseAccessExpiry(bad); err == nil {
			t.Errorf("parseAccessExpiry(%q): want error, got nil", bad)
		}
	}
}

func TestAccessGrantExpired(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		expiry string
		want   bool
	}{
		{"empty is permanent", "", false},
		{"future not expired", now.Add(time.Hour).Format(time.RFC3339), false},
		{"past expired", now.Add(-time.Hour).Format(time.RFC3339), true},
		{"exact instant expired", now.Format(time.RFC3339), true},
		// A corrupt field must fail OPEN (permanent), never lock a user out.
		{"unparseable is permanent", "garbage", false},
	}
	for _, c := range cases {
		if got := accessGrantExpired(c.expiry, now); got != c.want {
			t.Errorf("%s: accessGrantExpired(%q) = %v, want %v", c.name, c.expiry, got, c.want)
		}
	}
}

func TestPruneExpiredHiveGrants(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	u := &SaaSUser{
		GitHubUsername: "alice",
		Hives: map[string]string{
			"h-expired":   "read-write",
			"h-future":    "read",
			"h-permanent": "owner",
		},
		HiveExpiry: map[string]string{
			"h-expired": now.Add(-time.Minute).Format(time.RFC3339),
			"h-future":  now.Add(time.Hour).Format(time.RFC3339),
			"h-orphan":  now.Add(-time.Minute).Format(time.RFC3339),
		},
	}
	revoked := pruneExpiredHiveGrants(u, now)
	if len(revoked) != 1 || revoked[0] != "h-expired" {
		t.Fatalf("revoked = %v, want [h-expired]", revoked)
	}
	if _, ok := u.Hives["h-expired"]; ok {
		t.Error("expired grant still present")
	}
	if u.Hives["h-future"] != "read" || u.Hives["h-permanent"] != "owner" {
		t.Errorf("live grants disturbed: %v", u.Hives)
	}
	if _, ok := u.HiveExpiry["h-orphan"]; ok {
		t.Error("orphaned expiry entry not cleaned")
	}
	if _, ok := u.HiveExpiry["h-future"]; !ok {
		t.Error("future expiry dropped")
	}
}

func TestLoadSaaSUserPrunesExpiredGrantsOnRead(t *testing.T) {
	withTempSaaSDirs(t)
	err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "bob",
		Hives:          map[string]string{"h1": "read-write", "h2": "read"},
		HiveExpiry: map[string]string{
			"h1": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	u := loadSaaSUser("bob")
	if u == nil {
		t.Fatal("loadSaaSUser returned nil")
	}
	if _, ok := u.Hives["h1"]; ok {
		t.Error("expired grant survived read-time prune")
	}
	if u.Hives["h2"] != "read" {
		t.Errorf("unbounded grant disturbed: %v", u.Hives)
	}
}

func TestSweepExpiredAccessPersistsAndAudits(t *testing.T) {
	withTempSaaSDirs(t)
	s := grantableTestServer()
	err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "carol",
		Hives:          map[string]string{"h1": "merger"},
		HiveExpiry: map[string]string{
			"h1": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	s.sweepExpiredAccess(time.Now())
	// The prune must be PERSISTED: bypass loadSaaSUser's read-time filter and
	// check the raw record.
	raw, err := readSaaSUserFile("carol")
	if err != nil {
		t.Fatalf("readSaaSUserFile: %v", err)
	}
	var u SaaSUser
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := u.Hives["h1"]; ok {
		t.Error("sweep did not persist the revocation")
	}
	if _, ok := u.HiveExpiry["h1"]; ok {
		t.Error("sweep did not persist the expiry cleanup")
	}
}

// postAccess calls handleAccessAdd as username for hive h1 with the raw body.
func postAccess(t *testing.T, s *HubServer, username, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/h1/access", strings.NewReader(body))
	req.SetPathValue("id", "h1")
	req.AddCookie(&http.Cookie{
		Name:  "hive_hub_user",
		Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), username),
	})
	rec := httptest.NewRecorder()
	s.handleAccessAdd(rec, req)
	return rec
}

func TestHandleAccessAddExpirySemantics(t *testing.T) {
	withTempSaaSDirs(t)
	s := grantableTestServer()
	putHiveOwnedBy(t, "h1", "owner1")
	putUser(t, "owner1")
	putUser(t, "dave")

	t.Run("grant with expiry stores canonical instant", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"read","expires_at":"2099-06-15"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
		}
		u := loadSaaSUser("dave")
		want := time.Date(2099, 6, 16, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if u.HiveExpiry["h1"] != want {
			t.Fatalf("expiry = %q, want %q", u.HiveExpiry["h1"], want)
		}
	})

	t.Run("role change without expires_at preserves expiry", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"read-write"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
		}
		u := loadSaaSUser("dave")
		if u.Hives["h1"] != "read-write" {
			t.Fatalf("role = %q", u.Hives["h1"])
		}
		if u.HiveExpiry["h1"] == "" {
			t.Fatal("role change cleared the expiry; it must be preserved")
		}
	})

	t.Run("empty expires_at clears expiry", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"read-write","expires_at":""}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
		}
		u := loadSaaSUser("dave")
		if _, ok := u.HiveExpiry["h1"]; ok {
			t.Fatal("empty expires_at did not clear the expiry")
		}
	})

	t.Run("past expiry rejected", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"read","expires_at":"2001-01-01"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("garbage expiry rejected", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"read","expires_at":"whenever"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("expiry on the only owner rejected", func(t *testing.T) {
		// owner1 is the hive's only granted owner: give them the stored role
		// first, then try to time-limit it.
		if rec := postAccess(t, s, "owner1", `{"username":"owner1","role":"owner"}`); rec.Code != http.StatusOK {
			t.Fatalf("seed owner grant: status = %d", rec.Code)
		}
		rec := postAccess(t, s, "owner1", `{"username":"owner1","role":"owner","expires_at":"2099-01-01"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("expiry on a co-owner allowed once another owner exists", func(t *testing.T) {
		rec := postAccess(t, s, "owner1", `{"username":"dave","role":"owner","expires_at":"2099-01-01"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
		}
	})
}

func TestAccessListIncludesExpiry(t *testing.T) {
	withTempSaaSDirs(t)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "erin",
		Hives:          map[string]string{"h1": "read"},
		HiveExpiry:     map[string]string{"h1": future},
	})
	if err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	access := accessForHive("h1", listAllSaaSUsers(), false)
	if len(access) != 1 {
		t.Fatalf("access rows = %d, want 1", len(access))
	}
	if access[0].ExpiresAt != future {
		t.Errorf("ExpiresAt = %q, want %q", access[0].ExpiresAt, future)
	}
}

// TestAccessExpiryIsCheckboxGated pins the control that decides whether a grant
// expires at all.
//
// An <input type="date"> cannot express "no expiry": an empty one still paints
// the browser's own mm/dd/yyyy placeholder, so a permanent grant and one
// expiring today look identical. Labelling the field was not enough — the row
// then rendered "Expires: Never 08/27/2026", stating both answers at once. A
// checkbox owns the state and the date input exists only when it is checked,
// so exactly one answer is ever visible.
func TestAccessExpiryIsCheckboxGated(t *testing.T) {
	// Add User: a checkbox gates the date, and the date starts hidden.
	if !strings.Contains(dashboardHTML, `id="access-expiry-enabled"`) {
		t.Error("Add User has no expiry checkbox — a bare date input cannot express \"no expiry\"")
	}
	if !strings.Contains(dashboardHTML, `id="access-expiry" type="date" title="Access is revoked automatically after this date (UTC)."`) {
		t.Error("the Add User date input lost its title, or its markup moved")
	}
	if !strings.Contains(dashboardHTML, `style="display:none;padding:8px 12px`) {
		t.Error("the Add User date input must start hidden — visible-but-empty is the placeholder bug this fixes")
	}
	if !strings.Contains(dashboardHTML, "function toggleAddExpiryVisible()") {
		t.Error("toggleAddExpiryVisible is missing — the checkbox would not show or hide the date")
	}

	// Per-user rows: same gate, and an unchecked row says "Never" in words
	// rather than showing a date nobody chose.
	if !strings.Contains(dashboardHTML, "function toggleAccessExpiry(") {
		t.Error("toggleAccessExpiry is missing — row checkboxes could not flip a grant to permanent")
	}
	if !strings.Contains(dashboardHTML, `<span style="font-size:0.6rem;color:var(--text)">Never</span>`) {
		t.Error(`a row with no expiry must render the word "Never" instead of a date input`)
	}

	// Turning expiry ON must not submit today: that would revoke the grant at
	// once, which reads as the UI cancelling access rather than scheduling it.
	if !strings.Contains(dashboardHTML, "var defaultExpiryDays = 30;") {
		t.Error("defaultExpiryDays must exist and be non-zero — checking the box otherwise expires the grant immediately")
	}

	// Adding a user must reset BOTH halves. Clearing the date but leaving the
	// box checked would show an expiry while submitting '' (permanent).
	if !strings.Contains(dashboardHTML, "document.getElementById('access-expiry-enabled').checked = false;") {
		t.Error("addAccess must reset the expiry checkbox, not just the date, or the form desyncs from what it submits")
	}
}
