package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// writeSurfaceMu guards WriteSurface.Allowlist against the dashboard editor
// (#9587 phase 2). The relays read the allowlist on their own goroutines on
// every request, while the dashboard replaces it at runtime.
//
// Lock order is saveMu -> writeSurfaceMu, never the reverse (the same order as
// agentReposMu).
var writeSurfaceMu sync.RWMutex

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
	if c == nil {
		return true
	}
	writeSurfaceMu.RLock()
	defer writeSurfaceMu.RUnlock()
	if len(c.WriteSurface.Allowlist) == 0 {
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
	if cfg == nil {
		return nil
	}
	writeSurfaceMu.RLock()
	defer writeSurfaceMu.RUnlock()
	if len(cfg.WriteSurface.Allowlist) == 0 {
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

// Bounds on an allowlist edited through the dashboard (#9587 phase 2). They
// keep one PUT from writing an unbounded config; a real hive has a handful of
// lanes, each with at most every known operation.
const (
	// WriteSurfaceMaxLanes caps how many lanes one allowlist may list.
	WriteSurfaceMaxLanes = 256
	// WriteSurfaceMaxLaneNameLen caps one lane (agent) name.
	WriteSurfaceMaxLaneNameLen = 64
)

// writeSurfaceLaneNamePattern is the shape of a lane name the editor accepts:
// an agent or replica name ("scanner", "scanner-2", "ci.fixer"). It keeps
// markup, whitespace and path characters out of hive.yaml keys.
var writeSurfaceLaneNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// NormalizeWriteSurfaceAllowlist validates an allowlist submitted by an
// operator and returns it in canonical form, or an error naming the first
// problem. Unlike the boot-time check (WriteSurfaceWarnings), an unknown
// operation is an ERROR here: the editor can say so before anything is
// saved, instead of the entry silently denying what the operator meant to
// grant.
//
// Canonical form: lane names trimmed; operations lower-cased, de-duplicated
// and in KnownWriteOps order; a list containing "*" collapses to ["*"]; a
// lane with no operations is kept as an empty (allow-nothing) list. A nil or
// empty map normalizes to nil, which is "no allowlist, nothing restricted".
func NormalizeWriteSurfaceAllowlist(in map[string][]string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > WriteSurfaceMaxLanes {
		return nil, fmt.Errorf("write_surface.allowlist lists %d lanes; the limit is %d", len(in), WriteSurfaceMaxLanes)
	}
	rank := make(map[string]int, len(KnownWriteOps))
	for i, op := range KnownWriteOps {
		rank[op] = i
	}
	out := make(map[string][]string, len(in))
	for rawLane, ops := range in {
		lane := strings.TrimSpace(rawLane)
		if lane == "" {
			return nil, fmt.Errorf("write_surface.allowlist: a lane name is empty")
		}
		if len(lane) > WriteSurfaceMaxLaneNameLen {
			return nil, fmt.Errorf("write_surface.allowlist: lane name %q is longer than %d characters", lane, WriteSurfaceMaxLaneNameLen)
		}
		if !writeSurfaceLaneNamePattern.MatchString(lane) {
			return nil, fmt.Errorf("write_surface.allowlist: lane name %q may contain only letters, digits, '.', '_' and '-'", lane)
		}
		if _, dup := out[lane]; dup {
			return nil, fmt.Errorf("write_surface.allowlist: lane %q is listed twice", lane)
		}
		seen := map[string]bool{}
		norm := []string{}
		allowAll := false
		for _, rawOp := range ops {
			op := strings.ToLower(strings.TrimSpace(rawOp))
			if op == "" {
				continue
			}
			if op == WriteSurfaceAllowAll {
				allowAll = true
				continue
			}
			if _, known := rank[op]; !known {
				return nil, fmt.Errorf("write_surface.allowlist.%s: unknown operation %s (known: %s, or %q for all)",
					lane, strconv.Quote(rawOp), strings.Join(KnownWriteOps, ", "), WriteSurfaceAllowAll)
			}
			if !seen[op] {
				seen[op] = true
				norm = append(norm, op)
			}
		}
		if allowAll {
			norm = []string{WriteSurfaceAllowAll}
		} else {
			sort.Slice(norm, func(i, j int) bool { return rank[norm[i]] < rank[norm[j]] })
		}
		out[lane] = norm
	}
	return out, nil
}

// WriteSurfaceAllowlist returns a deep copy of the current allowlist, safe to
// hand to a caller that may read it while the dashboard replaces it.
func (c *Config) WriteSurfaceAllowlist() map[string][]string {
	if c == nil {
		return nil
	}
	writeSurfaceMu.RLock()
	defer writeSurfaceMu.RUnlock()
	return copyWriteSurfaceAllowlist(c.WriteSurface.Allowlist)
}

// SetWriteSurfaceAllowlist replaces the allowlist in memory. The new value is
// in force for the very next relay request. It takes saveMu so a concurrent
// Save never marshals a half-replaced map; the caller persists afterwards
// (the dashboard's saveConfig), exactly as with every other runtime setting.
//
// The caller must pass a value from NormalizeWriteSurfaceAllowlist. It is
// copied, so later changes to the caller's map do not reach the config.
func (c *Config) SetWriteSurfaceAllowlist(allowlist map[string][]string) {
	if c == nil {
		return
	}
	next := copyWriteSurfaceAllowlist(allowlist)
	saveMu.Lock()
	defer saveMu.Unlock()
	writeSurfaceMu.Lock()
	defer writeSurfaceMu.Unlock()
	c.WriteSurface.Allowlist = next
}

// copyWriteSurfaceAllowlist deep-copies an allowlist, keeping an empty
// (allow-nothing) list distinct from an absent lane.
func copyWriteSurfaceAllowlist(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for lane, ops := range in {
		out[lane] = append([]string{}, ops...)
	}
	return out
}
