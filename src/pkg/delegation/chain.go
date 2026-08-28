package delegation

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ChainVersion namespaces the signed payload so the format can evolve without a
// verifier ever accepting an old or foreign shape.
//
// Same discipline as ssoTokenVersion in pkg/hub/sso.go, and for the same
// reason: a version check inside the signature means a v1 token can never be
// replayed against a v2 verifier, so a format change is a clean break rather
// than a compatibility negotiation. Third parties verify these tokens, so the
// version is also the contract we are promising them — see
// src/docs/delegation-chain.md.
const ChainVersion = "hive-delegation-v1"

// maxChainDepth bounds how deep the `act` nesting may go.
//
// Every real hive situation is at most four links (see situations.go: the
// deepest is hub → hive_authority → app → agent). The bound exists because a
// chain arrives as attacker-influenceable JSON on the verification path: an
// unbounded nesting depth is an allocation and stack amplifier for a verifier
// that, by design, runs on a THIRD PARTY'S infrastructure with no rate limit we
// control. Rejecting at parse is cheaper than any downstream defense, and a
// legitimate chain has never come close to this.
//
// Set to 8 rather than 4 so that adding a situation does not require a format
// version bump, while still being far below any resource concern.
const maxChainDepth = 8

// ErrNoHonestRoot is returned by chain construction when the caller's inputs do
// not support an honest root.
//
// THIS ERROR IS A SUCCESS CONDITION, NOT A FAILURE. It is the mechanism by
// which this package declines to fabricate. A caller that receives it must emit
// NO CHAIN — not a chain with a placeholder root, not a chain rooted at
// "system" or "unknown", not the last human who happened to touch the config.
// Emitting nothing is a truthful statement ("we cannot prove who authorized
// this"); emitting a plausible-looking chain is a false one, and a false chain
// is strictly worse than no chain because the entire value proposition here is
// that a chain is EVIDENCE.
//
// pkg/dashboard's audit sites already model the honest version of this: the
// pseudo-users "", "system", "local", "unknown" are recorded as what they are
// rather than resolved into a person. A chain has no equivalent pseudo-root,
// because a signed pseudo-root would carry cryptographic weight that a bare
// audit string does not.
var ErrNoHonestRoot = errors.New("delegation: no honest root available; emit no chain")

// Chain is a delegation chain: a subject plus the nested actors that authorized
// it, shaped after RFC 8693's `act` claim.
//
// SHAPE. RFC 8693 nests actors so that the OUTERMOST actor is the most
// immediate one and each `act` is the party that delegated TO it. Hive follows
// that nesting exactly, so a reader who knows RFC 8693 reads a hive chain
// correctly with no hive-specific knowledge:
//
//	{
//	  "sub": {agent:scanner@acme},          // the party the action ran as
//	  "act": {app:kubestellar-hive[bot]},   // which acted under…
//	  "act": {hive_authority:acme}          // …which acted under
//	}
//
// The ROOT is the innermost `act` (or `sub` itself for a one-link chain). It is
// the party whose authority is not derived from anything else present, and it
// is the link that must be honest above all others — see ErrNoHonestRoot.
//
// SIGNATURE ALGORITHM. Ed25519, not the ES256 an RFC 8693 reader might expect.
// This is deliberate: hive already derives, provisions, rotates, and
// dual-accepts Ed25519 keys across ~65 spokes (pkg/hub/hub_keys.go,
// hub_pubkey_generations.go), and the generations machinery that makes rotation
// survivable is written against them. Introducing a second algorithm would mean
// a second rotation story, a second set of provisioning vars, and a second
// thing to get wrong — for a property (curve choice) that no consumer of this
// chain depends on. The `act` NESTING is the interoperable part and it is
// preserved verbatim; the signature is hive's existing primitive.
type Chain struct {
	// Version is checked before any other field is trusted.
	Version string `json:"v"`

	// Subject is the party the action ran AS — the outermost identity, the one
	// that would appear in a forge's "author" field.
	Subject Principal `json:"sub"`

	// Actors are the delegating parties, ordered OUTERMOST-FIRST: Actors[0]
	// delegated to Subject, Actors[1] delegated to Actors[0], and the last
	// entry is the root.
	//
	// A flat ordered slice rather than a recursive struct, because the nesting
	// is the only thing a recursive type would buy and a slice makes "walk to
	// the root" a loop rather than a recursion on attacker-supplied depth. The
	// JSON rendering (see MarshalNested) restores the RFC 8693 nested form for
	// external consumers; the internal representation stays flat.
	Actors []Principal `json:"act,omitempty"`

	// Action names what was done, e.g. "pr_opened", "agent_paused",
	// "config_saved". Deliberately aligned with the audit log's `action` field
	// (src/docs/audit-log.md) so the comparison harness in compare.go can join
	// the two without a translation table — a translation table is where a
	// mismatch would hide.
	Action string `json:"action,omitempty"`

	// HiveID is the hive this chain concerns. Present at the top level as well
	// as on principals so a tenant can filter to their own hive without walking
	// the chain.
	HiveID string `json:"hive_id,omitempty"`

	// Generation is the key generation that signed this chain. Non-secret — the
	// codebase is explicit that a generation ID names a key and is not a key
	// (pkg/hub/hub_generations.go). It lets a verifier select the right
	// published key directly instead of trial-verifying, and it is INSIDE the
	// signature so it cannot be steered by an attacker (the same reasoning that
	// put `g` inside hubCookieClaims rather than in a bare prefix).
	Generation int `json:"g,omitempty"`

	// IssuedAt and Expiry bound the chain's validity window.
	IssuedAt int64 `json:"iat"`
	Expiry   int64 `json:"exp"`
}

// Root returns the party whose authority is not derived from anything else in
// the chain, and whether the chain has one at all.
//
// A chain with no honest root should never have been minted (Mint returns
// ErrNoHonestRoot instead), so a false return here on a PARSED chain means the
// chain is malformed — treat it as unusable evidence rather than as a chain
// whose root is merely unstated.
func (c Chain) Root() (Principal, bool) {
	if len(c.Actors) > 0 {
		return c.Actors[len(c.Actors)-1], true
	}
	if c.Subject.Type == "" {
		return Principal{}, false
	}
	return c.Subject, true
}

// HasHumanRoot reports whether a natural person authorized this chain.
//
// This is the question most consumers actually want, and it is offered as a
// method precisely so no consumer has to reimplement it by inspecting
// identifier strings. Note that it asks about the ROOT specifically: a chain
// can contain a human anywhere and still not be human-authorized at the root,
// and vice versa is impossible by construction.
func (c Chain) HasHumanRoot() bool {
	root, ok := c.Root()
	return ok && root.Type.IsHuman()
}

// Depth is the number of links, subject included.
func (c Chain) Depth() int { return 1 + len(c.Actors) }

// Validate reports why the chain is structurally unusable, or nil.
//
// Called on both the mint and the verify path. On mint it stops a malformed
// chain being signed (a signature over nonsense is worse than no signature,
// because it makes the nonsense verifiable). On verify it runs AFTER the
// signature check, so it never spends work on unauthenticated bytes.
func (c Chain) Validate() error {
	if c.Version != ChainVersion {
		return fmt.Errorf("delegation: unexpected chain version %q", c.Version)
	}
	if c.Depth() > maxChainDepth {
		return fmt.Errorf("delegation: chain depth %d exceeds maximum %d", c.Depth(), maxChainDepth)
	}
	if err := c.Subject.Validate(); err != nil {
		return fmt.Errorf("delegation: subject: %w", err)
	}
	for i, a := range c.Actors {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("delegation: actor %d: %w", i, err)
		}
	}
	root, ok := c.Root()
	if !ok {
		return ErrNoHonestRoot
	}
	if !root.Type.CanRoot() {
		// An agent-rooted chain is the concrete shape "we lost the authority
		// that started this" takes. See PrincipalType.CanRoot.
		return fmt.Errorf("delegation: principal type %q cannot be a chain root", root.Type)
	}
	if c.IssuedAt <= 0 || c.Expiry <= 0 {
		return fmt.Errorf("delegation: chain has no validity window")
	}
	if c.Expiry <= c.IssuedAt {
		return fmt.Errorf("delegation: chain expiry precedes issuance")
	}
	return nil
}

// Describe renders the chain root-first as a human-readable arrow path, e.g.
//
//	hive_authority:acme(cadence:scanner) -> app:kubestellar-hive[bot] -> agent:scanner
//
// Root-first because that is the order a person reasons about authorization in
// ("who allowed this, and how did it get here"), even though the wire format is
// subject-first to match RFC 8693. Used by the emit path and the comparison
// harness; it carries only identifiers, so it is safe for logs.
func (c Chain) Describe() string {
	parts := make([]string, 0, c.Depth())
	for i := len(c.Actors) - 1; i >= 0; i-- {
		parts = append(parts, c.Actors[i].String())
	}
	parts = append(parts, c.Subject.String())
	return strings.Join(parts, " -> ")
}

// Expired reports whether the chain is outside its validity window at `now`,
// applying the same clock skew tolerance the SSO path uses.
func (c Chain) Expired(now time.Time) bool {
	skew := int64(ChainClockSkew / time.Second)
	n := now.Unix()
	return c.IssuedAt > n+skew || c.Expiry < n-skew
}

// nestedChain is the RFC 8693-shaped EXTERNAL rendering: `act` is a nested
// object rather than hive's internal flat slice.
//
// Two representations exist on purpose. Internally a slice is safer (bounded
// iteration, no recursion on parsed input). Externally the nested form is what
// an RFC 8693 reader — including the third-party verifier we are inviting — will
// expect, and the whole competitive claim is that a tenant can verify this with
// generic tooling. The conversion is total and tested in both directions.
type nestedChain struct {
	Principal
	Act *nestedChain `json:"act,omitempty"`
}

// Nested returns the RFC 8693-shaped actor nesting for external rendering.
// Returns nil when there are no delegating actors.
func (c Chain) Nested() *nestedChain {
	if len(c.Actors) == 0 {
		return nil
	}
	// Build from the ROOT outward so each node's `act` is already constructed.
	var node *nestedChain
	for i := len(c.Actors) - 1; i >= 0; i-- {
		node = &nestedChain{Principal: c.Actors[i], Act: node}
	}
	return node
}
