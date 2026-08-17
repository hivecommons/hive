// Peer-protocol compatibility detection for the contributor WebSocket protocol.
//
// This closes the one kubestellar/hive#2547 acceptance criterion that DECLARE
// (#2659/#2600) left open:
//
//	"Either the protocol gains a way for both sides to detect an incompatible
//	 peer, or it is documented that compatibility is managed out of band and how."
//
// #2567 gave both sides a version to STATE — the hub advertises
// contributorProtocolVersion on auth_ok, and a relay may declare
// capabilities.relay_protocol_version in auth_response. Neither side ever
// COMPARED them, so the issue's original complaint survived the fix: the only
// way to learn that an old relay is talking to a new hub was still to watch it
// misbehave. The declared version was rendered on the ops row as a bare
// "proto 1.1" chip with nothing to compare it against.
//
// That is not hypothetical. #2600 shipped both sides at "1.1"; #2671 bumped the
// hub to "1.2" (adding capCredentialAfterAccept) and left bin/contributor-relay.sh
// at "1.1", against an explicit "Keep in step with contributorProtocolVersion"
// comment that nothing enforced. The in-tree relay has been under-declaring its
// own protocol level ever since, and no surface said so.
//
// What this file adds, and the line it holds:
//
//   - A comparison, and a legible operator-facing verdict for it. Nothing else.
//
//   - It NEVER gates. A mismatched — even a majorly incompatible — peer
//     authenticates, is admitted, and receives exactly the work it receives
//     today. #2547 is emphatic that compatibility must be carried by the
//     DEFAULTS because there is no negotiation to carry it, and that an existing
//     relay must never lose assignments because it was written before a change.
//     A version is self-reported client text; rejecting on it would be routing
//     on a value the client controls, which is the failure mode the issue names.
//     TestProtocolCompat_NeverGates and TestProtocolCompatIsNotReadBySelection
//     pin this, mirroring the #3815 DECLARE/ROUTE source-level guard.
//
//   - The verdict is DERIVED, not stored: FleetSnapshot computes it from the
//     capability the client already declared. No new connection state, and no
//     new field on the wire in either direction.
package dashboard

import (
	"strconv"
	"strings"
)

// Peer-compatibility verdicts. Stable identifiers (an operator UI maps them to
// prose); add new ones rather than repurposing these.
const (
	// protoPeerUnknown: the peer declared no version at all — an unversioned
	// relay predating #2567. Compatibility is UNVERIFIABLE, not bad: this is the
	// backward-compatible default and every such client keeps working.
	protoPeerUnknown = "unknown"
	// protoPeerCurrent: peer and hub speak the same MAJOR.MINOR.
	protoPeerCurrent = "current"
	// protoPeerOlder: same major, lower minor. Purely additive drift — the peer
	// simply does not know about features added since its minor.
	protoPeerOlder = "older"
	// protoPeerNewer: same major, higher minor. The peer knows about features
	// this hub has not deployed; it should degrade using server_capabilities.
	protoPeerNewer = "newer"
	// protoPeerIncompatible: MAJOR mismatch. By the versioning rule in
	// contribute_protocol.go a major bump is a breaking wire change, so peer
	// behaviour is undefined. Reported LOUDLY, still not enforced.
	protoPeerIncompatible = "incompatible"
	// protoPeerMalformed: the peer declared something that is not MAJOR.MINOR.
	// Treated as unverifiable, exactly like unknown, never as a rejection.
	protoPeerMalformed = "malformed"
)

// maxDeclaredVersionRunes bounds the client-declared version string before it is
// echoed into an operator-visible payload. relay_protocol_version is
// client-controlled free text and nothing obliges a relay to keep it short.
const maxDeclaredVersionRunes = 32

// parseProtocolVersion parses a "MAJOR.MINOR" contributor-protocol version.
// Both components must be non-negative integers and nothing may follow the
// minor — "1.2.3", "v1.2", "1", and "" are all rejected, so an unrecognised
// shape lands on protoPeerMalformed rather than being silently coerced into a
// comparison that would report a confident and wrong verdict.
func parseProtocolVersion(v string) (major, minor int, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
}

// classifyPeerProtocol compares a peer's declared contributor-protocol version
// against the version this hub speaks and returns one of the protoPeer*
// verdicts. An empty declaration is protoPeerUnknown — NEVER an error and never
// a rejection: that is what every relay written before #2567 sends.
func classifyPeerProtocol(peer string) string {
	if strings.TrimSpace(peer) == "" {
		return protoPeerUnknown
	}
	peerMajor, peerMinor, ok := parseProtocolVersion(peer)
	if !ok {
		return protoPeerMalformed
	}
	// The hub's own constant is compile-time ours; if it ever stops parsing we
	// cannot make a meaningful comparison, so we report unknown rather than
	// accusing the client of a mismatch we caused.
	hubMajor, hubMinor, ok := parseProtocolVersion(contributorProtocolVersion)
	if !ok {
		return protoPeerUnknown
	}
	switch {
	case peerMajor != hubMajor:
		return protoPeerIncompatible
	case peerMinor < hubMinor:
		return protoPeerOlder
	case peerMinor > hubMinor:
		return protoPeerNewer
	default:
		return protoPeerCurrent
	}
}

// protocolVerdictDetail is the operator-facing sentence for a verdict.
//
// It deliberately interpolates NO client-supplied text. The hub's own version is
// a server constant and safe to name; the peer's is client-controlled and is
// carried separately in ProtocolCompat.Peer (bounded, and escaped at render).
// Keeping the two apart means a hostile version string cannot dress itself up as
// hub prose on an operator's screen.
func protocolVerdictDetail(verdict string) string {
	switch verdict {
	case protoPeerCurrent:
		return "Client speaks the same protocol version as this hub."
	case protoPeerOlder:
		return "Client speaks an older protocol minor than this hub (" + contributorProtocolVersion +
			"). Additive-only: features added since the client's version are unavailable to it, but nothing breaks."
	case protoPeerNewer:
		return "Client speaks a newer protocol minor than this hub (" + contributorProtocolVersion +
			"). It should degrade using the capability set advertised on auth_ok."
	case protoPeerIncompatible:
		return "Client declares a different protocol MAJOR than this hub (" + contributorProtocolVersion +
			"). A major bump is a breaking wire change, so behaviour is undefined — but the client is still served."
	case protoPeerMalformed:
		return "Client declared a protocol version this hub cannot parse (expected MAJOR.MINOR). Treated as undeclared."
	default:
		return "Client declared no protocol version (a relay predating the versioned handshake). Compatibility is unverifiable and assumed good."
	}
}

// ProtocolCompat is the read-only comparison between the contributor-protocol
// version this hub speaks and the one a connected client declared. It is DERIVED
// per snapshot from the client's existing declaration — it stores nothing new
// and adds nothing to the wire in either direction.
//
// It is observability, not enforcement: no field here is consulted when
// admitting a client or selecting work for one. Every verdict, including
// protoPeerIncompatible, describes a client that is fully served.
type ProtocolCompat struct {
	// Hub is the contributor-protocol version this hub speaks. Always set — the
	// operator complaint this answers is that a bare "proto 1.1" on a fleet row
	// had nothing to compare against.
	Hub string `json:"hub"`
	// Peer is the version the CLIENT declared, verbatim but length-bounded.
	// Empty for an unversioned relay. Client-controlled: escape at render.
	Peer string `json:"peer,omitempty"`
	// Verdict is one of the protoPeer* constants.
	Verdict string `json:"verdict"`
	// Detail is the operator-facing explanation. Server-authored; never contains
	// client text.
	Detail string `json:"detail,omitempty"`
	// Mismatch is true when the operator has something to look at — the peer
	// declared a version and it is not this hub's. It is false for both
	// protoPeerCurrent and protoPeerUnknown, so a healthy fleet and a fleet of
	// pre-#2567 relays are equally quiet. A UI can key its warning off this one
	// boolean rather than re-deriving the verdict set.
	Mismatch bool `json:"mismatch,omitempty"`
}

// peerProtocolCompat builds the comparison for a client-declared version.
// peer may be empty (unversioned relay) — that is a supported, non-exceptional
// input and yields a quiet protoPeerUnknown.
func peerProtocolCompat(peer string) ProtocolCompat {
	peer = strings.TrimSpace(peer)
	if r := []rune(peer); len(r) > maxDeclaredVersionRunes {
		peer = string(r[:maxDeclaredVersionRunes])
	}
	verdict := classifyPeerProtocol(peer)
	return ProtocolCompat{
		Hub:      contributorProtocolVersion,
		Peer:     peer,
		Verdict:  verdict,
		Detail:   protocolVerdictDetail(verdict),
		Mismatch: verdict == protoPeerOlder || verdict == protoPeerNewer || verdict == protoPeerIncompatible || verdict == protoPeerMalformed,
	}
}
