package config

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Per-repo custom agents (#6204).
//
// Hive could already vary what an agent KNOWS per repo — pkg/agentsmd injects a
// repo's AGENTS.md, skillreg resolves repo-local skills — but not which agents
// EXIST per repo. The roster was a function of the hive, so a specialist added
// for one repository woke on cadence and went looking for its concern in every
// other repository too: inference spend and issue noise on repos that never
// wanted it.
//
// AgentConfig.Repos is that missing dimension. It is deliberately agent-side
// rather than repo-side: it keeps a BYO agent a single self-contained
// declaration, which is what skillreg.AgentSpec is for, and it does not require
// project.repos to become a list of objects — the change #6111 proposes and
// which this feature must neither depend on nor pre-empt.
//
// Empty scope means every repo. That is the pre-existing behaviour and the
// right default: an agent that does not say what it is for is for everything.

// agentReposMu guards AgentConfig.Repos reads against the writers that change
// them. The scope is read from goroutines that are not the ones that write it —
// the proxy's per-request enforcement, the scheduler assembling a kick, the
// PR/merge relays — while the dashboard mutates it under saveMu.
//
// Lock order is saveMu → agentReposMu, never the reverse.
var agentReposMu sync.RWMutex

// qualifyAgentRepo returns repo as an "org/name" reference. An entry that
// already carries a slash is a deliberate cross-org reference and is returned
// unchanged — prefixing it again produces "org/org/repo", the bug every inline
// version of this join has eventually hit.
func qualifyAgentRepo(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") {
		return repo
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return repo
	}
	return org + "/" + repo
}

// agentRepoKey is the comparison key for a repo reference: org-qualified and
// case-folded, because GitHub repository names are case-insensitive. An
// operator who scopes an agent to "Console" must not find it silently excluded
// from "console".
func agentRepoKey(org, repo string) string {
	return strings.ToLower(qualifyAgentRepo(org, repo))
}

// IsRepoScoped reports whether this agent declares a repository scope. False —
// the zero value, and every agent in every config written before #6204 — means
// the agent serves the whole hive.
func (a AgentConfig) IsRepoScoped() bool {
	for _, r := range a.Repos {
		if strings.TrimSpace(r) != "" {
			return true
		}
	}
	return false
}

// ReposIsOperatorOwned reports whether an operator explicitly set this agent's
// repo scope, which makes it immune to pack reconciliation — the same contract
// ModelIsOperatorOwned provides for models and PauseIsOperatorOwned for run
// state.
//
// This marker is the specific failure mode #6204 asks to design against. ACMM
// packs reconcile agents on EVERY restart, and the ownership family
// (#5632/#5706) exists because a pack that does not know a field was chosen by
// a human silently reverts it on the next pod roll. No pack ships a repo scope
// today, so nothing reverts it today — the marker is here so that a pack which
// one day does cannot widen a specialist back to the whole hive without anyone
// noticing.
func (a AgentConfig) ReposIsOperatorOwned() bool {
	return a.ReposOwner == FieldOwnerOperator
}

// AgentServesRepo reports whether the named agent is in scope for repo. It is
// the single predicate every enforcement point asks — the MITM proxy, the
// hive-open-pr/hive-merge relays, kick assembly and the dashboard — so there is
// exactly one definition of "this agent is for this repo".
//
// An unknown agent, an unscoped agent, and an empty repo all answer true: the
// scope can only ever NARROW an agent that would otherwise serve everything,
// and a caller that cannot name a repo must not be refused on a scope it could
// not have violated.
func (c *Config) AgentServesRepo(agent, repo string) bool {
	if c == nil {
		return true
	}
	ac, ok := c.scopeSourceFor(agent)
	if !ok {
		return true
	}
	agentReposMu.RLock()
	defer agentReposMu.RUnlock()
	if !ac.IsRepoScoped() {
		return true
	}
	want := agentRepoKey(c.Project.Org, repo)
	if want == "" {
		return true
	}
	for _, scoped := range ac.Repos {
		if agentRepoKey(c.Project.Org, scoped) == want {
			return true
		}
	}
	return false
}

// scopeSourceFor resolves which AgentConfig carries the scope for a name.
//
// Replicas ("quality-2") are the same agent run more than once, so they carry
// the base agent's scope. ExpandAgentReplicas copies the whole AgentConfig, so
// a materialized replica already has it — but a hand-written overlay file that
// sets replica_of and nothing else would not, and a replica outside its base's
// scope is a hole straight through it. Falling back to the base whenever the
// replica declares no scope of its own closes that without ever WIDENING one: a
// replica that does declare a scope keeps it.
func (c *Config) scopeSourceFor(agent string) (AgentConfig, bool) {
	ac, ok := c.Agents[agent]
	if ok && ac.IsRepoScoped() {
		return ac, true
	}
	if base := c.BaseAgentName(agent); base != agent {
		if bc, bok := c.Agents[base]; bok {
			return bc, true
		}
	}
	return ac, ok
}

// ReposForAgent returns the repos this agent should be handed work on: its
// scope intersected with project.repos, in project.repos order and spelling.
// An unscoped agent gets project.repos itself, so a hive that does not use the
// feature allocates nothing and behaves exactly as before.
//
// The intersection matters. A scope naming a repo the hive does not watch must
// not conjure that repo into a kick — the agent would be told to work somewhere
// the GitHub client has never enumerated.
func (c *Config) ReposForAgent(agent string) []string {
	if c == nil {
		return nil
	}
	scope := c.AgentRepoScope(agent)
	if len(scope) == 0 {
		return c.Project.Repos
	}
	out := make([]string, 0, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		if c.AgentServesRepo(agent, repo) {
			out = append(out, repo)
		}
	}
	return out
}

// AgentRepoScope returns the agent's declared scope as written, or nil when it
// is unscoped. Unlike ReposForAgent this is NOT intersected with project.repos,
// so an operator-facing surface can show what was declared — including an entry
// that currently matches nothing, which is precisely the thing worth showing.
func (c *Config) AgentRepoScope(agent string) []string {
	if c == nil {
		return nil
	}
	ac, ok := c.scopeSourceFor(agent)
	if !ok {
		return nil
	}
	agentReposMu.RLock()
	defer agentReposMu.RUnlock()
	if !ac.IsRepoScoped() {
		return nil
	}
	out := make([]string, 0, len(ac.Repos))
	for _, r := range ac.Repos {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// PrimaryRepoForAgent is the repo an agent's prompts should treat as "the"
// repo — what $HIVE_REPO names and what the shipped templates pass to
// `gh ... --repo "$HIVE_REPO"`.
//
// For an unscoped agent this is the hive's primary repo, unchanged. For a
// SCOPED agent whose scope excludes the hive primary it is the agent's own
// first in-scope repo, because handing a specialist a $HIVE_REPO it is not
// allowed to write to is an active footgun: every template example would aim it
// at a repo the proxy then refuses.
func (c *Config) PrimaryRepoForAgent(agent string) string {
	if c == nil {
		return ""
	}
	primary := strings.TrimSpace(c.Project.PrimaryRepo)
	if primary == "" && len(c.Project.Repos) > 0 {
		primary = c.Project.Repos[0]
	}
	if primary != "" && c.AgentServesRepo(agent, primary) {
		return primary
	}
	if scoped := c.ReposForAgent(agent); len(scoped) > 0 {
		return scoped[0]
	}
	return primary
}

// RepoScopedAgents returns the names of the agents that declare a repo scope,
// sorted. Empty on every hive that does not use the feature, which is what
// makes it cheap to call from boot logging and status surfaces.
func (c *Config) RepoScopedAgents() []string {
	if c == nil {
		return nil
	}
	var out []string
	for name := range c.Agents {
		if len(c.AgentRepoScope(name)) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// AgentsForRepo returns the names of the configured agents in scope for repo,
// sorted. Unscoped agents are included: they serve every repo.
func (c *Config) AgentsForRepo(repo string) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		if c.AgentServesRepo(name, repo) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// SetAgentReposAndSave replaces one agent's repo scope and persists the config,
// mirroring SetAgentPausedAndSave: the read-modify-write and the Save happen
// under the SAME saveMu every other saver takes, so a scope change cannot be
// lost to a concurrent write of an older snapshot.
//
// Passing an empty list clears the scope, returning the agent to hive-wide. The
// operator-ownership marker is stamped either way — "this operator decided this
// agent is hive-wide" is a decision a pack must not silently overturn, exactly
// as an explicit resume claims pause ownership (#5706).
//
// Returns whether anything changed.
func (c *Config) SetAgentReposAndSave(agent string, repos []string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(agent)
	if name == "" {
		return false, fmt.Errorf("agent is required")
	}

	saveMu.Lock()
	defer saveMu.Unlock()

	ac, ok := c.Agents[name]
	if !ok {
		return false, fmt.Errorf("unknown agent %q", name)
	}

	next := make([]string, 0, len(repos))
	for _, r := range repos {
		if r = strings.TrimSpace(r); r != "" {
			next = append(next, r)
		}
	}
	if len(next) == 0 {
		next = nil
	}

	agentReposMu.Lock()
	changed := !stringSlicesEqual(ac.Repos, next) || ac.ReposOwner != FieldOwnerOperator
	if changed {
		ac.Repos = next
		ac.ReposOwner = FieldOwnerOperator
		c.Agents[name] = ac
	}
	agentReposMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// AgentRepoScopeWarnings reports scopes that cannot do what they say. Both
// cases are inert rather than dangerous, and both are silent without this:
//
//   - an entry naming a repo the hive does not watch (a typo, or a repo removed
//     from project.repos after the agent was scoped to it), and
//   - a scope whose entries ALL miss, which leaves the agent with no repos at
//     all — it would still wake on its cadence and have nowhere to work.
//
// Deliberately warnings and not validation errors: refusing to boot over a
// stale scope entry would be a worse failure than an ignored one, and keeping
// the entry means re-adding the repo restores the scope rather than silently
// widening the agent to the whole hive.
func AgentRepoScopeWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	watched := make(map[string]bool, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if key := agentRepoKey(cfg.Project.Org, repo); key != "" {
			watched[key] = true
		}
	}

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	var warnings []string
	for _, name := range names {
		scope := cfg.AgentRepoScope(name)
		if len(scope) == 0 {
			continue
		}
		matched := 0
		for _, entry := range scope {
			if watched[agentRepoKey(cfg.Project.Org, entry)] {
				matched++
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"agent %q is scoped to %q, which is not in project.repos — that entry matches nothing", name, entry))
		}
		if matched == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"agent %q is scoped to %d repo(s), none of which this hive watches — it has no repos to work", name, len(scope)))
		}
	}
	return warnings
}
