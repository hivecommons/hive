// Package delegation implements hive's OBSERVE-ONLY delegation chain: a
// cryptographically verifiable record of WHICH AUTHORIZATIONS COMPOSED to
// produce an action, shaped after RFC 8693's `act` (actor) claim nesting.
//
// WHAT THIS PACKAGE IS FOR, AND WHY MULTI-TENANCY MAKES IT LOAD-BEARING.
//
// A hive action is almost never authorized by one party. A hosted spoke agent
// opening a PR is acting under a GitHub App installation, which is itself
// scoped by the hive the hub provisioned, which exists because an operator
// asked for it. Today that composition is reconstructible only by reading the
// audit log and knowing how the pieces fit — it is institutional knowledge, not
// evidence. The delegation chain makes it evidence: a signed, third-party
// verifiable statement of the whole delegation path.
//
// The multi-tenant property is what makes this matter more here than in a
// single-tenant runner. A tenant's hive runs inside a fleet SOMEONE ELSE
// OPERATES. "Trust the operator's dashboard" is precisely the assurance a
// tenant cannot accept, so the chain is published in a form the tenant's own
// services can verify with no hive credentials and no request-time call to the
// hub — see pkg/delegation/verify.go and src/docs/delegation-chain.md.
//
// ============================ OBSERVE ONLY ============================
//
// NOTHING IN HIVE CONSULTS A CHAIN TO DECIDE ANYTHING. This package mints,
// publishes verification material, and emits. It does not authorize, refuse,
// degrade, or alter any behavior, and no caller may make it do so. That is not
// a phase-one convenience — it is the design, and it is pinned by a
// source-level test (TestObserveOnlyInvariant_NoEnforcementConsultsChain) that
// fails if any non-test call site branches on chain validity.
//
// The reason is hive's own recent history. Three separate incidents in one week
// came from changing an auth decision:
//
//   - #3982 hardened the identity oracle and broke every staff agent's
//     author-gated listing; a fleet owner was dead in the water until #4049.
//   - #4045's token bypass existed because a scrub sat at the wrong boundary.
//   - #4043's label injection failed closed and bricked operations.
//
// Each was a correct-looking tightening of an auth path that turned out to have
// a consumer nobody had enumerated. A delegation chain touches EVERY identity
// situation at once, so shipping it in enforcing mode would be all three
// failure modes simultaneously. Observation first produces the data — via the
// comparison harness in compare.go — that tells us what enforcement would
// actually have refused, BEFORE anything refuses it.
//
// Moving to enforcement is a separate, deliberate decision documented in
// src/docs/delegation-chain.md. It is not a follow-up chore.
package delegation

import (
	"fmt"
	"sort"
	"strings"
)

// PrincipalType names the KIND of party a chain link represents.
//
// The type is explicit and mandatory because the single most dangerous thing
// this package could do is let a machine actor read as a person. An audit
// consumer that sees a bare login string cannot tell "clubanderson approved
// this" from "something ran as clubanderson's token"; a consumer that sees
// PrincipalUser vs PrincipalHiveAuthority cannot confuse them.
//
// This follows hive's established precedent for machine actors rather than
// inventing one. #4001 records a hook-initiated pause as `paused_by:
// hook:<name>` — a machine identity in the actor field — instead of attributing
// it to whoever last touched the config. #4055's PauseBy carries the rule
// verbatim: "empty means 'no human actor' — never fabricate one". A principal
// type is the same commitment made structural: there is no way to spell a
// scheduled agent's root that a reader could mistake for a human.
type PrincipalType string

const (
	// PrincipalUser is a HUMAN, identified by a forge login that a person
	// actually authenticated as. Only ever set from a server-resolved identity
	// (a verified session, a user token whose /user call succeeded) — never
	// from a client-supplied header, and never inferred from a machine
	// credential that merely happens to be associated with a person.
	PrincipalUser PrincipalType = "user"

	// PrincipalApp is a forge App acting through an INSTALLATION, e.g.
	// "kubestellar-hive[bot]". This is the honest root for hosted spoke agent
	// work and it is NOT a person.
	//
	// #4049 is the reason this type exists as its own thing. An App
	// installation token (ghs_…) has no user identity at all — `gh api user`
	// 403s for every staff agent — which is exactly why gh-wrapper.sh reads the
	// trusted bot-login file (github.BotLoginFilePath()) rather than asking the
	// token who it is. There is no human behind an installation token to
	// discover, so a chain that names one is fabricating it.
	PrincipalApp PrincipalType = "app"

	// PrincipalHiveAuthority is a HIVE ACTING ON ITS OWN DELEGATED AUTHORITY:
	// work the hive performs because it was configured to, with no human
	// initiating this particular occurrence.
	//
	// This is the type that exists so scheduled work never has to lie. A
	// cadence-triggered agent run (pkg/config/cadence.go) has a real, honest
	// authorization story — an operator delegated standing authority to the
	// hive when they configured the cadence — but it has NO human initiator for
	// THIS run, and the operator who set the cadence may have left the org
	// years ago. Naming them as the actor would be a fabricated root of exactly
	// the kind this package refuses. The hive's own standing authority is the
	// truthful answer, and it gets a distinct type so no consumer can read it
	// as a person.
	PrincipalHiveAuthority PrincipalType = "hive_authority"

	// PrincipalHub is the control plane acting on a spoke — a hub-originated
	// directive delivered on the heartbeat response. Distinct from
	// PrincipalHiveAuthority because the authority is the OPERATOR'S control
	// plane, not the tenant's hive, and a tenant auditing their own fleet needs
	// to see that difference plainly.
	PrincipalHub PrincipalType = "hub"

	// PrincipalAgent is a named agent process within a hive (scanner, reviewer,
	// feature…). Never a root — an agent always acts under something, and a
	// chain rooted at an agent would be missing the authority that started it.
	// enforced by Chain.Validate.
	PrincipalAgent PrincipalType = "agent"
)

// validPrincipalTypes is the closed set. A chain carrying anything else is
// malformed rather than "extensible": an unknown principal type is precisely
// the case where a consumer cannot tell whether it denotes a person, so it must
// fail parsing rather than be passed through to a human-readable rendering.
var validPrincipalTypes = map[PrincipalType]bool{
	PrincipalUser:          true,
	PrincipalApp:           true,
	PrincipalHiveAuthority: true,
	PrincipalHub:           true,
	PrincipalAgent:         true,
}

// IsHuman reports whether this type denotes a natural person.
//
// Exactly one type returns true, and that is the point: every rendering of a
// chain that wants to say "a person did this" must route through here rather
// than pattern-matching on an identifier string. A login like
// "kubestellar-hive[bot]" is machine, "clubanderson" is human, and no amount of
// string inspection distinguishes them reliably across forges.
func (t PrincipalType) IsHuman() bool { return t == PrincipalUser }

// Valid reports whether t is a member of the closed set.
func (t PrincipalType) Valid() bool { return validPrincipalTypes[t] }

// Principal is one party in a delegation chain.
//
// IDENTIFIERS AND GENERATIONS ONLY — NEVER KEY MATERIAL. Every field here is
// safe to publish: a login, a numeric App/installation ID, a hive ID, an agent
// name. Nothing derived from the master secret, no token, no seed, no
// signature-bearing value from another domain. The chain is signed OVER these
// fields; it never carries the material that signs it.
type Principal struct {
	// Type is mandatory. A Principal with an empty or unknown Type is invalid
	// (see Validate) rather than defaulted, because every plausible default is
	// a lie about whether a human was involved.
	Type PrincipalType `json:"type"`

	// ID is the stable identifier within Type's namespace: a forge login for
	// user/app, the hive ID for hive_authority, the agent name for agent, and
	// the hub's own identifier for hub.
	ID string `json:"id"`

	// HiveID scopes this principal to a hive where that is meaningful. It is
	// what lets a tenant confirm a chain concerns THEIR hive: a chain naming
	// another tenant's hive is evidence about someone else's fleet.
	HiveID string `json:"hive_id,omitempty"`

	// AppID and InstallationID pin a PrincipalApp to the exact App and
	// installation whose token was used. Both are non-secret metadata (they
	// appear in webhook payloads), and they are what makes "which installation
	// authorized this" answerable without the hub.
	AppID          int64 `json:"app_id,omitempty"`
	InstallationID int64 `json:"installation_id,omitempty"`

	// Via records the MECHANISM through which this principal's authority
	// reached the action, when that is not implied by Type — e.g.
	// "cadence:scanner" for a scheduled run, "heartbeat-directive" for a
	// hub-delivered command, "device-flow" for a user token.
	//
	// This is deliberately a mechanism label and NOT an actor. It mirrors
	// PausedTrigger sitting alongside PausedBy in pkg/agent: the trigger says
	// how, the principal says who, and collapsing the two is how a hook ends up
	// looking like a person.
	Via string `json:"via,omitempty"`
}

// Validate reports why p cannot appear in a chain, or nil.
func (p Principal) Validate() error {
	if !p.Type.Valid() {
		return fmt.Errorf("delegation: unknown principal type %q", p.Type)
	}
	if strings.TrimSpace(p.ID) == "" {
		// An identifier-less principal is the shape a fabricated root would
		// take if one slipped through: "something acted, we are not saying
		// what". Reject it here so it can never be minted.
		return fmt.Errorf("delegation: principal of type %q has no id", p.Type)
	}
	if p.Type == PrincipalApp && p.InstallationID == 0 {
		// An App principal without an installation cannot be checked against
		// anything: the whole assurance of "this App was authorized on this
		// account" is the installation, and an App ID alone does not carry it.
		return fmt.Errorf("delegation: app principal %q has no installation id", p.ID)
	}
	return nil
}

// CanRoot reports whether this principal type may be the ROOT of a chain — the
// party whose own authority is not derived from anything else in the chain.
//
// PrincipalAgent cannot root: an agent process has no authority of its own, it
// runs because a hive's cadence or a person started it, and a chain that
// bottoms out at an agent has dropped the link that actually authorized the
// work. Refusing to mint such a chain is better than minting a chain whose root
// is misleading — the whole no-fabricated-root rule in one check.
func (t PrincipalType) CanRoot() bool {
	return t == PrincipalUser || t == PrincipalApp || t == PrincipalHiveAuthority || t == PrincipalHub
}

// String renders a principal for logs and the comparison harness.
//
// The type prefix is ALWAYS present, including for humans. A bare login in a
// log line is the ambiguity this package exists to remove, and a format that
// drops the prefix "when it's obviously a person" reintroduces it precisely
// where the reader is most likely to be wrong.
func (p Principal) String() string {
	var b strings.Builder
	b.WriteString(string(p.Type))
	b.WriteString(":")
	b.WriteString(p.ID)
	if p.HiveID != "" {
		b.WriteString("@")
		b.WriteString(p.HiveID)
	}
	if p.Via != "" {
		b.WriteString("(")
		b.WriteString(p.Via)
		b.WriteString(")")
	}
	return b.String()
}

// SortPrincipalTypes returns the closed set in a stable order, for docs
// generation and test table completeness checks.
func SortPrincipalTypes() []PrincipalType {
	out := make([]PrincipalType, 0, len(validPrincipalTypes))
	for t := range validPrincipalTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
