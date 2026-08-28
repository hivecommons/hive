package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/delegation"
)

const delegationTestMaster = "test-master-for-delegation-keys"

// TestDeriveDomainKeyMatchesHubDerivation is the anti-drift pin for
// pkg/delegation's local copy of deriveDomainKey.
//
// The copy exists because pkg/delegation must stay near-leaf (pkg/dashboard and
// pkg/agent import it, and pkg/hub imports most of the tree). The risk that
// creates is silent divergence: if the two implementations ever disagreed, the
// hub would publish a public key that verifies nothing, and the only symptom
// would be "every chain is unverifiable" with no error near the cause.
//
// This test makes that divergence impossible to merge.
func TestDeriveDomainKeyMatchesHubDerivation(t *testing.T) {
	for _, master := range []string{delegationTestMaster, "another-master", "x"} {
		hubDerived := deriveDomainKey(master, delegation.InfoChainEd25519Seed)
		pkgDerived := delegation.SeedFromMaster(master)
		if hubDerived != pkgDerived {
			t.Fatalf("derivation drift for master %q:\n hub: %s\n pkg: %s", master, hubDerived, pkgDerived)
		}
	}
	// The empty-master fail-closed contract must match too: no secret means no
	// key, which means no chain is emitted.
	if deriveDomainKey("", delegation.InfoChainEd25519Seed) != delegation.SeedFromMaster("") {
		t.Error("empty-master behaviour diverges between hub and pkg/delegation")
	}
	if delegation.SeedFromMaster("") != "" {
		t.Error("an empty master derived a usable key")
	}
}

// TestDelegationDomainIsSeparatedFromExistingDomains pins that a delegation
// chain signature can never verify as an SSO handoff or a session cookie, and
// vice versa.
//
// Domain separation is the entire reason hive's info labels exist
// (hub_keys.go), and a new domain that collided with an existing one would let
// an artifact from one lane be replayed into another. The labels differ, so the
// keys must differ — this asserts it rather than assuming it.
func TestDelegationDomainIsSeparatedFromExistingDomains(t *testing.T) {
	master := delegationTestMaster
	delegationSeed := delegation.SeedFromMaster(master)

	others := map[string]string{
		"sso-ed25519":     deriveDomainKey(master, infoSSOEd25519Seed),
		"session-ed25519": deriveDomainKey(master, infoSessionEd25519Seed),
		"heartbeat":       deriveDomainKey(master, infoHeartbeatKey),
		"session":         deriveDomainKey(master, infoSessionKey),
		"sso":             deriveDomainKey(master, infoSSOKey),
		"impersonate":     deriveDomainKey(master, infoImpersonateKey),
		"invite":          deriveDomainKey(master, infoInviteKey),
	}
	for name, key := range others {
		if key == delegationSeed {
			t.Errorf("delegation seed collides with the %s domain key", name)
		}
	}
	// And the derived PUBLIC keys differ, which is what a verifier actually
	// compares against.
	if delegation.PublicKeyFromSeed(delegationSeed) == ssoPublicKeyFromSeed(others["sso-ed25519"]) {
		t.Error("delegation public key collides with the SSO public key")
	}
}

// TestDelegationKeyEndpointServesOnlyPublicMaterial is the secrets-discipline
// assertion for the published endpoint.
//
// Correctness alone cannot catch a leak here: a document that also contained
// the private seed would still verify chains perfectly. So this asserts the
// absence directly, against the response bytes a third party actually receives.
func TestDelegationKeyEndpointServesOnlyPublicMaterial(t *testing.T) {
	t.Setenv(delegation.EnvChainsEnabled, "1")

	s := &HubServer{hubSecret: delegationTestMaster}
	rec := httptest.NewRecorder()
	s.handleDelegationKeys(rec, httptest.NewRequest(http.MethodGet, delegation.KeysPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The PRIVATE seed must not appear, in hex or raw.
	seed := delegation.SeedFromMaster(delegationTestMaster)
	if seed == "" {
		t.Fatal("test master derived no seed")
	}
	if strings.Contains(body, seed) {
		t.Fatal("the published document contains the PRIVATE signing seed")
	}
	// Nor the master itself, nor any other domain's key.
	if strings.Contains(body, delegationTestMaster) {
		t.Fatal("the published document contains the master secret")
	}
	for _, k := range []string{
		deriveDomainKey(delegationTestMaster, infoHeartbeatKey),
		deriveDomainKey(delegationTestMaster, infoSessionKey),
		deriveDomainKey(delegationTestMaster, infoSSOEd25519Seed),
	} {
		if k != "" && strings.Contains(body, k) {
			t.Fatal("the published document contains another domain's key material")
		}
	}

	// The PUBLIC key must be present and usable.
	var doc delegation.KeyDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parsing published document: %v", err)
	}
	if !doc.Enabled {
		t.Error("document reports disabled with the flag on")
	}
	if len(doc.Keys) == 0 {
		t.Fatal("document carries no keys")
	}
	wantPub := delegation.PublicKeyFromSeed(seed)
	if doc.Keys[0].PublicKey != wantPub {
		t.Errorf("published key = %q, want %q", doc.Keys[0].PublicKey, wantPub)
	}
	if !doc.Keys[0].Current {
		t.Error("the only published key is not marked current")
	}
	if doc.Keys[0].Algorithm != delegation.KeyAlgorithm || doc.Keys[0].Curve != delegation.KeyCurve {
		t.Errorf("published algorithm/curve = %q/%q", doc.Keys[0].Algorithm, doc.Keys[0].Curve)
	}

	// End to end: a chain minted by this hub's key verifies against the served
	// document, with nothing but the response body.
	c, err := delegation.ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	now := time.Now()
	token := delegation.MintToken(s.delegationSigningSeed(), c, now)
	if token == "" {
		t.Fatal("hub minted nothing")
	}
	if _, _, err := delegation.VerifyTokenAcrossKeys(doc.PublicKeysFrom(), token, now); err != nil {
		t.Fatalf("a chain minted by this hub failed to verify against its own published material: %v", err)
	}
}

// TestDelegationKeyEndpointIsAnonymous pins that the endpoint requires no
// credential. This is the competitive property: a tenant, or an auditor they
// delegate to, must be able to verify without the operator vouching for them.
// A future PR that wrapped this route in requireAuth would defeat the feature
// while protecting nothing, so the absence of auth is asserted rather than left
// to reviewer memory.
func TestDelegationKeyEndpointIsAnonymous(t *testing.T) {
	t.Setenv(delegation.EnvChainsEnabled, "1")
	s := &HubServer{hubSecret: delegationTestMaster}

	// No cookie, no bearer, no headers of any kind.
	req := httptest.NewRequest(http.MethodGet, delegation.KeysPath, nil)
	rec := httptest.NewRecorder()
	s.handleDelegationKeys(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("an anonymous request got %d; the endpoint must be publicly fetchable", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// Cross-origin readable, so browser-based tenant tooling can verify.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestDelegationKeyEndpointFlagOffPublishesNoKeys pins the flag's OFF state at
// the HTTP boundary — the surface a third party actually observes.
func TestDelegationKeyEndpointFlagOffPublishesNoKeys(t *testing.T) {
	t.Setenv(delegation.EnvChainsEnabled, "")
	s := &HubServer{hubSecret: delegationTestMaster}

	rec := httptest.NewRecorder()
	s.handleDelegationKeys(rec, httptest.NewRequest(http.MethodGet, delegation.KeysPath, nil))

	var doc delegation.KeyDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if doc.Enabled {
		t.Error("document reports enabled with the flag off")
	}
	if len(doc.Keys) != 0 {
		t.Errorf("document published %d keys with the flag off; want 0", len(doc.Keys))
	}
	// The public key must not appear anywhere in the body.
	pub := delegation.PublicKeyFromSeed(delegation.SeedFromMaster(delegationTestMaster))
	if pub != "" && strings.Contains(rec.Body.String(), pub) {
		t.Error("a public key was published with the flag off")
	}
}

// TestDelegationKeyDocumentRotationWindow pins that the published set follows
// the SAME acceptance window as every other verifier in the system — a
// generation whose VerifyUntil has passed leaves the document, and a MISSING
// VerifyUntil is treated as already expired so a hand-edited generations file
// fails closed.
//
// This is what makes rotation self-clearing rather than a permanent compat lane.
func TestDelegationKeyDocumentRotationWindow(t *testing.T) {
	t.Setenv(delegation.EnvChainsEnabled, "1")
	now := time.Now()

	gs := newGenerationSet(2, []keyGeneration{
		{ID: 2, Secret: "gen-two-secret", Created: now},
		{ID: 1, Secret: "gen-one-secret", Created: now.Add(-time.Hour), VerifyUntil: now.Add(time.Hour)},
	})
	s := &HubServer{hubSecret: "gen-two-secret", keyGenerations: gs}

	doc := s.delegationKeyDocument(now)
	if len(doc.Keys) != 2 {
		t.Fatalf("during the acceptance window the document should carry 2 keys, got %d", len(doc.Keys))
	}

	// A chain minted under generation 1 verifies during the window.
	c, err := delegation.ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	c.Generation = 1
	token := delegation.MintToken(delegation.SeedFromMaster("gen-one-secret"), c, now)
	if token == "" {
		t.Fatal("minted nothing under generation 1")
	}
	if _, _, err := delegation.VerifyTokenAcrossKeys(doc.PublicKeysFrom(), token, now); err != nil {
		t.Fatalf("a generation-1 chain failed during its acceptance window: %v", err)
	}

	// After the window closes, generation 1 leaves the document.
	afterWindow := now.Add(2 * time.Hour)
	closed := s.delegationKeyDocument(afterWindow)
	if len(closed.Keys) != 1 {
		t.Errorf("after the window the document should carry 1 key, got %d", len(closed.Keys))
	}
	for _, k := range closed.Keys {
		if k.Generation == 1 {
			t.Error("generation 1 remained published after its acceptance window closed")
		}
	}
}
