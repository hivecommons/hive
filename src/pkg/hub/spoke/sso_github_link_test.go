package spoke

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// SSO tokens carry a proven GitHub login link (#19992) under the signature,
// and omit it entirely when there is none.

const linkTestMaster = "link-test-master-secret"

func TestSSOToken_CarriesGitHubLinkUnderSignature(t *testing.T) {
	seed := SSOSigningSeedFromMaster(linkTestMaster)
	pub := ssoPublicKeyFromSeed(seed)
	now := time.Now()
	linkedAt := now.Add(-time.Hour).Truncate(time.Second)
	tok := MintSSOTokenWithGitHubLink(seed, "custom:alice", "owner", "hive-1", SSOGitHubLink{Login: "alice-gh", LinkedAt: linkedAt}, now)
	id, err := VerifySSOTokenIdentity(pub, tok, "hive-1", now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Username != "custom:alice" || id.Role != "owner" || id.GitHubLink.Login != "alice-gh" || !id.GitHubLink.LinkedAt.Equal(linkedAt) {
		t.Fatalf("identity = %+v", id)
	}
	// Across keys returns the same identity.
	id2, idx, err := VerifySSOIdentityAcrossKeys([]string{pub}, tok, "hive-1", now)
	if err != nil || idx != 0 || id2.GitHubLink.Login != "alice-gh" {
		t.Fatalf("across keys = %+v %d %v", id2, idx, err)
	}
	// A tampered login breaks the signature.
	body, sig, _ := strings.Cut(tok, ".")
	raw, _ := ssoB64.DecodeString(body)
	var claims map[string]any
	_ = json.Unmarshal(raw, &claims)
	claims["g"] = "mallory-gh"
	forged, _ := json.Marshal(claims)
	if _, err := VerifySSOTokenIdentity(pub, ssoB64.EncodeToString(forged)+"."+sig, "hive-1", now); err == nil {
		t.Fatal("a token with a swapped GitHub login must not verify")
	}
}

func TestSSOToken_NoLinkIsOmittedAndOldVerifierStillWorks(t *testing.T) {
	seed := SSOSigningSeedFromMaster(linkTestMaster)
	pub := ssoPublicKeyFromSeed(seed)
	now := time.Now()
	for _, link := range []SSOGitHubLink{{}, {Login: "alice-gh"}, {LinkedAt: now}} {
		tok := MintSSOTokenWithGitHubLink(seed, "custom:alice", "read", "hive-1", link, now)
		body, _, _ := strings.Cut(tok, ".")
		raw, _ := ssoB64.DecodeString(body)
		if strings.Contains(string(raw), `"g"`) || strings.Contains(string(raw), `"ga"`) {
			t.Fatalf("incomplete link %+v must be omitted, payload %s", link, raw)
		}
		id, err := VerifySSOTokenIdentity(pub, tok, "hive-1", now)
		if err != nil || id.GitHubLink.Login != "" {
			t.Fatalf("identity = %+v err %v", id, err)
		}
	}
	// The plain verifier still reads a linked token's username and role.
	tok := MintSSOTokenWithGitHubLink(seed, "custom:alice", "read", "hive-1", SSOGitHubLink{Login: "alice-gh", LinkedAt: now}, now)
	u, r, err := VerifySSOToken(pub, tok, "hive-1", now)
	if err != nil || u != "custom:alice" || r != "read" {
		t.Fatalf("VerifySSOToken = %q %q %v", u, r, err)
	}
	if _, _, err := VerifySSOIdentityAcrossKeys(nil, tok, "hive-1", now); err == nil {
		t.Fatal("no keys must fail")
	}
	if _, _, err := VerifySSOIdentityAcrossKeys([]string{pub}, tok, "other-hive", now); err == nil {
		t.Fatal("wrong hive must fail")
	}
}
