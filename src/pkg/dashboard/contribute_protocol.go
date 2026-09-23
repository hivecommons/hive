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
const contributorProtocolVersion = "1.4"

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
	// capBlockedVerdict: the hub reads the OPTIONAL verdict_blocked marker on
	// a no_work_needed task_complete (hivecommons/hive#7924) — the agent said
	// nothing in the repo can move until something outside it lands — and
	// holds the issue for the full with-PR cooldown instead of the escalating
	// no-PR ladder. Purely additive: the relay spells the verdict as the
	// no_work_needed a hub without this capability already books, so nothing
	// on the wire depends on the hub advertising it.
	capBlockedVerdict = "blocked_verdict"
	// capTokenRefreshFailed: when a mid-task re-mint FAILS, the hub tells the
	// relay so with a token_refresh_failed message instead of only logging it
	// hub-side (#5447). Without it the relay's first evidence that its
	// credential went stale is a push that starts failing roughly an hour into
	// a long task, reported to the agent as a generic auth error — the
	// misleading-symptom class of #5343. Purely additive and advisory: the
	// existing token stays in place and the hub keeps retrying on the next
	// heartbeat exactly as before, so a relay that ignores the message behaves
	// precisely as it does today.
	capTokenRefreshFailed = "token_refresh_failed"
	// capQuotaPreflight: the hub accepts a contributor-local quota preflight
	// decline after task metadata is offered but before the scoped credential is
	// delivered. The decline is local capacity, not task failure.
	capQuotaPreflight = "quota_preflight_v1"
	// capRunStage: the relay can receive stage-scoped run assignments. Unlike
	// plain task metadata, a stage is load-bearing for per-stage generations, so
	// opted-in run-stage work is offered only to relays that declare this token.
	capRunStage = "run-stage"
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
		capTokenRefreshFailed,
		capQuotaPreflight,
		capBlockedVerdict,
		capRunStage,
	}
}

func relaySupportsRunStage(c *ContributorConnection) bool {
	if c == nil || c.capabilities == nil {
		return false
	}
	return c.capabilities.DeclaresCapability(capRunStage)
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
	// RelayCapabilities is the relay's OUTBOUND capability set — the mirror of the
	// hub's server_capabilities (serverCapabilities()), letting a relay name the
	// negotiated features it actually implements rather than the hub inferring
	// them from a version number (kubestellar/hive#6954). This is the reverse
	// direction #6931 left unbuilt: quota_preflight_v1 existed only as a
	// hub-outbound string, so the hub had no field in which a relay could answer
	// and fell back to proxying on RelayProtocolVersion — which withholds the
	// auto-accept credential from EVERY relay that declares any version, whether
	// or not it has ever heard of the capability. Each entry is a stable token
	// (e.g. "quota_preflight_v1"); an undeclared/empty list reads as "declares no
	// negotiated capability" and is treated exactly as a pre-#6833 relay. Like
	// every other field here it is an honest self-report, bounded on receipt and
	// NEVER a trust signal — it only lets the hub gate on the ADVERTISED set
	// instead of a version proxy. Compatibility boundary with #6825: this adds a
	// relay→hub capability list to the assignment boundary that RFC #6825 is still
	// designing; #6954 is the sub-issue split out to carry exactly this wire
	// change independently, so whichever of the two lands second must not redefine
	// the token vocabulary silently.
	RelayCapabilities []string `json:"relay_capabilities,omitempty"`
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

// IsZero reports whether the client declared no capabilities at all, so the hub
// can store nil (indistinguishable from an unversioned client) rather than an
// empty struct.
func (c ContributorCapabilities) IsZero() bool {
	return c.ContainerRuntime == "" && c.OS == "" && c.Arch == "" &&
		c.AgentCLIVersion == "" && c.RelayProtocolVersion == "" && c.CredentialType == "" &&
		len(c.RelayCapabilities) == 0 &&
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
		RelayCapabilities:    sanitizeCapabilityTokens(c.RelayCapabilities),
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

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func sanitizeKnowledgeError(s string) string {
	return truncateFailureReason(redactTokens(sanitizeString(s)))
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

// DeclaresCapability reports whether the relay advertised the named negotiated
// capability token (kubestellar/hive#6954). It is the hub-side read of
// RelayCapabilities and the reason the field exists: the hub gates on the
// ADVERTISED set, never on a protocol-version proxy. Matching is exact against a
// sanitized token, so trailing whitespace or control characters a client padded
// in cannot make a capability appear or disappear.
func (c ContributorCapabilities) DeclaresCapability(token string) bool {
	want := sanitizeCapabilityField(token)
	if want == "" {
		return false
	}
	for _, have := range c.RelayCapabilities {
		if sanitizeCapabilityField(have) == want {
			return true
		}
	}
	return false
}

// capabilityListMaxLen bounds how many relay-declared capability tokens the hub
// will store (kubestellar/hive#6954). The negotiated set is small and stable —
// a handful of tokens — so 32 is far more than any honest relay sends while
// still capping a client that pads the list to bloat every fleet poll.
const capabilityListMaxLen = 32

// sanitizeCapabilityTokens bounds and cleans a relay-declared capability list
// (kubestellar/hive#6954). Each token is run through sanitizeCapabilityField so
// the same control-character/length hygiene the other declared fields get
// applies here, empties (a token that sanitizes to nothing) are dropped so a
// whitespace entry cannot masquerade as a capability, and the list is capped at
// capabilityListMaxLen. Like Sanitized() this is hygiene, not validation: no
// token is checked against a vocabulary and a nonsense token is still stored, it
// simply cannot match a real capability on the exact compare in DeclaresCapability.
func sanitizeCapabilityTokens(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		tok := sanitizeCapabilityField(raw)
		if tok == "" {
			continue
		}
		out = append(out, tok)
		if len(out) >= capabilityListMaxLen {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
