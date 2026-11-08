package dashboard

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

// legacyClaudeProjectsDir is the shared home's Claude project tree that
// #17039 stopped writing to but left in place. A var so tests can point it at
// a temp dir.
var legacyClaudeProjectsDir = "/data/home/.claude/projects"

// handleKnowledgeDrainLegacyMemory serves POST
// /api/knowledge/cli-memory/drain-legacy (#17058): a one-shot, owner-only,
// audited drain of every <legacyClaudeProjectsDir>/*/memory store into draft
// entries in the bead-synth vault, tagged agent:shared. The memory files are
// emptied only with {"truncate": true}, and only those whose every fact is
// now recorded in the vault.
func (s *Server) handleKnowledgeDrainLegacyMemory(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req struct {
		Truncate bool `json:"truncate"`
	}
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}
	vault := s.deps.Knowledge.AutoMemoryVault()
	if vault == nil {
		// Spelled without the dotted hive.yaml path so the settings-parity
		// guard does not mistake this message for a Settings UI reference.
		jsonError(w, "the bead-synth-wiki vault is not connected; configure bead stores or enable the CLI-memory drain (drain_to_knowledge under the agent's cli_memory block) so drained drafts land in a vault the primer reads", http.StatusConflict)
		return
	}
	sources, err := climemory.LegacyClaudeSources(legacyClaudeProjectsDir)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	layer := knowledge.LayerPersonal
	var repos []string
	if cfg := s.deps.Config; cfg != nil {
		if l := cfg.Knowledge.BeadSynthesizer.TargetLayer; l != "" {
			layer = knowledge.LayerType(l)
		}
		repos = cfg.Project.Repos
	}
	drainer := &climemory.Drainer{Dir: vault.RootDir(), Store: vault, Layer: layer}
	res := drainer.Drain(climemory.Request{Agent: climemory.LegacyAgent, Repos: repos, Sources: sources})
	errs := append([]error(nil), res.Errors...)
	truncated, kept := []string{}, []string{}
	if req.Truncate {
		t, k, terr := drainer.TruncateDrained(sources)
		truncated, kept = append(truncated, t...), append(kept, k...)
		if terr != nil {
			errs = append(errs, terr)
		}
	}
	errText := make([]string, 0, len(errs))
	for _, e := range errs {
		errText = append(errText, e.Error())
	}

	s.auditFromRequest(r, "knowledge_drain_legacy_memory", auditDetail(
		"sources", strconv.Itoa(len(sources)),
		"drained", strconv.Itoa(res.Drained),
		"dupes", strconv.Itoa(res.Dupes),
		"skipped", strconv.Itoa(res.Skipped),
		"truncate", strconv.FormatBool(req.Truncate),
		"truncated", strconv.Itoa(len(truncated)),
		"errors", strconv.Itoa(len(errText)),
	), "")
	jsonResponse(w, map[string]interface{}{
		"store":     vault.Name(),
		"agent":     climemory.LegacyAgent,
		"sources":   len(sources),
		"drained":   res.Drained,
		"dupes":     res.Dupes,
		"skipped":   res.Skipped,
		"truncate":  req.Truncate,
		"truncated": truncated,
		"kept":      kept,
		"errors":    errText,
	})
}
