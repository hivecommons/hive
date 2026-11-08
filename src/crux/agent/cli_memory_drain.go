package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hivecommons/hive/crux"
)

// CLI memory drain (hivecommons/hive#17033). What a CLI wrote to its private
// memory store is converted into draft knowledge entries before the kick
// (and before clear_on_kick wipes the store), so a one-off inference the CLI
// kept for itself becomes a reviewable fact in the knowledge store instead
// of a hidden notebook. The conversion itself lives in the root module
// (pkg/knowledge/climemory) because the Crux module does not import the
// knowledge packages; the manager only collects the stores and hands them to
// the drainer the hive installs with SetCLIMemoryDrainer
// (crux.CLIMemoryDrainHost).

// Aliases of the crux host types so the manager and cmd/hive name the same
// hook without cmd/hive importing this package.
type (
	CLIMemorySource       = crux.CLIMemorySource
	CLIMemoryDrainRequest = crux.CLIMemoryDrainRequest
	CLIMemoryDrainResult  = crux.CLIMemoryDrainResult
	CLIMemoryDrainer      = crux.CLIMemoryDrainer

	CLIMemoryDrainNowRequest = crux.CLIMemoryDrainNowRequest
)

// cliMemoryState groups the manager's CLI-memory bookkeeping so the Manager
// struct carries one field for it.
type cliMemoryState struct {
	warnOnce sync.Once // see warnSharedCLIMemoryOnce (hivecommons/hive#17031)
	drainer  atomic.Pointer[CLIMemoryDrainer]
}

// CLI names used in CLIMemorySource; they match the climemory constants.
const (
	cliMemoryClaude = "claude"
	cliMemoryCodex  = "codex"
	cliMemoryGemini = "gemini"
)

// SetCLIMemoryDrainer installs (or with nil, removes) the drain hook. Safe
// to leave unset: agents with drain_to_knowledge then log once per kick that
// no drainer is available and the wipe proceeds as configured.
func (m *Manager) SetCLIMemoryDrainer(fn CLIMemoryDrainer) {
	if fn == nil {
		m.cliMemory.drainer.Store(nil)
		return
	}
	m.cliMemory.drainer.Store(&fn)
}

// cliMemoryDrainSources lists the agent's memory stores with the CLI that
// owns each. Same per-agent-home-only rule as cliMemoryStorePaths: a shared
// or bridged store is never drained under one agent's name.
//
// The store paths come from a Glob under the agent's home, which follows a
// symlink in any ancestor (`.claude`, `projects`, the project slug). The
// agent owns those directories and the drain runs with the hive's
// privileges, so a slug pointed at another agent's store, or at any
// hive-readable tree holding `memory/*.md`, would be drained under this
// agent's name and, with truncate, emptied. Each store is therefore resolved
// with EvalSymlinks and kept only if it still lies under the agent's own
// resolved home (or CODEX_HOME); the resolved path is what the drainer opens
// its store root at, so the listing and the open name the same directory,
// and two listings that resolve to one store are drained once.
func cliMemoryDrainSources(agent *AgentProcess, backend string) []CLIMemorySource {
	homes := []string{AgentHome(agent.Name, agent.UID, backend), codexHomePath(agent.Name)}
	for i, h := range homes {
		if r, err := filepath.EvalSymlinks(h); err == nil {
			homes[i] = r
		}
	}
	var sources []CLIMemorySource
	seen := map[string]bool{}
	for _, p := range cliMemoryStorePaths(agent, backend) {
		cli := cliMemoryCodex
		switch {
		case filepath.Base(p) == geminiMemoryFileName:
			cli = cliMemoryGemini
		case filepath.Base(p) == "memory":
			cli = cliMemoryClaude
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil || !pathUnderAny(real, homes) || seen[real] {
			continue
		}
		seen[real] = true
		sources = append(sources, CLIMemorySource{CLI: cli, Path: real})
	}
	return sources
}

// pathUnderAny reports whether path is one of, or strictly inside one of,
// the given directories. All arguments are expected to be symlink-resolved.
func pathUnderAny(path string, dirs []string) bool {
	for _, d := range dirs {
		rel, err := filepath.Rel(d, path)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// drainCLIMemoryForKick runs the installed drainer over the agent's memory
// stores when agents.cli_memory.drain_to_knowledge is set. It is called
// before clearCLIMemoryForKick so the facts survive the wipe, and it is
// best-effort: a failed drain is logged and the kick proceeds. The one log
// line per drain is
//
//	cli-memory: drained N facts from <agent> into <store> (dupes K, spooled S)
func (m *Manager) drainCLIMemoryForKick(agent *AgentProcess, backend string) {
	if !agent.Config.CLIMemory.DrainMemoryToKnowledge() {
		return
	}
	sources := cliMemoryDrainSources(agent, backend)
	if len(sources) == 0 {
		return
	}
	fn := m.cliMemory.drainer.Load()
	if fn == nil {
		m.logger.Warn("cli-memory: drain_to_knowledge is set but no knowledge store is available to drain into",
			"agent", agent.Name, "sources", len(sources))
		return
	}
	res := (*fn)(CLIMemoryDrainRequest{Agent: agent.Name, Backend: backend, Sources: sources})
	if res.Err != nil {
		m.logger.Warn("cli-memory: drain failed before kick",
			"agent", agent.Name, "store", res.Store, "drained", res.Drained, "error", res.Err)
	}
	m.logger.Info(fmt.Sprintf("cli-memory: drained %d facts from %s into %s (dupes %d, spooled %d)",
		res.Drained, agent.Name, res.Store, res.Dupes, res.Spooled),
		"agent", agent.Name, "store", res.Store, "drained", res.Drained,
		"dupes", res.Dupes, "skipped", res.Skipped, "spooled", res.Spooled, "sources", len(sources))
}

// DrainCLIMemoryNow drains req.Agent's memory stores on demand
// (hivecommons/hive#17208, crux.CLIMemoryDrainNowHost). An agent may only
// drain its own memory: when the UID map is loaded and the caller's UID is
// known, the UID must belong to req.Agent, the same forge check as the
// request relays. The sources are the ones the kick drain uses, resolved from
// the agent's own home; req.Cwd only narrows them to one Claude project.
func (m *Manager) DrainCLIMemoryNow(req CLIMemoryDrainNowRequest) (CLIMemoryDrainResult, bool, error) {
	name := strings.TrimSpace(req.Agent)
	if name == "" {
		return CLIMemoryDrainResult{}, false, errors.New("no agent named in the request")
	}
	if m.uidMap != nil && req.CallerUID > 0 {
		owner := m.uidMap.LookupByUID(req.CallerUID)
		if owner == "" {
			return CLIMemoryDrainResult{}, false, fmt.Errorf("caller uid %d is not a registered agent", req.CallerUID)
		}
		if owner != name {
			return CLIMemoryDrainResult{}, false, fmt.Errorf("request claims agent %q but comes from agent %q (uid %d); an agent may only drain its own memory", name, owner, req.CallerUID)
		}
	}
	m.mu.RLock()
	agent := m.agents[name]
	m.mu.RUnlock()
	if agent == nil {
		return CLIMemoryDrainResult{}, false, fmt.Errorf("unknown agent %q", name)
	}
	if !agent.Config.CLIMemory.DrainMemoryToKnowledge() {
		return CLIMemoryDrainResult{}, false, nil
	}
	// The PostToolUse hook lives in the one fleet-wide Claude settings file,
	// so it also fires for agents that only drain at SessionEnd.
	if req.OnWrite && !agent.Config.CLIMemory.DrainMemoryOnWrite() {
		return CLIMemoryDrainResult{}, false, nil
	}
	backend := effectiveBackend(agent)
	sources := cliMemoryDrainSources(agent, backend)
	if req.Cwd != "" {
		sources = claudeProjectSources(sources, req.Cwd)
	}
	if len(sources) == 0 {
		return CLIMemoryDrainResult{}, false, nil
	}
	fn := m.cliMemory.drainer.Load()
	if fn == nil {
		return CLIMemoryDrainResult{}, false, errors.New("drain_to_knowledge is set but no knowledge store is available to drain into")
	}
	res := (*fn)(CLIMemoryDrainRequest{Agent: name, Backend: backend, Sources: sources, Truncate: req.Truncate})
	m.logger.Info(fmt.Sprintf("cli-memory: drained %d facts from %s into %s on request (dupes %d, truncated %d)",
		res.Drained, name, res.Store, res.Dupes, res.Truncated),
		"agent", name, "store", res.Store, "drained", res.Drained, "dupes", res.Dupes,
		"skipped", res.Skipped, "spooled", res.Spooled, "truncated", res.Truncated,
		"kept", res.Kept, "sources", len(sources), "error", res.Err)
	return res, true, nil
}

// claudeSlugUnsafe matches the characters Claude Code replaces with "-" when
// it turns a working directory into its ~/.claude/projects/<slug> key.
var claudeSlugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeProjectSources keeps the Claude memory directory of the project
// Claude keys by cwd and drops every other store. The slug is only compared
// with the directories already resolved from the agent's home, so a crafted
// cwd can narrow the drain but never point it elsewhere.
func claudeProjectSources(sources []CLIMemorySource, cwd string) []CLIMemorySource {
	slug := claudeSlugUnsafe.ReplaceAllString(filepath.Clean(cwd), "-")
	var out []CLIMemorySource
	for _, s := range sources {
		if s.CLI == cliMemoryClaude && filepath.Base(filepath.Dir(s.Path)) == slug {
			out = append(out, s)
		}
	}
	return out
}
