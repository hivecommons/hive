package hub

import (
	"net/http"
	"time"

	"github.com/kubestellar/hive/pkg/delegation"
)

// Delegation-chain verification material, published for THIRD-PARTY use.
//
// This is the hub half of pkg/delegation's published-key story: the package
// defines the document, and this file is where it is filled from the hub's key
// generations and served on an unauthenticated route.
//
// WHY THE HUB SERVES IT. The signing seed is derived from a key generation's
// master secret, and the hub is the only party that holds the generation set.
// A spoke could publish its own key, but a tenant checking a chain minted
// anywhere in the fleet would then need to discover and trust N endpoints; one
// hub-served document covers the fleet and is the same artifact for every
// tenant, which is what makes it citable.
//
// OBSERVE-ONLY: nothing here reads a chain, and no hub decision consults one.
// This endpoint only publishes public keys.

// delegationSigningSeed returns the PRIVATE Ed25519 seed for the delegation
// domain under the hub's currently-minting generation.
//
// Private material. Never logged, never returned by a handler, never placed in
// a Deployment env var — the same contract hub_keys.go states for
// ssoSigningSeed and sessionSigningSeed. It exists so the hub can derive the
// matching PUBLIC key below; nothing else in this file calls it.
func (s *HubServer) delegationSigningSeed() string {
	if gs := s.currentGenerations(); gs != nil {
		return delegation.SeedFromMaster(gs.currentSecret())
	}
	return delegation.SeedFromMaster(s.hubSecret)
}

// delegationPublicKey returns the hex Ed25519 PUBLIC key for the current
// generation, with the private half expanded and discarded inside
// PublicKeyFromSeed.
func (s *HubServer) delegationPublicKey() string {
	return delegation.PublicKeyFromSeed(s.delegationSigningSeed())
}

// delegationKeyDocument assembles the published material.
//
// The previous-generation keys come from previousPublicKeys, which is already
// generic over the domain label and — critically — applies the expiry rule:
// a generation whose VerifyUntil has passed is excluded, and a MISSING
// VerifyUntil is treated as already expired so a hand-edited generations file
// fails closed. Reusing it means this endpoint's acceptance window closes on
// the same wall clock as every other verifier in the system, with no separate
// retirement path to forget.
//
// Before any rotation — the state of every hub in the fleet today —
// previousPublicKeys returns empty and the document carries exactly one key.
func (s *HubServer) delegationKeyDocument(now time.Time) delegation.KeyDocument {
	enabled := delegation.Enabled()
	currentGen := 0
	var previous []delegation.PublishedKey

	if gs := s.currentGenerations(); gs != nil {
		if g, ok := gs.currentGeneration(); ok {
			currentGen = g.ID
		}
		// Walk the acceptable set for the non-current generations, deriving
		// each one's delegation-domain public key. Same construction as
		// previousPublicKeys, but retaining the generation ID: the published
		// document is keyed by generation so a verifier can select rather than
		// trial-verify, which previousPublicKeys' flat []string cannot express.
		for _, g := range gs.acceptableGenerations(now) {
			if g.ID == gs.Current {
				continue
			}
			pub := delegation.PublicKeyFromSeed(delegation.SeedFromMaster(g.Secret))
			if pub == "" {
				// A generation whose seed does not expand yields no entry
				// rather than an empty-string one — an empty key in a published
				// document would look configured while verifying nothing.
				continue
			}
			previous = append(previous, delegation.PublishedKey{Generation: g.ID, PublicKey: pub})
		}
	}

	return delegation.BuildKeyDocument(enabled, currentGen, s.delegationPublicKey(), previous, now)
}

// handleDelegationKeys serves the verification material.
//
// UNAUTHENTICATED, deliberately, and registered without requireAuth. A tenant
// must be able to verify a chain without holding a hive credential — that is
// the entire competitive claim — and an auditor they delegate to holds no
// credential at all. The response is public keys and generation numbers, both
// of which this codebase already treats as non-secret (a generation ID names a
// key; it is not a key). There is nothing here to protect.
func (s *HubServer) handleDelegationKeys(w http.ResponseWriter, r *http.Request) {
	delegation.ServeKeys(w, s.delegationKeyDocument(time.Now()))
}
