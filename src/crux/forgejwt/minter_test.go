package forgejwt

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/pushbroker"
)

func testMinter(t *testing.T, auds map[string]Audience) Minter {
	t.Helper()
	return Minter{
		Signer:    Signer{Issuer: testIssuer, KeyFile: writeFile(t, pemBytes(t, testKeys()[0]), 0o600)},
		Audiences: auds,
	}
}

func mintedClaims(t *testing.T, m pushbroker.TokenMinter) map[string]any {
	t.Helper()
	tok, err := m.MintPushToken(context.Background(), "team/app")
	if err != nil {
		t.Fatalf("MintPushToken: %v", err)
	}
	_, jwks, err := KeyList(testIssuer, &testKeys()[0].PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, tok, jwks); err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	// The token travels in a git credential URL; it must not need escaping.
	if _, err := pushbroker.CredentialStoreLine("https://forgejo.example.com", tok); err != nil {
		t.Fatalf("token unusable as a git credential: %v", err)
	}
	var c map[string]any
	decodeSegment(t, strings.Split(tok, ".")[1], &c)
	return c
}

// Each tier gets the token for its own integration, with sub naming the tier
// so the integration's claim rule pins it.
func TestMinterMintsForTheAgentTier(t *testing.T) {
	m := testMinter(t, map[string]Audience{"advisor": "aud-advisor", "contributor": "aud-contributor"})
	for tier, want := range map[string][2]string{
		"advisor":     {"aud-advisor", "hive:advisor"},
		"contributor": {"aud-contributor", "hive:contributor"},
		"":            {"aud-contributor", "hive:contributor"},
	} {
		c := mintedClaims(t, m.WithTier(tier))
		if c["aud"] != want[0] || c["sub"] != want[1] || c["iss"] != string(testIssuer) {
			t.Fatalf("tier %q claims = %v, want aud %q sub %q", tier, c, want[0], want[1])
		}
	}
}

// A tier with no integration falls to a less-privileged one, never up.
func TestMinterFallsOnlyToALessPrivilegedTier(t *testing.T) {
	m := testMinter(t, map[string]Audience{"advisor": "aud-advisor", "contributor": "aud-contributor"})
	if c := mintedClaims(t, m.WithTier("trusted")); c["sub"] != "hive:contributor" {
		t.Fatalf("trusted fell to %v, want contributor", c["sub"])
	}
	if c := mintedClaims(t, m.WithTier("reviewer")); c["sub"] != "hive:advisor" {
		t.Fatalf("reviewer fell to %v, want advisor", c["sub"])
	}
	onlyTrusted := testMinter(t, map[string]Audience{"trusted": "aud-trusted"})
	for _, tier := range []string{"contributor", "advisor", "newcomer", "reviewer", "not-a-tier"} {
		if _, err := onlyTrusted.WithTier(tier).MintPushToken(context.Background(), "team/app"); err == nil {
			t.Fatalf("tier %q got a token from a more-privileged integration", tier)
		}
	}
}

func TestMinterRefusesWithoutAudiences(t *testing.T) {
	_, err := testMinter(t, nil).MintPushToken(context.Background(), "team/app")
	if err == nil || !strings.Contains(err.Error(), "no integration audiences") {
		t.Fatalf("err = %v", err)
	}
	_, err = testMinter(t, map[string]Audience{"contributor": "  "}).MintPushToken(context.Background(), "team/app")
	if err == nil {
		t.Fatal("blank audience accepted")
	}
}

// A signer error (here a key file others can read) surfaces, with no token.
func TestMinterSurfacesSignerErrors(t *testing.T) {
	m := Minter{
		Signer:    Signer{Issuer: testIssuer, KeyFile: writeFile(t, pemBytes(t, testKeys()[0]), 0o644)},
		Audiences: map[string]Audience{"contributor": "aud"},
	}
	if tok, err := m.MintPushToken(context.Background(), "team/app"); err == nil || tok != "" {
		t.Fatalf("token %q, err %v; want the key-mode refusal", tok, err)
	}
}

func TestMinterWorksThroughPushbrokerWithTier(t *testing.T) {
	m := testMinter(t, map[string]Audience{"advisor": "aud-advisor"})
	if c := mintedClaims(t, pushbroker.WithTier(m, "advisor")); c["sub"] != "hive:advisor" {
		t.Fatalf("pushbroker.WithTier did not narrow the JWT minter: %v", c)
	}
}
