package agent

import (
	"fmt"
	"path/filepath"
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
func cliMemoryDrainSources(agent *AgentProcess, backend string) []CLIMemorySource {
	var sources []CLIMemorySource
	for _, p := range cliMemoryStorePaths(agent, backend) {
		cli := cliMemoryCodex
		switch {
		case filepath.Base(p) == geminiMemoryFileName:
			cli = cliMemoryGemini
		case filepath.Base(p) == "memory":
			cli = cliMemoryClaude
		}
		sources = append(sources, CLIMemorySource{CLI: cli, Path: p})
	}
	return sources
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
