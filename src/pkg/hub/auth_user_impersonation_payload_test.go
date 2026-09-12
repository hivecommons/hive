package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Coverage for the impersonation fold-in of handleAuthUser (oauth.go): when an
// admin is viewing as another user, the auth payload must render AS the target
// (login, avatar, country) while keeping hub_admin FALSE and naming the real
// admin — the dashboard's "Viewing as … read-only" banner is driven entirely
// by this one payload.

// impersonatedAuthUser performs GET /api/auth/user as the hub admin carrying a
// signed impersonation grant for target, returning the decoded payload.
func impersonatedAuthUser(t *testing.T, s *HubServer, target string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/auth/user", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	req.AddCookie(&http.Cookie{
		Name:  impersonateCookieName,
		Value: mintImpersonateCookieValueForGeneration(s.currentGenerations(), hubAdminUsername, target, time.Now()),
	})
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("impersonated /api/auth/user = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding auth payload: %v (body %q)", err, rec.Body.String())
	}
	return payload
}

// TestAuthUserImpersonationPayload pins the full swap: identity fields render
// as the target, hub_admin is forced false, and the target's country replaces
// the admin's (no stale flag survives the swap).
func TestAuthUserImpersonationPayload(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUserCountry(t, hubAdminUsername, "US")
	mkUserCountry(t, "alice", "DE")

	payload := impersonatedAuthUser(t, s, "alice")

	if payload["authenticated"] != true {
		t.Fatal("impersonated request must still report authenticated")
	}
	if payload["impersonating"] != true {
		t.Fatal("payload must flag impersonation for the read-only banner")
	}
	if payload["hub_admin"] != false {
		t.Error("hub_admin must be FALSE during impersonation — the admin is deliberately a normal user")
	}
	login, _ := payload["login"].(string)
	viewingAs, _ := payload["viewing_as"].(string)
	realUser, _ := payload["real_user"].(string)
	if login != viewingAs {
		t.Errorf("login %q must render as the impersonation target %q", login, viewingAs)
	}
	if login == "" || login == realUser {
		t.Errorf("login = %q, real_user = %q: the effective identity must be the target, not the admin", login, realUser)
	}
	if realUser == "" {
		t.Error("real_user must name the admin behind the grant")
	}
	if got := payload["country"]; got != "DE" {
		t.Errorf("country = %v, want the TARGET's country DE, not the admin's US", got)
	}
}

// TestAuthUserImpersonationCountryDeleted pins the delete-not-blank rule: when
// the target has no country, the admin's own country must not leak through —
// the key is absent entirely.
func TestAuthUserImpersonationCountryDeleted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUserCountry(t, hubAdminUsername, "US")
	mkUser(t, "bob")

	payload := impersonatedAuthUser(t, s, "bob")

	if got, present := payload["country"]; present {
		t.Errorf("country = %v, want the key ABSENT: the admin's flag must not sit beside the target's face", got)
	}
	if payload["impersonating"] != true || payload["hub_admin"] != false {
		t.Errorf("impersonation flags wrong: impersonating=%v hub_admin=%v", payload["impersonating"], payload["hub_admin"])
	}
}

// TestAuthUserOwnCountryShipped pins the non-impersonated country branch: a
// viewer with a stored country gets it in the payload as the alpha-2 code, and
// a viewer without one gets no key at all (payload stays byte-identical to
// before the feature).
func TestAuthUserOwnCountryShipped(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUserCountry(t, "carol", "FR")
	mkUser(t, "dave")

	for _, tc := range []struct {
		user    string
		want    string
		present bool
	}{
		{"carol", "FR", true},
		{"dave", "", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/auth/user", nil)
		req.AddCookie(testAuthCookie(tc.user))
		rec := httptest.NewRecorder()
		s.handleAuthUser(rec, req)
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: decoding payload: %v", tc.user, err)
		}
		got, present := payload["country"]
		if present != tc.present {
			t.Errorf("%s: country key present = %v, want %v", tc.user, present, tc.present)
		}
		if tc.present && got != tc.want {
			t.Errorf("%s: country = %v, want %q", tc.user, got, tc.want)
		}
	}
}
