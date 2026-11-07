package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

// beadSynthVaultName is the store name the bead-synth vault is connected
// and registered under; CLI-memory drafts land in the same store.
const beadSynthVaultName = "bead-synth-wiki"

// cliMemoryDrainEnabled reports whether any enabled agent drains its CLI
// memory into the knowledge store (agents.cli_memory.drain_to_knowledge,
// fleet-wide or per agent).
func (b *boot) cliMemoryDrainEnabled() bool {
	if b.cfg == nil {
		return false
	}
	if b.cfg.AgentsCLIMemory.DrainToKnowledge {
		return true
	}
	for _, ac := range b.cfg.EnabledAgents() {
		if ac.CLIMemory.DrainMemoryToKnowledge() {
			return true
		}
	}
	return false
}

// installCLIMemoryDrainer connects the bead-synth vault when no bead store
// did and installs the drain hook on the in-process runtime
// (hivecommons/hive#17033). Drained facts are written as `state: draft`
// entries into that vault, so the Primer (approved-only by default) never
// primes them until an operator approves each one. It runs before
// registerKnowledgeStores so the vault is primed like any other store.
func (b *boot) installCLIMemoryDrainer() {
	if !b.cliMemoryDrainEnabled() {
		return
	}
	host, ok := b.agentMgr.(crux.CLIMemoryDrainHost)
	if !ok {
		b.logger.Warn("agents.cli_memory.drain_to_knowledge needs the in-process crux runtime; the sidecar runtime cannot drain CLI memory yet",
			"runtime", fmt.Sprintf("%T", b.agentMgr))
		return
	}
	if b.knowledgeAPI == nil {
		b.logger.Warn("agents.cli_memory.drain_to_knowledge is set but the knowledge API is unavailable; CLI memory will not be drained")
		return
	}
	vaultPath := b.beadSynthVaultPath()
	if err := os.MkdirAll(vaultPath, 0o755); err != nil {
		b.logger.Warn("failed to create vault for CLI memory drain", "path", vaultPath, "error", err)
		return
	}
	if b.knowledgeAPI.GetVaultStore(vaultPath) == nil {
		if err := b.knowledgeAPI.ConnectVault(vaultPath, beadSynthVaultName); err != nil {
			b.logger.Warn("failed to connect vault for CLI memory drain", "path", vaultPath, "error", err)
			return
		}
	}
	layer := knowledge.LayerType(b.cfg.Knowledge.BeadSynthesizer.TargetLayer)
	if layer == "" {
		layer = knowledge.LayerPersonal
	}
	drainer := &climemory.Drainer{Dir: vaultPath, Store: b.knowledgeAPI.GetVaultStore(vaultPath), Layer: layer}
	cfg := b.cfg
	host.SetCLIMemoryDrainer(func(req crux.CLIMemoryDrainRequest) crux.CLIMemoryDrainResult {
		sources := make([]climemory.Source, 0, len(req.Sources))
		for _, s := range req.Sources {
			sources = append(sources, climemory.Source{CLI: s.CLI, Path: s.Path})
		}
		res := drainer.Drain(climemory.Request{Agent: req.Agent, Repos: cfg.ReposForAgent(req.Agent), Sources: sources})
		return crux.CLIMemoryDrainResult{
			Store:   beadSynthVaultName,
			Drained: res.Drained,
			Dupes:   res.Dupes,
			Skipped: res.Skipped,
			Err:     errors.Join(res.Errors...),
		}
	})
	b.logger.Info("CLI memory drain installed", "store", beadSynthVaultName, "path", vaultPath, "layer", layer)
}
