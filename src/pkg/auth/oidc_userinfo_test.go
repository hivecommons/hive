package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// minimalIDToken returns an IBMid-style minimal claim set: identity + nonce,
// NO display claims (name/email/picture).
func minimalIDToken(issuer, aud, nonce, sub string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer, "aud": aud, "sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce,
	}
}

func TestUserInfo_EnrichesMissingDisplayClaims(t *testing.T) {
	// The production gap: a minimal id_token stores nothing to display. The
	// userinfo endpoint carries name/email/picture — Exchange must fill them.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce, sub = "cid", "n1", "310002H3UQ"
	o.idToken = o.signToken(t, minimalIDToken(o.issuer, clientID, nonce, sub))
	o.accessToken = "at-123"
	o.userInfo = map[string]any{
		"sub": sub, "name": "Ada Lovelace",
		"email": "ada@example.com", "picture": "https://example.com/ada.png",
	}

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != sub {
		t.Errorf("subject = %q", claims.Subject)
	}
	if claims.Name != "Ada Lovelace" || claims.Email != "ada@example.com" || claims.AvatarURL != "https://example.com/ada.png" {
		t.Errorf("enriched claims = %+v, want userinfo display claims", claims)
	}
	if o.userInfoCalls != 1 {
		t.Errorf("userinfo calls = %d, want 1", o.userInfoCalls)
	}
}

func TestUserInfo_SkippedWhenIDTokenComplete(t *testing.T) {
	// A complete id_token needs no enrichment: no userinfo round-trip.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce = "cid", "n1"
	o.idToken = o.signToken(t, baseClaims(o.issuer, clientID, nonce))
	o.accessToken = "at-123"
	o.userInfo = map[string]any{"sub": "1078901234567890", "name": "Should Not Be Fetched"}

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Name != "A Person" {
		t.Errorf("name = %q, want the id_token's own name", claims.Name)
	}
	if o.userInfoCalls != 0 {
		t.Errorf("userinfo calls = %d, want 0 (id_token already complete)", o.userInfoCalls)
	}
}

func TestUserInfo_SubMismatchDiscarded(t *testing.T) {
	// OIDC Core §5.3.2: userinfo's sub MUST match the id_token's. A mismatched
	// response must be discarded wholesale — none of its claims may be trusted.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce, sub = "cid", "n1", "real-sub"
	o.idToken = o.signToken(t, minimalIDToken(o.issuer, clientID, nonce, sub))
	o.accessToken = "at-123"
	o.userInfo = map[string]any{
		"sub": "SOMEONE-ELSE", "name": "Mallory", "email": "mallory@evil.example",
	}

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Name != "" || claims.Email != "" {
		t.Errorf("claims = %+v, want mismatched userinfo fully discarded", claims)
	}
	if claims.Subject != sub {
		t.Errorf("subject = %q, must stay the id_token's", claims.Subject)
	}
}

func TestUserInfo_FailureNeverFailsLogin(t *testing.T) {
	// The login is already verified; a broken userinfo endpoint (404) must be a
	// silent no-op, not an error.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce = "cid", "n1"
	o.idToken = o.signToken(t, minimalIDToken(o.issuer, clientID, nonce, "s1"))
	o.accessToken = "at-123"
	// o.userInfo stays nil → /userinfo answers 404.

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange must not fail on userinfo error: %v", err)
	}
	if claims.Subject != "s1" {
		t.Errorf("subject = %q", claims.Subject)
	}
}

func TestUserInfo_NoAccessTokenNoFetch(t *testing.T) {
	// A token response without access_token cannot authorize a userinfo call.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce = "cid", "n1"
	o.idToken = o.signToken(t, minimalIDToken(o.issuer, clientID, nonce, "s1"))
	// accessToken deliberately empty.
	o.userInfo = map[string]any{"sub": "s1", "name": "X"}

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if o.userInfoCalls != 0 {
		t.Errorf("userinfo calls = %d, want 0 without an access token", o.userInfoCalls)
	}
}

func TestDisplayName_GivenFamilyComposition(t *testing.T) {
	// IBMid/Entra often carry given_name/family_name without a composite name.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID, nonce = "cid", "n1"
	c := minimalIDToken(o.issuer, clientID, nonce, "s1")
	c["given_name"] = "Grace"
	c["family_name"] = "Hopper"
	c["email"] = "grace@example.com"
	c["picture"] = "https://example.com/g.png"
	o.idToken = o.signToken(t, c)

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Name != "Grace Hopper" {
		t.Errorf("name = %q, want given+family composition", claims.Name)
	}
}
