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
//   - Client capabilities are self-reported. The hub may use an explicit
//     "cannot fit" declaration to avoid wasting an assignment on a matching
//     task requirement, but it must never treat the declaration as a trust or
//     security signal. Unknown/absent still means unknown, not incapable.
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
	// capCompletionVerdict: the hub accepts an OPTIONAL verdict field on
	// task_complete (#3987) — "no_work_needed" lets a relay report that the
	// agent affirmatively determined nothing is shippable (maintainer-gated
	// remainder / already covered by merged work), earning the issue a long
	// offer-pool suppression instead of the short idle cooldown loop. Purely
	// additive: a relay that never sends the field behaves exactly as before.
	capCompletionVerdict = "completion_verdict"
	// capCapabilityRouting: the hub can derive task requirements from labels and
	// avoid assigning a task to a client that explicitly declared it cannot fit.
	// Self-reported, advisory, and backward-compatible: undeclared clients still
	// receive work as before.
	capCapabilityRouting = "capability_routing"
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
		capCompletionVerdict,
		capCapabilityRouting,
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
// contributor relay may report in its auth_response (#2547). Every
// field is omitempty: a client that declares nothing sends an absent/empty
// object and is treated exactly as an unversioned client. The hub STORES this on
// the connection and SURFACES it. Routing may only use it to avoid tasks whose
// requirements the client explicitly cannot satisfy; it is NEVER a trust signal.
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
	// Pi readiness is a four-stage, machine-readable declaration (#5039). Auth
	// deliberately distinguishes configured_unverified from verified: a key or
	// auth-file entry is configuration, not proof that a provider accepted it.
	// Invocation becomes succeeded only after Pi completes a real one-shot task.
	PiBinary         string `json:"pi_binary,omitempty"`
	PiConfiguration  string `json:"pi_configuration,omitempty"`
	PiAuthentication string `json:"pi_authentication,omitempty"`
	PiInvocation     string `json:"pi_invocation,omitempty"`
}

// ContributorTaskRequirements is the hub-derived task-side vocabulary used for
// the ROUTE half of #2547. It is intentionally tiny and label-derived: operators
// can add labels without changing issue bodies, and old relays remain compatible
// because an undeclared client is treated as unknown rather than incapable.
type ContributorTaskRequirements struct {
	ContainerRuntime string `json:"container_runtime,omitempty"`
	OS               string `json:"os,omitempty"`
	Arch             string `json:"arch,omitempty"`
	CLIBackend       string `json:"cli_backend,omitempty"`
	CredentialType   string `json:"credential_type,omitempty"`
}

// IsZero reports whether a task has no explicit capability requirements.
func (r ContributorTaskRequirements) IsZero() bool {
	return r.ContainerRuntime == "" && r.OS == "" && r.Arch == "" &&
		r.CLIBackend == "" && r.CredentialType == ""
}

// TaskRequirementsFromLabels derives hard routing requirements from issue
// labels. The vocabulary is deliberately explicit: labels outside these forms
// remain ordinary triage labels and do not affect assignment.
func TaskRequirementsFromLabels(labels []string) ContributorTaskRequirements {
	var out ContributorTaskRequirements
	for _, raw := range labels {
		l := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case l == "needs-container" || l == "requires-container":
			if out.ContainerRuntime == "" {
				out.ContainerRuntime = "container"
			}
		case l == "needs-docker" || l == "requires-docker" || l == "runtime/docker":
			out.ContainerRuntime = "docker"
		case l == "needs-podman" || l == "requires-podman" || l == "runtime/podman":
			out.ContainerRuntime = "podman"
		case strings.HasPrefix(l, "os/"):
			out.OS = strings.TrimSpace(strings.TrimPrefix(l, "os/"))
		case strings.HasPrefix(l, "arch/"):
			out.Arch = strings.TrimSpace(strings.TrimPrefix(l, "arch/"))
		case strings.HasPrefix(l, "backend/"):
			out.CLIBackend = strings.TrimSpace(strings.TrimPrefix(l, "backend/"))
		case strings.HasPrefix(l, "credential/"):
			out.CredentialType = strings.TrimSpace(strings.TrimPrefix(l, "credential/"))
		}
	}
	return out
}

// ContributorCanRunTask reports whether a self-declared client fits the task's
// requirements. Unknown always fits for backward compatibility; only an explicit
// contradictory declaration excludes the client.
func ContributorCanRunTask(caps *ContributorCapabilities, cliBackend string, req ContributorTaskRequirements) bool {
	if req.IsZero() || caps == nil || caps.IsZero() {
		return true
	}
	if req.ContainerRuntime != "" {
		have := strings.ToLower(strings.TrimSpace(caps.ContainerRuntime))
		want := strings.ToLower(req.ContainerRuntime)
		if want == "container" {
			if have == "none" {
				return false
			}
		} else if have != "" && have != want {
			return false
		}
	}
	if !capabilityFieldFits(caps.OS, req.OS) {
		return false
	}
	if !capabilityFieldFits(caps.Arch, req.Arch) {
		return false
	}
	if !capabilityFieldFits(caps.CredentialType, req.CredentialType) {
		return false
	}
	if req.CLIBackend != "" {
		have := strings.ToLower(strings.TrimSpace(cliBackend))
		want := strings.ToLower(req.CLIBackend)
		if have != "" && have != want {
			return false
		}
	}
	return true
}

func capabilityFieldFits(have, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return true
	}
	have = strings.ToLower(strings.TrimSpace(have))
	return have == "" || have == want
}

// IsZero reports whether the client declared no capabilities at all, so the hub
// can store nil (indistinguishable from an unversioned client) rather than an
// empty struct.
func (c ContributorCapabilities) IsZero() bool {
	return c.ContainerRuntime == "" && c.OS == "" && c.Arch == "" &&
		c.AgentCLIVersion == "" && c.RelayProtocolVersion == "" && c.CredentialType == "" &&
		c.PiBinary == "" && c.PiConfiguration == "" && c.PiAuthentication == "" && c.PiInvocation == ""
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
		PiBinary:             sanitizeCapabilityField(c.PiBinary),
		PiConfiguration:      sanitizeCapabilityField(c.PiConfiguration),
		PiAuthentication:     sanitizeCapabilityField(c.PiAuthentication),
		PiInvocation:         sanitizeCapabilityField(c.PiInvocation),
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

// Task failure kinds (#2547). A relay MAY tag a task_failed with the kind of
// failure it observed, so an operator can tell a work item that failed on its
// merits from one that was fine and simply landed on a client whose environment
// could not run it.
//
// The issue's own framing: "a task that failed because the client couldn't run
// it and a task that failed because the agent got it wrong are, from the hub's
// side, the same event with different terminal scrollback." Today the hub logs
// task_failed's reason and discards it, so that inference is left to whoever
// reads a tmux tail.
//
// Like ContributorCapabilities, this is SELF-REPORTED and advisory. It is
// stored and surfaced read-only; it does NOT influence selection, admission, or
// the failure cooldown. That separation is the DECLARE/ROUTE split this issue
// exists to keep: acting on it is ROUTE, which is intentionally undecided, and
// making a work item's cooldown depend on a client-controlled value would be
// exactly the "routing on a value the client controls" hazard the issue names.
const (
	// TaskFailureKindEnvironment: the client's own runtime could not run the
	// work (no container runtime, agent CLI never started or crashed, missing
	// toolchain). The work item itself is unjudged.
	TaskFailureKindEnvironment = "environment"
	// TaskFailureKindTask: the work was attempted and failed on its merits.
	TaskFailureKindTask = "task"
	// TaskFailureKindUnspecified is the value for a relay that sent no kind, or
	// sent one this hub does not recognize. It is deliberately the default for
	// EVERY older relay: absent must never be read as either of the above, or a
	// hub would be inferring a cause no client stated.
	TaskFailureKindUnspecified = "unspecified"
)

// NormalizeTaskFailureKind maps a client-supplied failure kind onto the known
// set, collapsing absent/unknown values to TaskFailureKindUnspecified.
//
// Unknown values are NOT preserved verbatim: the field is rendered to
// operators, and echoing an arbitrary client string into that surface would let
// a client write free text into the operator's view. Matching is
// case-insensitive and space-trimmed so a relay's "Environment" is not silently
// demoted to unspecified on a cosmetic difference.
func NormalizeTaskFailureKind(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case TaskFailureKindEnvironment:
		return TaskFailureKindEnvironment
	case TaskFailureKindTask:
		return TaskFailureKindTask
	default:
		return TaskFailureKindUnspecified
	}
}

// ContributorFailure is a read-only record of the most recent task failure a
// connection reported (#2547). It is stored on the connection and surfaced on
// FleetClanker, mirroring how lastIdleReason/IdleReason (#2546) made "why is
// this clanker idle" answerable instead of an indistinguishable silence.
//
// Metadata only — never a credential. Reason is client-supplied free text and
// is scrubbed before it is surfaced.
type ContributorFailure struct {
	TaskID string `json:"task_id,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// Kind is the normalized, self-reported cause (see NormalizeTaskFailureKind).
	Kind string `json:"kind,omitempty"`
	// Reason is the client's free-text explanation, as already carried on
	// task_failed today and, until now, only written to the hub log.
	Reason string `json:"reason,omitempty"`
	// Permanent mirrors the task_failed flag: the client does not expect a retry
	// to succeed.
	Permanent bool `json:"permanent,omitempty"`
	// At is when the hub recorded the failure (RFC3339, UTC).
	At string `json:"at,omitempty"`
}

// maxFailureReasonLen bounds the client-supplied task_failed reason surfaced on
// the fleet view (#2547). The reason is free text a relay chose — an error
// string, or whatever a future relay decides to put there — so it is bounded at
// the point it becomes operator-visible rather than trusted to be short. 512
// characters comfortably holds a real error line while keeping one clanker's
// row from dominating a fleet snapshot.
const maxFailureReasonLen = 512

// truncateFailureReason bounds a failure reason for display, marking any
// truncation so an operator can tell a cut-off message from a terse one.
// Truncation is rune-safe: cutting mid-rune would emit invalid UTF-8 into a
// JSON response.
func truncateFailureReason(s string) string {
	r := []rune(s)
	if len(r) <= maxFailureReasonLen {
		return s
	}
	return string(r[:maxFailureReasonLen]) + "… (truncated)"
}
