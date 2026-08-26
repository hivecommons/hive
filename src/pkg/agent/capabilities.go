package agent

import "strings"

// AgentCapabilities carries the ORTHOGONAL permissions an agent holds — the
// ones that are not a rung on the AgentMode ladder (#4492).
//
// AgentMode is an ordered trust tier: ADVISORY < ISSUES_ONLY < ISSUES_AND_PRS <
// ISSUES_PRS_MERGE, and everything it gates is a strict superset of the tier
// below. Conversation does not fit that shape. Commenting on an issue is not a
// step between "observe" and "file issues"; it is a different axis. A reviewer
// may reasonably want an advisory agent that talks, or a merge-capable agent
// that stays silent, and neither is expressible by moving along the ladder.
//
// Inserting a fifth ordered mode would also renumber the enum, which is used as
// an array index (modeNames/modeEmojis/modeSuffixes, agentModeCount) and mapped
// to policy-file suffixes, so every agent would need a new policy template for
// a permission that is not a trust tier.
//
// Capabilities are therefore a separate, unordered set. They only ever WIDEN
// what a mode permits — see proxy.AllowedByModeCaps, where a capability is an
// additional grant path checked beside the tier comparison, never a replacement
// for it. A zero AgentCapabilities is exactly today's behaviour.
type AgentCapabilities struct {
	// Converse permits conversation-shaped writes — posting a comment on an
	// issue or PR, and leaving a PR review — independently of the agent's mode.
	//
	// It does NOT permit artifact production: creating or editing an issue,
	// relabelling, pushing, opening or merging a PR all remain on the mode
	// ladder. An ADVISORY agent with Converse can reply on a thread it was
	// mentioned in; it still cannot file, edit or relabel anything.
	Converse bool
}

// CanConverse reports whether this agent may post comments and reviews.
func (a AgentCapabilities) CanConverse() bool { return a.Converse }

// Any reports whether any capability is set. A zero value means "mode alone
// decides", which is how every hive behaved before capabilities existed.
func (a AgentCapabilities) Any() bool { return a.Converse }

// DefaultCapabilities returns the capabilities an agent holds when its config
// says nothing.
//
// COMPATIBILITY CONTRACT (#4492): this is the zero value at every mode and ACMM
// level, deliberately. `converse` is opt-in everywhere.
//
// The RFC proposed defaulting it on "at exactly the levels where those
// operations are permitted today", but a single flag cannot express that: the
// two operations it governs sit at DIFFERENT tiers today — issue comments at
// ISSUES_ONLY, PR reviews at ISSUES_AND_PRS. Defaulting on at ISSUES_ONLY would
// newly grant PR reviews two tiers early; defaulting on at ISSUES_AND_PRS would
// newly forbid comments an ISSUES_ONLY agent can post now. Either is a
// behaviour change on existing hives.
//
// Keeping the default off, and treating the capability as an ADDITIONAL grant
// beside the unchanged tier floors, gives the compatibility contract the RFC
// actually asked for — every existing hive behaves identically — while still
// making both of the shapes it wanted reachable:
//
//   - "advisory agent that talks": mode ADVISORY + converse
//   - "comment without issue mutation": mode ADVISORY + converse (an
//     ISSUES_ONLY agent keeps issue mutation because that is what its mode
//     means, so the narrow grant is expressed by lowering the mode, not by
//     subtracting from it)
func DefaultCapabilities(_ AgentMode, _ int) AgentCapabilities {
	return AgentCapabilities{}
}

// capabilityConverse is the token used in the on-disk capability file and in
// the `converse` agent config field.
const capabilityConverse = "converse"

// String renders the capability set as the comma-separated token list written
// to /tmp/.hive-caps-<agent> and read back by the proxy. An empty set renders
// as the empty string, so the file for a default agent is empty rather than
// absent-with-meaning.
func (a AgentCapabilities) String() string {
	var caps []string
	if a.Converse {
		caps = append(caps, capabilityConverse)
	}
	return strings.Join(caps, ",")
}

// ParseCapabilities reads the token list String() writes. Unknown tokens are
// ignored rather than rejected: this file is read by the proxy on the request
// path, and a token from a newer hive version must degrade to "capability not
// held" — the deny-by-default direction — not to a parse error that would be
// indistinguishable from a missing file.
func ParseCapabilities(s string) AgentCapabilities {
	var a AgentCapabilities
	for _, tok := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case capabilityConverse:
			a.Converse = true
		}
	}
	return a
}
