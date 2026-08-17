// Contributor-protocol versioning, capability advertisement, and the
// surface/schema identifier — the ADDITIVE, backward-compatible contributor
// handshake extensions from kubestellar/hive#2547 (capability DECLARE half) and
// kubestellar/hive#2567 (protocol version + server capability set + surface
// identifier).
//
// Design contract — forward/backward compatibility (#2567):
//
//   - Every field added by these two issues is OPTIONAL and additive. A client
//     that omits the new auth_response capability fields authenticates and runs
//     EXACTLY as before — nothing here gates admission, model acceptance, or
//     task selection. A client that ignores the new auth_ok fields (an existing
//     unversioned relay) behaves exactly as today.
//
//   - UNKNOWN MESSAGE TYPES ARE IGNORED. The hub read loop (contribute_ws.go)
//     already drops a message whose "type" it does not recognise via the
//     switch's implicit default, and the relay's handleMessage does the same.
//     This is the forward-compatibility rule: a newer peer may introduce a new
//     message type, and an older peer silently ignores it rather than erroring.
//     The protocol version + capability set below let a NEWER client degrade
//     INTENTIONALLY — it can learn from auth_ok what the deployed server
//     supports (e.g. token_refresh, task_unavailable reasons, prompt preview)
//     instead of probing and reacting to silence.
//
//   - This is the DECLARE half of #2547 only. Clients may DECLARE their runtime
//     posture (container runtime, OS/arch, agent/relay versions, credential
//     type); the hub PARSES, STORES, and SURFACES it read-only. It does NOT
//     ROUTE or gate work on any declared capability — that is the deferred Gate
//     half and is deliberately out of scope here.
package dashboard

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// contributorProtocolVersion is the contributor WebSocket protocol version this
// hub speaks. It is advertised to clients on auth_ok (#2567) so a relay can
// learn the deployed protocol level without probing. Bump ONLY on a wire
// contract change; new OPTIONAL fields do not require a bump because they are
// backward-compatible by construction. Semantic: MAJOR.MINOR where a MINOR bump
// is purely additive and a MAJOR bump would be a breaking change (none is made
// here).
const contributorProtocolVersion = "1.2"

// Server capability tokens advertised on auth_ok (#2567). Each names a message
// type or feature this hub supports so a client can adapt without probing. They
// are stable identifiers, not free text; add new ones as features land.
const (
	// capTokenRefresh: the hub proactively re-mints and pushes a fresh
	// github_token via a token_refresh message before the old one expires
	// (see wsTokenRefreshPeriod / #2393 item 2).
	capTokenRefresh = "token_refresh"
	// capTaskUnavailableReasons: task_unavailable carries a machine-readable
	// reason (the taskUnavailable* set, #2436/#2546) rather than silence.
	capTaskUnavailableReasons = "task_unavailable_reasons"
	// capPromptPreview: the ops surface can preview the exact assignment prompt
	// without exposing the minted token (#2539).
	capPromptPreview = "prompt_preview"
	// capCapabilityDeclare: the hub accepts, stores, and surfaces client-declared
	// capabilities in auth_response (#2547 declare half).
	capCapabilityDeclare = "capability_declare"
	// capCredentialAfterAccept: the hub splits the scoped GitHub credential OUT of
	// task_assign and delivers it (via a token_refresh) only AFTER the task's
	// acceptance decision (#2537). A client that sees this capability knows the
	// task_assign it receives carries NO github_token and that the credential
	// arrives in a following token_refresh — but no client change is required, since
	// the token_refresh delivery is backward-compatible (see deliverTaskCredential).
	capCredentialAfterAccept = "credential_after_accept"
	// capAgentRoleClaim: the hub accepts an OPTIONAL auth_response.role request
	// from contributor relays and, when allowed by hive config/tier/grants, shapes
	// assignments with the matching spoke agent's prompt.
	capAgentRoleClaim = "agent_role_claim"
)

// serverCapabilities returns the capability set this hub advertises on auth_ok.
// It is a fixed list of what the deployed server supports; it is order-stable so
// the wire payload is deterministic across connections.
func serverCapabilities() []string {
	return []string{
		capTokenRefresh,
		capTaskUnavailableReasons,
		capPromptPreview,
		capCapabilityDeclare,
		capCredentialAfterAccept,
		capAgentRoleClaim,
	}
}

// Surface identifiers for the /api/contribute/status response (#2567). Two
// public deployments of this same binary answer /api/contribute/status with the
// same handler — the Hub discovery front door and a selected spoke — and until
// now no field said which one replied, so a wrong-base-URL request returned 200
// and looked valid (verified live: both 200, no discriminator). The status
// response now carries one of these so a client can tell which surface answered
// and a wrong URL fails LOUDLY (identifiable) instead of silently.
const (
	surfaceHub   = "hub"
	surfaceSpoke = "spoke"
)

// ContributorCapabilities is the OPTIONAL, client-declared runtime posture a
// contributor relay may report in its auth_response (#2547 declare half). Every
// field is omitempty: a client that declares nothing sends an absent/empty
// object and is treated exactly as an unversioned client. The hub STORES this on
// the connection and SURFACES it read-only (FleetClanker) so the Operations tab
// COULD show it — it is NEVER used to route or gate work.
//
// Fields are honest self-reports the relay can cheaply determine; none is
// trusted for a security decision (server-side policy still governs what a
// contributor may do — see the Restrictions reservation in contribute_ws.go).
type ContributorCapabilities struct {
	// ContainerRuntime is the container runtime the relay found available:
	// "docker", "podman", or "none". Advisory only.
	ContainerRuntime string `json:"container_runtime,omitempty"`
	// OS and Arch are the client's operating system and CPU architecture
	// (e.g. "linux"/"amd64"), as the client reports them.
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// AgentCLIVersion is the version string of the agent CLI backend the relay
	// drives (claude/codex/goose/…), when the relay can determine it.
	AgentCLIVersion string `json:"agent_cli_version,omitempty"`
	// RelayProtocolVersion is the contributor-protocol version the relay speaks.
	// It lets the hub see, read-only, which protocol level a connected client is
	// on (the mirror of the version the hub advertises on auth_ok).
	RelayProtocolVersion string `json:"relay_protocol_version,omitempty"`
	// CredentialType names the kind of credential the relay authenticates GitHub
	// with (e.g. "app", "pat", "oauth"), NOT the credential itself. Advisory.
	CredentialType string `json:"credential_type,omitempty"`
}

// IsZero reports whether the client declared no capabilities at all, so the hub
// can store nil (indistinguishable from an unversioned client) rather than an
// empty struct.
func (c ContributorCapabilities) IsZero() bool {
	return c.ContainerRuntime == "" && c.OS == "" && c.Arch == "" &&
		c.AgentCLIVersion == "" && c.RelayProtocolVersion == "" && c.CredentialType == ""
}

// capabilityFieldMaxLen bounds each declared capability field the hub is willing
// to store. These are compact tokens ("podman", "linux", "1.2.3") rendered inline
// on one operator row, so 64 runes is far more than any honest value needs while
// still fitting the surface. Mirrors the existing bound on other client-supplied
// display strings (bobKeyNameMaxLen / gatewayKeyNameMaxLen).
const capabilityFieldMaxLen = 64

// Sanitized returns the declaration bounded and made printable.
//
// A declaration is UNVERIFIED CLIENT TEXT — that is the whole premise of the
// declare half (#2547): the hub stores what it is told and shows it, and a client
// can say anything. "Anything" includes 64KB of padding (the read limit is the
// only ceiling on the wire) and embedded newlines or escapes, and this value is
// held for the life of the connection, re-serialized into every fleet poll, and
// rendered into an operator row. Trusting a client to self-limit its own display
// string is not a bound at all, so the hub applies one on receipt: control
// characters become spaces, runs of whitespace collapse, and each field is
// truncated to capabilityFieldMaxLen runes.
//
// This is hygiene on a display value, NOT validation: no field is checked against
// a vocabulary, nothing is rejected, and an over-long or messy declaration is
// still accepted and still authenticates. Declaring badly must never cost a
// client its connection or its work.
func (c ContributorCapabilities) Sanitized() ContributorCapabilities {
	return ContributorCapabilities{
		ContainerRuntime:     sanitizeCapabilityField(c.ContainerRuntime),
		OS:                   sanitizeCapabilityField(c.OS),
		Arch:                 sanitizeCapabilityField(c.Arch),
		AgentCLIVersion:      sanitizeCapabilityField(c.AgentCLIVersion),
		RelayProtocolVersion: sanitizeCapabilityField(c.RelayProtocolVersion),
		CredentialType:       sanitizeCapabilityField(c.CredentialType),
	}
}

// sanitizeCapabilityField makes one declared value printable and bounded.
// Truncation is by rune, not byte, so a multi-byte value is never cut mid-rune
// into invalid UTF-8 on the way to the fleet JSON. U+FFFD is dropped: encoding/json
// substitutes it for undecodable bytes, and a replacement character is never part
// of an honest runtime/version token.
func sanitizeCapabilityField(s string) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			// Collapse any whitespace run (including embedded newlines) to one
			// space, and never let one lead the value.
			pendingSpace = b.Len() > 0
			continue
		}
		if pendingSpace {
			b.WriteRune(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > capabilityFieldMaxLen {
		n := 0
		for i := range out {
			if n == capabilityFieldMaxLen {
				out = out[:i]
				break
			}
			n++
		}
	}
	return strings.TrimSpace(out)
}
