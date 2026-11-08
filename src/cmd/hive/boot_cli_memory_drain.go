package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

// beadSynthVaultName is the store name the bead-synth vault is connected
// and registered under; CLI-memory drafts land in the same store.
const beadSynthVaultName = "bead-synth-wiki"

// cliMemorySpoolDir keeps a copy of memory files whose facts could not all
// be stored, per agent, so clear_on_kick does not lose them; the drainer
// retries them on the agent's next drain (#17056). A var so tests can point
// it at a temp dir.
var cliMemorySpoolDir = "/data/knowledge/auto-memory-spool"

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
	drainer := &climemory.Drainer{Dir: vaultPath, Store: b.knowledgeAPI.GetVaultStore(vaultPath), Layer: layer, SpoolDir: cliMemorySpoolDir}
	cfg := b.cfg
	host.SetCLIMemoryDrainer(func(req crux.CLIMemoryDrainRequest) crux.CLIMemoryDrainResult {
		sources := make([]climemory.Source, 0, len(req.Sources))
		for _, s := range req.Sources {
			sources = append(sources, climemory.Source{CLI: s.CLI, Path: s.Path})
		}
		res := drainer.Drain(climemory.Request{Agent: req.Agent, Repos: cfg.ReposForAgent(req.Agent), Sources: sources})
		out := crux.CLIMemoryDrainResult{
			Store:   beadSynthVaultName,
			Drained: res.Drained,
			Dupes:   res.Dupes,
			Skipped: res.Skipped,
			Spooled: res.Spooled,
			Err:     errors.Join(res.Errors...),
		}
		// Only after a clean drain, and only files whose every memhash is
		// now in the vault (hivecommons/hive#17208).
		if req.Truncate && out.Err == nil {
			truncated, kept, err := drainer.TruncateDrained(sources)
			out.Truncated, out.Kept, out.Err = len(truncated), len(kept), err
		}
		return out
	})
	b.logger.Info("CLI memory drain installed", "store", beadSynthVaultName, "path", vaultPath, "layer", layer)
}

// startCLIMemoryDrainRequests serves on-demand drains to agents
// (hivecommons/hive#17208). A Claude hook runs as the agent's own UID and can
// neither write the hive-owned vault nor reach the in-process drainer, so it
// drops a request into github.MemoryDrainRequestDir like every other
// agent-to-hive relay. The owner of the request file is the agent's identity:
// the runtime refuses a request naming any other agent, and each handled
// request is audited as cli_memory_drain_requested. Started only when some
// agent drains (the hook is seeded only then); a request from an agent with
// the drain off is answered as a no-op.
func (b *boot) startCLIMemoryDrainRequests() {
	if !b.cliMemoryDrainEnabled() || b.ctx == nil {
		return
	}
	host, ok := b.agentMgr.(crux.CLIMemoryDrainNowHost)
	if !ok {
		return
	}
	var audit agentaudit.AuditSink
	if b.dashSrv != nil {
		audit = b.dashSrv.AgentAuditSink()
	}
	startMemoryDrainRequestWatcher(b.ctx, b.logger, cliMemoryDrainRequestHandler(host, audit))
}

// startMemoryDrainRequestWatcher is a seam so boot tests do not poll the
// real request directory.
var startMemoryDrainRequestWatcher = github.StartMemoryDrainRequestWatcher

// auditCLIMemoryDrainRequested is the audit action of one on-demand drain.
const auditCLIMemoryDrainRequested = "cli_memory_drain_requested"

// cliMemoryDrainRequestHandler runs one dropped request through the runtime
// and records it in the audit log, refused or not.
func cliMemoryDrainRequestHandler(host crux.CLIMemoryDrainNowHost, audit agentaudit.AuditSink) github.MemoryDrainHandler {
	return func(req github.MemoryDrainRequest, fileUID int) github.MemoryDrainResponse {
		res, ran, err := host.DrainCLIMemoryNow(crux.CLIMemoryDrainNowRequest{
			Agent: req.Agent, CallerUID: fileUID, Cwd: req.Cwd, Truncate: req.Truncate, OnWrite: req.OnWrite,
		})
		resp := github.MemoryDrainResponse{
			OK: err == nil, Agent: req.Agent, Ran: ran, Store: res.Store,
			Drained: res.Drained, Dupes: res.Dupes, Skipped: res.Skipped, Spooled: res.Spooled,
			Truncated: res.Truncated, Kept: res.Kept,
		}
		outcome := "drained"
		switch {
		case err != nil:
			outcome, resp.Error = "refused", err.Error()
		case !ran:
			outcome = "noop"
		case res.Err != nil:
			outcome, resp.Error = "partial", res.Err.Error()
		}
		if audit != nil {
			audit.Record(req.Agent, auditCLIMemoryDrainRequested, req.Agent, agentaudit.Fields(
				"outcome", outcome,
				"uid", strconv.Itoa(fileUID),
				"cwd", req.Cwd,
				"truncate", strconv.FormatBool(req.Truncate),
				"on_write", strconv.FormatBool(req.OnWrite),
				"drained", strconv.Itoa(res.Drained),
				"dupes", strconv.Itoa(res.Dupes),
				"truncated", strconv.Itoa(res.Truncated),
				"kept", strconv.Itoa(res.Kept),
				"error", resp.Error,
			))
		}
		return resp
	}
}
