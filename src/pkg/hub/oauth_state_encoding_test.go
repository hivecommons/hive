package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

// ibmidStateHub returns a HubServer whose registry carries an ibmid provider,
// enough for state verification and provider parsing (no Exchange happens).
func ibmidStateHub() *HubServer {
	return &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name: "ibmid", DisplayName: "IBMid", IsOIDC: true,
			Issuer: "https://login.ibm.com/oidc/endpoint/default", ClientID: "cid",
		}),
	}
}

// TestCallbackState_ProductionIBMidShape replays the EXACT state shape captured
// from the failing production IBMid callback:
//
//	state=<hex-nonce>%253Aibmid%253A
//
// %253A is ':' escaped twice — which is precisely what the hub itself sends:
// startProviderLogin QueryEscapes "nonce:ibmid:" once and AuthCodeURL's
// url.Values.Encode() escapes it again, so a spec-compliant IdP returning state
// verbatim produces %253A on the callback. This test pins that the double-decode
// path verifies the nonce and parses provider=ibmid — i.e. the production
// failure was NOT in state handling (a state failure returns "invalid login
// state", HTTP 400, not the observed "could not verify your identity", 502).
func TestCallbackState_ProductionIBMidShape(t *testing.T) {
	s := ibmidStateHub()
	const nonce = "f4948cc201204f6617040d9c88efda41a62beaa836c5a52a6d39dfdcba2062cc"
	// Literal RawQuery as captured (code/grant_id present, state double-encoded).
	target := "/api/auth/callback?code=oo0dDhveSh9UL7JhbCjKfcA_gqNG3glQWzSu7bvZbtM.jZWkqwJ1IYYXSz05GqH-o-4h9eEDwPMhWoXfLuuBHDRvpSVhuxo2hwGcaTQ5Z7KTEUlqZmMVB5JFygQj9lRdxQ" +
		"&grant_id=7c4576a5-892a-42ef-b63d-8ee8a1f67960" +
		"&state=" + nonce + "%253Aibmid%253A"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})

	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("production-shaped double-encoded state must verify against the cookie nonce")
	}
	provider, redirect := s.parseCallbackState(req)
	if provider != "ibmid" {
		t.Errorf("provider = %q, want ibmid", provider)
	}
	if redirect != "/dashboard" {
		t.Errorf("redirect = %q, want /dashboard default for empty redirect", redirect)
	}
}

// TestCallbackState_FullAuthorizeRoundTrip simulates the hub's OIDC state
// encoding exactly (QueryEscape in startProviderLogin, then url.Values.Encode in
// AuthCodeURL), lets a spec-compliant IdP parse the authorize query once and
// re-encode the state verbatim onto the callback, and confirms verification +
// provider/redirect parsing. This locks the encode/decode contract end to end.
func TestCallbackState_FullAuthorizeRoundTrip(t *testing.T) {
	s := ibmidStateHub()
	nonce, _ := oauthStateNonce()
	state := url.QueryEscape(nonce + oauthStateSeparator + "ibmid" + oauthStateSeparator + "/my-hives")
	// The authorize hop re-encodes state (url.Values.Encode in AuthCodeURL):
	authorizeQuery := url.Values{"state": {state}}.Encode()
	// A compliant IdP parses the authorize query once...
	parsed, err := url.ParseQuery(authorizeQuery)
	if err != nil {
		t.Fatal(err)
	}
	idpSeenState := parsed.Get("state") // single-encoded again
	// ...and re-encodes it verbatim onto the callback:
	callbackQuery := url.Values{"code": {"c"}, "state": {idpSeenState}}.Encode()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?"+callbackQuery, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})

	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("round-tripped OIDC state must verify")
	}
	provider, redirect := s.parseCallbackState(req)
	if provider != "ibmid" || redirect != "/my-hives" {
		t.Errorf("provider,redirect = %q,%q, want ibmid,/my-hives", provider, redirect)
	}
}

// TestCallbackState_ToleratesPreDecodedState covers an IdP that DECODES the
// state before returning it (non-verbatim, seen in the wild): the callback then
// carries the state only single-encoded, and the standard second unescape is a
// destructive over-decode ONLY if the state contains '%'-sequences. The
// cookie-gated candidate selection must still match.
func TestCallbackState_ToleratesPreDecodedState(t *testing.T) {
	s := ibmidStateHub()
	nonce, _ := oauthStateNonce()
	// IdP returned "nonce:ibmid:/a b" decoded; its callback encodes it once.
	plain := nonce + oauthStateSeparator + "ibmid" + oauthStateSeparator + "/a b"
	req := httptest.NewRequest(http.MethodGet,
		"/api/auth/callback?code=c&state="+url.QueryEscape(plain), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})

	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("pre-decoded state must still verify via the raw candidate")
	}
	provider, redirect := s.parseCallbackState(req)
	if provider != "ibmid" || redirect != "/a b" {
		t.Errorf("provider,redirect = %q,%q, want ibmid,'/a b'", provider, redirect)
	}
}

// TestCallbackState_ToleratesExtraEncodedState covers an IdP/intermediate that
// RE-encoded the (already double-encoded) state once more: triple-encoded on
// the wire, matched by the depth-2 fallback candidate.
func TestCallbackState_ToleratesExtraEncodedState(t *testing.T) {
	s := ibmidStateHub()
	nonce, _ := oauthStateNonce()
	once := url.QueryEscape(nonce + oauthStateSeparator + "ibmid" + oauthStateSeparator)
	twice := url.QueryEscape(once)
	thrice := url.QueryEscape(twice)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=c&state="+thrice, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})

	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("triple-encoded state must verify via the extra-decode fallback")
	}
	if provider, _ := s.parseCallbackState(req); provider != "ibmid" {
		t.Errorf("provider = %q, want ibmid", provider)
	}
}

// TestCallbackState_StillRejectsMismatch pins fail-closed behavior: no decode
// depth may rescue a state whose nonce is not this browser's cookie.
func TestCallbackState_StillRejectsMismatch(t *testing.T) {
	s := ibmidStateHub()
	nonce, _ := oauthStateNonce()
	other, _ := oauthStateNonce()
	state := url.QueryEscape(url.QueryEscape(other + oauthStateSeparator + "ibmid" + oauthStateSeparator))
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=c&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("a foreign nonce must be rejected at every decode depth")
	}

	// Missing cookie and missing state also fail closed.
	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=c&state="+state, nil)
	if s.verifyOAuthStateNonce(req2) {
		t.Fatal("missing state cookie must be rejected")
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=c", nil)
	req3.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	if s.verifyOAuthStateNonce(req3) {
		t.Fatal("missing state parameter must be rejected")
	}
}
