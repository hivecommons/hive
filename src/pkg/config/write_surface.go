package config

import (
	"sort"
	"strconv"
	"strings"
)

// WriteSurfaceAllowAll is the allowlist entry that grants a lane every relay
// operation. It lets an operator list a lane explicitly (so it is visible in
// config) without narrowing it.
const WriteSurfaceAllowAll = "*"

// WriteSurfaceConfig is the per-lane allowlist for the audited GitHub write
// surface (hivecommons/hive#9587). The relays (hive-open-pr, hive-open-issue,
// review and merge requests) consult it after authorizing the request's agent
// and before any GitHub call; a refused request is quarantined with an
// explanation and audited as agent_write_refused.
//
// Operation names are the pkg/github WriteOp* constants: open_pr,
// create_issue, comment, claim, close_issue, review, resolve_thread, merge_pr.
// See docs/github-write-surface.md.
type WriteSurfaceConfig struct {
	// Allowlist maps an agent (lane) name to the operations it may perform.
	// An agent with no entry is unrestricted, which is every agent on every
	// hive that does not set this block. A replica ("scanner-2") with no entry
	// of its own uses its base agent's. An entry that is present but empty
	// allows nothing.
	Allowlist map[string][]string `yaml:"allowlist,omitempty" json:"allowlist,omitempty"`
}

// AgentMayWrite reports whether the named agent's lane may perform op through
// the write relays. It is the single predicate the relays ask.
//
// An unconfigured allowlist, an unnamed agent, and an agent with no entry all
// answer true: the allowlist can only NARROW what an agent may already do, so
// turning the feature on for one lane changes nothing for the others.
func (c *Config) AgentMayWrite(agent, op string) bool {
	if c == nil || len(c.WriteSurface.Allowlist) == 0 {
		return true
	}
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return true
	}
	allowed, ok := c.WriteSurface.Allowlist[agent]
	if !ok {
		if base := c.BaseAgentName(agent); base != agent {
			allowed, ok = c.WriteSurface.Allowlist[base]
		}
	}
	if !ok {
		return true
	}
	op = strings.ToLower(strings.TrimSpace(op))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == WriteSurfaceAllowAll || (a != "" && a == op) {
			return true
		}
	}
	return false
}

// KnownWriteOps is the relay operation vocabulary, duplicated from
// pkg/github.WriteOps because this package must not import pkg/github. The
// parity test in pkg/github fails the build of that package's tests when the
// two lists drift.
var KnownWriteOps = []string{
	"open_pr",
	"create_issue",
	"comment",
	"claim",
	"close_issue",
	"review",
	"resolve_thread",
	"merge_pr",
}

// WriteSurfaceWarnings reports allowlist entries that cannot do what they say:
// an operation name the relays do not know. A misspelt name is not an error -
// the allowlist fails closed for it, which is the safe direction - but it
// silently denies the operation the operator meant to grant, so it is worth a
// line in the boot log.
func WriteSurfaceWarnings(cfg *Config) []string {
	if cfg == nil || len(cfg.WriteSurface.Allowlist) == 0 {
		return nil
	}
	known := make(map[string]bool, len(KnownWriteOps)+1)
	for _, op := range KnownWriteOps {
		known[op] = true
	}
	known[WriteSurfaceAllowAll] = true
	agents := make([]string, 0, len(cfg.WriteSurface.Allowlist))
	for agent := range cfg.WriteSurface.Allowlist {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	var out []string
	for _, agent := range agents {
		for _, op := range cfg.WriteSurface.Allowlist[agent] {
			norm := strings.ToLower(strings.TrimSpace(op))
			if !known[norm] {
				out = append(out, "write_surface.allowlist."+agent+": unknown operation "+strconv.Quote(op)+" (known: "+strings.Join(KnownWriteOps, ", ")+")")
			}
		}
	}
	return out
}
