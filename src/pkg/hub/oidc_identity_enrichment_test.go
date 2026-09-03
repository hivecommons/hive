package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hivecommons/hive/pkg/auth"
)

// oidcCallbackRequest builds a nonce-consistent callback request for provider p.
func oidcCallbackRequest(providerName, stateNonce, oidcNonce string) *http.Request {
	state := url.QueryEscape(stateNonce + oauthStateSeparator + providerName + oauthStateSeparator + "/dashboard")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=xyz&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: stateNonce})
	req.AddCookie(&http.Cookie{Name: oidcNonceCookieName, Value: oidcNonce})
	return req
}

func ibmidHubServer(f *fakeOIDCProvider, clientID string) *HubServer {
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	s.authProviders = auth.NewRegistry(&auth.Provider{
		Name: "ibmid", DisplayName: "IBMid", IsOIDC: true,
		Issuer: f.issuer, ClientID: clientID, Scopes: []string{"openid", "email", "profile"},
		SubjectClaim: "uid",
	})
	return s
}

// TestOIDCCallback_StoresDisplayName pins that a login carrying a name claim
// persists it as the provider-asserted DisplayName on the user record.
func TestOIDCCallback_StoresDisplayName(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	f := newFakeOIDCProvider(t)
	defer f.close()

	const clientID, nonce, uid = "cid", "n-1", "310002H3UQ"
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "uid": uid, "sub": "ignored-when-uid-present",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "name": "Ada Lovelace", "email": "ada@example.com",
	})
	s := ibmidHubServer(f, clientID)

	stateNonce, _ := oauthStateNonce()
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, oidcCallbackRequest("ibmid", stateNonce, nonce))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("ibmid:" + uid)
	if u == nil {
		t.Fatal("expected ibmid user record")
	}
	if u.DisplayName != "Ada Lovelace" {
		t.Errorf("DisplayName = %q, want Ada Lovelace", u.DisplayName)
	}
	if u.Email != "ada@example.com" {
		t.Errorf("Email = %q", u.Email)
	}
}

// TestOIDCCallback_EnrichesExistingRecordOnNextLogin is the backfill guarantee:
// a provider:sub record created BEFORE enrichment shipped (no display claims)
// gets name/email upserted — same record, not a new one — on its next login.
func TestOIDCCallback_EnrichesExistingRecordOnNextLogin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	f := newFakeOIDCProvider(t)
	defer f.close()

	const clientID, uid = "cid", "310002H3UQ"
	s := ibmidHubServer(f, clientID)

	// First login: minimal id_token — the pre-enrichment record shape.
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "uid": uid,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": "n-1",
	})
	n1, _ := oauthStateNonce()
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, oidcCallbackRequest("ibmid", n1, "n-1"))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("first login status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	u := loadSaaSUser("ibmid:" + uid)
	if u == nil || u.DisplayName != "" {
		t.Fatalf("precondition: bare record expected, got %+v", u)
	}
	created := u.CreatedAt

	// Second login: the provider now sends display claims. Same record must be
	// UPDATED — not recreated — with the claims upserted.
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "uid": uid,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": "n-2", "name": "Ada Lovelace", "email": "ada@example.com",
		"picture": "https://example.com/ada.png",
	})
	n2, _ := oauthStateNonce()
	rec2 := httptest.NewRecorder()
	s.handleOAuthCallback(rec2, oidcCallbackRequest("ibmid", n2, "n-2"))
	if rec2.Code != http.StatusTemporaryRedirect {
		t.Fatalf("second login status = %d (body=%s)", rec2.Code, rec2.Body.String())
	}

	u = loadSaaSUser("ibmid:" + uid)
	if u == nil {
		t.Fatal("record vanished")
	}
	if u.DisplayName != "Ada Lovelace" || u.Email != "ada@example.com" || u.AvatarURL != "https://example.com/ada.png" {
		t.Errorf("record not enriched: %+v", u)
	}
	if u.CreatedAt != created {
		t.Errorf("CreatedAt changed %q -> %q — record was recreated, not updated", created, u.CreatedAt)
	}
	if u.LoginCount != 2 {
		t.Errorf("LoginCount = %d, want 2 (same record across both logins)", u.LoginCount)
	}
}

// TestOIDCCallback_ClaimGapNeverBlanksStoredValue: a later login missing a
// claim must not erase what an earlier login stored.
func TestOIDCCallback_ClaimGapNeverBlanksStoredValue(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	f := newFakeOIDCProvider(t)
	defer f.close()

	const clientID, uid = "cid", "310002H3UQ"
	s := ibmidHubServer(f, clientID)

	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "uid": uid,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": "n-1", "name": "Ada Lovelace", "email": "ada@example.com",
	})
	n1, _ := oauthStateNonce()
	s.handleOAuthCallback(httptest.NewRecorder(), oidcCallbackRequest("ibmid", n1, "n-1"))

	// Second login with NO display claims.
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "uid": uid,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": "n-2",
	})
	n2, _ := oauthStateNonce()
	s.handleOAuthCallback(httptest.NewRecorder(), oidcCallbackRequest("ibmid", n2, "n-2"))

	u := loadSaaSUser("ibmid:" + uid)
	if u == nil || u.DisplayName != "Ada Lovelace" || u.Email != "ada@example.com" {
		t.Errorf("stored display claims were blanked by a claimless login: %+v", u)
	}
}

// TestDisplayIdentity_PrefersDisplayName pins the navbar label order for OIDC
// users: DisplayName > Email > canonical id. GitHub users stay the bare login.
func TestDisplayIdentity_PrefersDisplayName(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}

	u := ensureSaaSUser("ibmid:310002H3UQ")
	u.DisplayName = "Ada Lovelace"
	u.Email = "ada@example.com"
	u.AvatarURL = "https://example.com/ada.png"
	saveSaaSUser(u)

	login, avatar := s.displayIdentity("ibmid:310002H3UQ")
	if login != "Ada Lovelace" {
		t.Errorf("login = %q, want the display name", login)
	}
	if avatar != "https://example.com/ada.png" {
		t.Errorf("avatar = %q", avatar)
	}

	// Without a display name, email is the label.
	u.DisplayName = ""
	saveSaaSUser(u)
	if login, _ = s.displayIdentity("ibmid:310002H3UQ"); login != "ada@example.com" {
		t.Errorf("login = %q, want the email fallback", login)
	}

	// GitHub identity: unchanged bare-login behavior.
	if login, avatar = s.displayIdentity("octocat"); login != "octocat" || avatar != "https://github.com/octocat.png" {
		t.Errorf("github displayIdentity = %q,%q", login, avatar)
	}
}
