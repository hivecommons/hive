package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ibmidLikeProvider mirrors the registry's IBMid spec: OIDC with
// SubjectClaim="uid".
func ibmidLikeProvider(o *oidcTestServer, clientID string) *Provider {
	return &Provider{
		Name: "ibmid", DisplayName: "IBMid", IsOIDC: true,
		Issuer: o.issuer, ClientID: clientID, Scopes: []string{"openid", "email", "profile"},
		SubjectClaim: "uid",
	}
}

func TestVerify_IBMidFallsBackToSubWhenUidAbsent(t *testing.T) {
	// The production IBMid failure: the id_token carries the OIDC-required "sub"
	// but NOT "uid" (claims_supported describes userinfo, not the id_token).
	// SubjectClaim="uid" must fall back to "sub" instead of failing the login.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ibm-client"
	const nonce = "n-ibm"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": o.issuer, "aud": clientID,
		"sub": "6500001ABC", // OIDC-required subject; no "uid" claim
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "email": "someone@example.com", "name": "Some One",
	})
	p := ibmidLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "6500001ABC" {
		t.Errorf("subject = %q, want fallback to sub 6500001ABC", claims.Subject)
	}
	if claims.Email != "someone@example.com" {
		t.Errorf("email = %q", claims.Email)
	}
}

func TestVerify_IBMidPrefersUidOverSub(t *testing.T) {
	// When BOTH uid and sub are present, the configured claim (uid) wins.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ibm-client"
	const nonce = "n-ibm"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": o.issuer, "aud": clientID,
		"uid": "2700ABC123", "sub": "opaque-sub-value",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce,
	})
	p := ibmidLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "2700ABC123" {
		t.Errorf("subject = %q, want the configured uid claim to win", claims.Subject)
	}
}

func TestVerify_NoSubjectAtAllStillFails(t *testing.T) {
	// Neither the configured claim nor "sub" present → fail closed, naming both.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ibm-client"
	const nonce = "n-ibm"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": o.issuer, "aud": clientID,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "email": "x@example.com",
	})
	p := ibmidLikeProvider(o, clientID)
	_, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce)
	if err == nil {
		t.Fatal("expected rejection when no subject claim exists")
	}
	if !strings.Contains(err.Error(), "no subject") {
		t.Errorf("error = %v, want a no-subject error", err)
	}
}

func TestVerify_AcceptsPS256(t *testing.T) {
	// IBMid advertises PS256 among id_token_signing_alg_values_supported; the
	// same RSA JWKS key verifies RSA-PSS signatures. Must be accepted.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	tok := jwt.NewWithClaims(jwt.SigningMethodPS256, baseClaims(o.issuer, clientID, nonce))
	tok.Header["kid"] = o.kid
	raw, err := tok.SignedString(o.key)
	if err != nil {
		t.Fatal(err)
	}
	o.idToken = raw

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange with PS256 token: %v", err)
	}
	if claims.Subject == "" {
		t.Error("PS256 token verified but subject empty")
	}
}

func TestVerify_ToleratesSmallClockSkew(t *testing.T) {
	// golang-jwt v5 has zero default leeway; jwtClockSkewLeeway must absorb a
	// provider clock slightly ahead of ours (nbf/iat in the near future) and an
	// exp marginally in the past.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	c := baseClaims(o.issuer, clientID, nonce)
	c["iat"] = time.Now().Add(30 * time.Second).Unix() // provider clock ahead
	c["nbf"] = time.Now().Add(30 * time.Second).Unix()
	o.idToken = o.signToken(t, c)

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err != nil {
		t.Fatalf("Exchange with 30s-ahead nbf/iat: %v", err)
	}
}

func TestVerify_StillRejectsBeyondLeeway(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	c := baseClaims(o.issuer, clientID, nonce)
	c["exp"] = time.Now().Add(-(jwtClockSkewLeeway + time.Minute)).Unix()
	o.idToken = o.signToken(t, c)

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for token expired beyond leeway")
	}
}

func TestFailedStep_Classification(t *testing.T) {
	// Verification failure (nonce mismatch) → id_token_verify.
	o := newOIDCTestServer(t)
	const clientID = "client-abc"
	o.idToken = o.signToken(t, baseClaims(o.issuer, clientID, "real-nonce"))
	p := googleLikeProvider(o, clientID)
	_, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", "wrong-nonce")
	if got := FailedStep(err); got != "id_token_verify" {
		t.Errorf("FailedStep(nonce mismatch) = %q, want id_token_verify", got)
	}
	o.close()

	// Token endpoint failure → token_exchange.
	o2 := newOIDCTestServer(t)
	defer o2.close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	p2 := googleLikeProvider(o2, clientID)
	if err := p2.ensureDiscovered(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2.TokenURL = broken.URL
	_, err = p2.Exchange(context.Background(), "c", o2.issuer+"/cb", "n")
	if got := FailedStep(err); got != "token_exchange" {
		t.Errorf("FailedStep(token 500) = %q, want token_exchange", got)
	}

	// Discovery failure → discovery.
	p3 := &Provider{Name: "x", IsOIDC: true, Issuer: "", ClientID: clientID}
	_, err = p3.Exchange(context.Background(), "c", "cb", "n")
	if got := FailedStep(err); got != "discovery" {
		t.Errorf("FailedStep(no issuer) = %q, want discovery", got)
	}

	// Unclassified / nil.
	if got := FailedStep(nil); got != "unknown" {
		t.Errorf("FailedStep(nil) = %q, want unknown", got)
	}
	if got := FailedStep(errors.New("misc")); got != "unknown" {
		t.Errorf("FailedStep(misc) = %q, want unknown", got)
	}
}
