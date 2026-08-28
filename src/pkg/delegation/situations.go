package delegation

import (
	"fmt"
	"strings"
)

// THE FIVE IDENTITY SITUATIONS.
//
// This file is the enumeration the chain shape was designed AROUND, not a set
// of convenience constructors bolted on afterwards. The order matters: the
// shape of a delegation chain is only defensible if it was derived from the
// real authorization stories hive already has, and hive has exactly five.
//
// Each constructor below returns ErrNoHonestRoot rather than a chain when its
// inputs do not establish the root it needs. Every one of them CAN return it,
// including the ones with a human root — a caller that cannot resolve a user is
// exactly the caller that must not claim one.

// SituationID names one of the five for logs, docs, and the comparison harness.
type SituationID string

const (
	// SituationHostedSpokeAgent — (a) a hosted spoke agent acting under a
	// GitHub App INSTALLATION token.
	//
	// THE ROOT IS NOT A HUMAN, AND THIS IS THE SITUATION MOST LIKELY TO BE GOT
	// WRONG. A ghs_… installation token has no user identity at all: `gh api
	// user` 403s for every staff agent, which is precisely why gh-wrapper.sh
	// reads the trusted bot-login file (github.BotLoginFilePath()) instead of
	// asking the token who it is (#4044/#4049). There is no person behind the
	// credential to discover, so the honest root is the App installation —
	// PrincipalApp — and the hive whose configuration scoped it.
	//
	// The tempting wrong answer is to root this at the hive owner, since a
	// human did once install the App. That is a fabricated root: the owner did
	// not authorize THIS action, may not know it happened, and may no longer be
	// with the org. The installation is what authorized it.
	SituationHostedSpokeAgent SituationID = "hosted_spoke_agent"

	// SituationSelfHostedSpoke — (b) a self-hosted / native spoke.
	//
	// Distinguished from (a) by WHOSE authority scopes the agent. A hosted
	// spoke is provisioned by the operator's hub; a self-hosted spoke runs
	// under its own operator's master secret with no hub provisioning in the
	// chain. Collapsing the two would tell a tenant that the fleet operator
	// authorized something the fleet operator never touched — which is exactly
	// the assurance a multi-tenant chain exists to get right.
	SituationSelfHostedSpoke SituationID = "self_hosted_spoke"

	// SituationContributePlaneUser — (c) a contribute-plane client acting with
	// a USER token.
	//
	// The only situation with a genuinely human root, and the only one where a
	// human root is honest. A user token DOES resolve /user (the exact opposite
	// of the installation-token case), and the dashboard resolves the identity
	// server-side — session, then the hub-injected X-Hive-User, then the
	// persisted owner token — never from a client-supplied header
	// (resolveViewerUsername, api_contribute.go). The chain must be built from
	// that server-resolved value and nothing else.
	SituationContributePlaneUser SituationID = "contribute_plane_user"

	// SituationHubDirective — (d) a hub→spoke call: a directive delivered on
	// the heartbeat response.
	//
	// The heartbeat RESPONSE is hive's command channel — UpgradeTo,
	// SwitchToTag, RestartSpoke, AuthorizedUsers. Authentication runs only in
	// the reverse direction: the spoke proves identity with its per-hive
	// derived bearer, and the response is trusted because it arrived on that
	// connection. So the response carries NO actor, and a spoke that restarts
	// on a hub directive has no field recording who caused it.
	//
	// The honest root is therefore PrincipalHub — the control plane itself —
	// and NOT whichever hub admin might have clicked something. The hub admin's
	// identity is genuinely not present in the data the spoke receives, and a
	// chain that named one would be inventing it. If a future PR propagates an
	// originating admin onto the directive, this situation gains a link; until
	// then it honestly bottoms out at the control plane.
	SituationHubDirective SituationID = "hub_directive"

	// SituationScheduledWork — (e) scheduled / cadence-triggered agent work
	// that NO HUMAN INITIATED.
	//
	// The governor selects agents purely from wall-clock state
	// (agentsDueForKick, governor.go) and SendKick takes no actor at all. This
	// is the largest volume of unattributed action in hive, and the situation
	// the PrincipalHiveAuthority type was introduced for.
	//
	// The root is the HIVE'S OWN DELEGATED AUTHORITY: an operator delegated
	// standing authority when they configured the cadence, and the hive is
	// exercising it. That is a true statement about authorization that names no
	// person. Naming the operator who set the cadence would be the fabrication
	// — they authorized the STANDING RULE, not this occurrence, and the
	// distinction is the whole reason #4055 coined `hook:<name>` instead of
	// attributing hook pauses to a dashboard user.
	SituationScheduledWork SituationID = "scheduled_work"
)

// AllSituations is the closed enumeration, in documentation order. The
// table-driven tests assert every entry has coverage, so adding a situation
// without a test is a build-visible omission rather than a silent gap.
var AllSituations = []SituationID{
	SituationHostedSpokeAgent,
	SituationSelfHostedSpoke,
	SituationContributePlaneUser,
	SituationHubDirective,
	SituationScheduledWork,
}

// HostedSpokeAgentChain builds situation (a).
//
//	agent:<name>@<hive>  ->  app:<bot>[installation]  ->  hive_authority:<hive>
//
// (rendered root-first; on the wire the agent is `sub` and the rest nest in
// `act`, outermost-first, per RFC 8693.)
//
// botLogin is the App bot identity ("<app-slug>[bot]") as published by the hive
// process into the trusted bot-login file — the same unspoofable oracle
// gh-wrapper.sh uses. It is NOT read from the token, because the token cannot
// answer.
//
// Returns ErrNoHonestRoot when the bot login or installation is unknown. That
// is the correct outcome and not a degraded one: without the installation there
// is nothing to prove the App was authorized on this account, and a chain
// asserting it anyway would be evidence of something we did not check.
func HostedSpokeAgentChain(hiveID, agentName, botLogin string, appID, installationID int64) (Chain, error) {
	if strings.TrimSpace(hiveID) == "" || strings.TrimSpace(agentName) == "" {
		return Chain{}, ErrNoHonestRoot
	}
	if strings.TrimSpace(botLogin) == "" || installationID == 0 {
		// The identity oracle did not resolve. #4049's lesson is that this is a
		// REAL and recurring state (the file can be absent on a degraded path),
		// so it must be handled as "emit nothing" rather than treated as
		// impossible.
		return Chain{}, ErrNoHonestRoot
	}
	return Chain{
		Version: ChainVersion,
		HiveID:  hiveID,
		Subject: Principal{Type: PrincipalAgent, ID: agentName, HiveID: hiveID},
		Actors: []Principal{
			{
				Type:           PrincipalApp,
				ID:             botLogin,
				HiveID:         hiveID,
				AppID:          appID,
				InstallationID: installationID,
				Via:            "app-installation-token",
			},
			{Type: PrincipalHiveAuthority, ID: hiveID, HiveID: hiveID, Via: "hosted"},
		},
	}, nil
}

// SelfHostedSpokeChain builds situation (b).
//
//	agent:<name>@<hive>  ->  hive_authority:<hive>(self-hosted)
//
// Two links, not three: there is no App installation in the chain when the
// spoke authenticates with its own operator's material, and no hub provisioning
// authority above it. Shorter than (a) because the real authorization story IS
// shorter — padding it to match would misrepresent who is involved.
func SelfHostedSpokeChain(hiveID, agentName string) (Chain, error) {
	if strings.TrimSpace(hiveID) == "" || strings.TrimSpace(agentName) == "" {
		return Chain{}, ErrNoHonestRoot
	}
	return Chain{
		Version: ChainVersion,
		HiveID:  hiveID,
		Subject: Principal{Type: PrincipalAgent, ID: agentName, HiveID: hiveID},
		Actors: []Principal{
			{Type: PrincipalHiveAuthority, ID: hiveID, HiveID: hiveID, Via: "self-hosted"},
		},
	}, nil
}

// ContributePlaneUserChain builds situation (c).
//
//	user:<login>@<hive>  ->  (root; the user IS the subject)
//
// A one-link chain: the person acted directly, nothing delegated to anything.
// Depth 1 is a legitimate chain, and Chain.Root returns the subject for it.
//
// `username` MUST be the server-resolved identity (resolveViewerUsername's
// session → X-Hive-User → owner-token ladder), never a value read from a
// client-supplied header. Handing this function a request header would
// reintroduce exactly the spoofing hole the dashboard strips inbound
// X-Hive-User to close.
//
// Returns ErrNoHonestRoot for an anonymous caller. An anonymous action has no
// human root by definition, and "" / "local" / "unknown" are pseudo-users in
// the audit log for the same reason — they are not people, and signing one into
// a chain would give a non-identity cryptographic weight it has not earned.
func ContributePlaneUserChain(hiveID, username string) (Chain, error) {
	u := strings.TrimSpace(username)
	if u == "" || isAuditPseudoUser(u) {
		return Chain{}, ErrNoHonestRoot
	}
	return Chain{
		Version: ChainVersion,
		HiveID:  hiveID,
		Subject: Principal{Type: PrincipalUser, ID: u, HiveID: hiveID, Via: "contribute-plane"},
	}, nil
}

// isAuditPseudoUser mirrors pkg/dashboard's auditPseudoUsers set.
//
// Duplicated deliberately rather than imported: pkg/delegation must not depend
// on pkg/dashboard (dashboard imports this package, not the reverse), and the
// set is a four-element stable contract documented in src/docs/audit-log.md.
// The comparison harness cross-checks the two, so a divergence surfaces as
// data rather than drifting silently.
//
// Machine identities coined post-#4055 ("hook:<name>") are deliberately NOT
// listed here: they are not pseudo-users, they are real non-human actors, and
// this package models them with a principal type rather than by exclusion.
func isAuditPseudoUser(u string) bool {
	switch u {
	case "", "system", "local", "unknown":
		return true
	}
	return false
}

// HubDirectiveChain builds situation (d).
//
//	hub:<hubID>  ->  ... acting on hive <hiveID>
//
// `directive` names the command (e.g. "upgrade_to", "restart_spoke",
// "authorized_users") and lands in Via as a mechanism label, not as an actor —
// the same separation PausedTrigger keeps from PausedBy.
//
// NOTE FOR A FUTURE READER: this chain is honestly SHALLOW because the data is.
// The heartbeat response has no actor field, so no originating admin is
// recoverable at the spoke. Do not "improve" this by attributing the directive
// to a hub admin looked up elsewhere — that would be a fabricated root assembled
// from a plausible correlation, which is worse than a shallow honest one. The
// right fix, if it is ever wanted, is to propagate a real actor onto the
// directive at the hub and then add the link here.
func HubDirectiveChain(hubID, hiveID, directive string) (Chain, error) {
	if strings.TrimSpace(hubID) == "" || strings.TrimSpace(hiveID) == "" {
		return Chain{}, ErrNoHonestRoot
	}
	via := strings.TrimSpace(directive)
	if via == "" {
		via = "heartbeat-directive"
	} else {
		via = "heartbeat-directive:" + via
	}
	return Chain{
		Version: ChainVersion,
		HiveID:  hiveID,
		Subject: Principal{Type: PrincipalHub, ID: hubID, HiveID: hiveID, Via: via},
	}, nil
}

// ScheduledWorkChain builds situation (e).
//
//	agent:<name>@<hive>  ->  hive_authority:<hive>(cadence:<agent>)
//
// `cadenceLabel` describes the schedule that fired (e.g. "cadence:scanner",
// "cel-trigger:pr-opened", "startup-kick"). It is a MECHANISM, carried in Via.
//
// There is no human link and there must never be one. The governor picks agents
// from wall-clock state alone; no request, no session, no person. The hive's
// standing authority is the whole of the honest answer.
func ScheduledWorkChain(hiveID, agentName, cadenceLabel string) (Chain, error) {
	if strings.TrimSpace(hiveID) == "" || strings.TrimSpace(agentName) == "" {
		return Chain{}, ErrNoHonestRoot
	}
	via := strings.TrimSpace(cadenceLabel)
	if via == "" {
		// Degrade to the bare kind rather than inventing a schedule name —
		// exactly hookPauseActor's fallback from "hook:<name>" to "hook".
		via = "cadence"
	}
	return Chain{
		Version: ChainVersion,
		HiveID:  hiveID,
		Subject: Principal{Type: PrincipalAgent, ID: agentName, HiveID: hiveID},
		Actors: []Principal{
			{Type: PrincipalHiveAuthority, ID: hiveID, HiveID: hiveID, Via: via},
		},
	}, nil
}

// DescribeSituation returns the documented one-line shape for a situation, used
// by the docs test to keep src/docs/delegation-chain.md honest about what the
// code actually builds.
func DescribeSituation(s SituationID) string {
	switch s {
	case SituationHostedSpokeAgent:
		return "hive_authority:<hive>(hosted) -> app:<bot>(app-installation-token) -> agent:<name>"
	case SituationSelfHostedSpoke:
		return "hive_authority:<hive>(self-hosted) -> agent:<name>"
	case SituationContributePlaneUser:
		return "user:<login>(contribute-plane)"
	case SituationHubDirective:
		return "hub:<hub-id>(heartbeat-directive:<directive>)"
	case SituationScheduledWork:
		return "hive_authority:<hive>(cadence:<label>) -> agent:<name>"
	}
	return fmt.Sprintf("unknown situation %q", s)
}
