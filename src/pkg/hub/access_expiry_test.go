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

// TestAccessExpiryFieldIsLabelled pins the VISIBLE label on the Manage Access
// expiry controls.
//
// An empty <input type="date"> is not blank on screen: browsers render their
// own mm/dd/yyyy placeholder, which reads as a date that has already been
// chosen. Without a label beside it, an operator cannot tell that the box is an
// expiry, which direction it acts in, or that leaving it alone is a valid
// answer meaning "permanent". A title tooltip does not cover this — it is
// invisible until hover and unavailable on touch.
//
// The field is and must stay OPTIONAL: nothing pre-fills it, and addAccess
// sends whatever is there, so empty means permanent.
func TestAccessExpiryFieldIsLabelled(t *testing.T) {
	// The Add User control carries a visible label naming the field and saying
	// an empty value is allowed.
	if !strings.Contains(dashboardHTML, `<label for="access-expiry"`) {
		t.Error("the Add User expiry input has no visible <label> — a bare date box does not say it is an expiry")
	}
	if !strings.Contains(dashboardHTML, "Expires <span style=\"opacity:0.75\">(optional)</span>") {
		t.Error(`the Add User expiry label must say "Expires (optional)": the name gives the field meaning, "(optional)" says an empty field is a valid answer`)
	}

	// Per-user rows: a grant with no expiry must SAY it is permanent rather
	// than leaving the reader to infer it from a box that looks filled in.
	if !strings.Contains(dashboardHTML, `Expires: <span style="color:var(--text)">Never</span>`) {
		t.Error(`a grant with no expiry must render "Expires: Never" — otherwise a permanent grant is indistinguishable from one expiring today`)
	}

	// The input must not gain a default. A pre-filled expiry would silently
	// convert every new grant into a temporary one.
	if strings.Contains(dashboardHTML, `id="access-expiry" type="date" value=`) {
		t.Error("the expiry input must not carry a default value — empty means permanent, and a default would make every new grant expire")
	}
}
