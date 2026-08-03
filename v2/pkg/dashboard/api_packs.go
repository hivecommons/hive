package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func (s *Server) handlePacksList(w http.ResponseWriter, r *http.Request) {
	packs := config.ACMMPacks()

	currentLevel := s.detectCurrentLevel()

	type packSummary struct {
		Level       int                 `json:"level"`
		Name        string              `json:"name"`
		Description string              `json:"description"`
		AgentCount  int                 `json:"agentCount"`
		Governor    config.PackGovernor `json:"governor"`
		Current     bool                `json:"current"`
		Agents      []config.PackAgent  `json:"agents"`
	}

	result := make([]packSummary, 0, len(packs))
	for _, p := range packs {
		result = append(result, packSummary{
			Level:       p.Level,
			Name:        p.Name,
			Description: p.Description,
			AgentCount:  len(p.Agents),
			Governor:    p.Governor,
			Current:     p.Level == currentLevel,
			Agents:      p.Agents,
		})
	}
	jsonResponse(w, result)
}

// ApplyPackResult holds the outcome of applying an ACMM pack.
type ApplyPackResult struct {
	Name    string   `json:"name"`
	Created []string `json:"created"`
	Updated []string `json:"updated"`
	Skipped []string `json:"skipped"`
	Paused  []string `json:"paused"`
	Resumed []string `json:"resumed"`
	// Tombstoned lists pack agents deliberately deleted by the operator and
	// therefore NOT re-created. Surfaced so an under-full roster reads as an
	// honored choice rather than an apply that quietly dropped agents.
	Tombstoned []string `json:"tombstoned,omitempty"`
}

// ApplyPack applies the ACMM pack for the given level. It creates agents,
// sets governor config (eval interval, cadences, thresholds, stale timeouts),
// syncs agent visibility, and persists state. Callable from both the HTTP
// handler and the startup bootstrap path.
func (s *Server) ApplyPack(level int) (*ApplyPackResult, error) {
	return s.applyPack(level, false)
}

func (s *Server) applyPack(level int, forceLevel bool) (*ApplyPackResult, error) {
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil, err
	}

	current := s.configSnapshot()
	if current == nil {
		return nil, fmt.Errorf("runtime config is not configured")
	}
	agentsDir := current.Data.AgentsDir
	if agentsDir == "" {
		return nil, fmt.Errorf("agents_dir not configured")
	}

	var created []string
	var skipped []string
	// tombstoned collects pack agents skipped because the operator deleted
	// them. Reported back so the caller can say so out loud instead of
	// silently under-delivering the level's roster.
	var tombstoned []string
	var updated []string
	var createErrs []string
	if err := s.mutateConfig(func(candidate *config.Config) error {
		if forceLevel {
			for name, agentConfig := range candidate.Agents {
				agentConfig.Mode = ""
				candidate.Agents[name] = agentConfig
			}
		}

		for _, pa := range pack.Agents {
			// A deliberately deleted agent is NOT re-created, at any level.
			// ApplyPack runs on every restart, so without this the pack re-added
			// agents whose operator had removed them — four times over on one
			// hive.
			if s.deps.Config.IsAgentRemoved(pa.Name) {
				tombstoned = append(tombstoned, pa.Name)
				continue
			}
			if existing, exists := candidate.Agents[pa.Name]; exists {
				changed := false
				// Pack-behavior fields define what an agent DOES at a given ACMM
				// level (which kick template it runs, its issue/PR mode, which model,
				// its role/description). These MUST be reconciled to the current pack
				// on every level change — otherwise a value written by a PREVIOUS
				// level's pack is indistinguishable from a user override and sticks
				// forever, so the agent keeps behaving at its old level. That is the
				// exact drift observed in the field: hives that climbed levels kept
				// scanner/ci-maintainer/quality on their lower-level advisory
				// templates and models even though acmm_level had moved up.
				//
				// Model/Backend are the exception: reconcile them as a complete
				// runtime pair only while the pack still owns them. Explicit operator
				// ownership and the legacy CLI pin remain authoritative.
				//
				// Pack reconciliation selects a complete runtime pair for an
				// unpinned agent. Keeping a backend from the previous level while
				// replacing only its model can produce an impossible launch (for
				// example, Codex with a Copilot-only Claude model). Preserve an
				// explicitly pinned CLI, but otherwise reconcile backend and
				// model together.
				reconcileBackend := !existing.CLIPinned && !existing.BackendIsOperatorOwned()
				if reconcileBackend && pa.Backend != "" && existing.Backend != pa.Backend {
					existing.Backend = pa.Backend
					existing.BackendOwner = config.FieldOwnerPack
					changed = true
				}
				modelMatchesBackend := pa.Backend == "" || existing.Backend == "" || existing.Backend == pa.Backend
				if !existing.CLIPinned && modelMatchesBackend && pa.Model != "" && existing.Model != pa.Model && !existing.ModelIsOperatorOwned() {
					existing.Model = pa.Model
					existing.ModelOwner = config.FieldOwnerPack
					changed = true
				}
				if pa.KickTemplate != "" && existing.KickTemplate != pa.KickTemplate {
					existing.KickTemplate = pa.KickTemplate
					changed = true
				}
				if pa.Mode != "" && existing.Mode != pa.Mode {
					existing.Mode = pa.Mode
					changed = true
				}
				if pa.Description != "" && existing.Description != pa.Description {
					existing.Description = pa.Description
					changed = true
				}
				if pa.Role != "" && existing.Role != pa.Role {
					existing.Role = pa.Role
					changed = true
				}
				if pa.BeadRole != "" && existing.BeadRole != pa.BeadRole {
					existing.BeadRole = pa.BeadRole
					changed = true
				}
				if pa.DisplayName != "" && existing.DisplayName != pa.DisplayName {
					existing.DisplayName = pa.DisplayName
					changed = true
				}

				if pa.StaleTimeout > 0 && existing.StaleTimeout != pa.StaleTimeout {
					existing.StaleTimeout = pa.StaleTimeout
					changed = true
				}

				// Respect user's enabled: false — don't override it.
				if changed {
					candidate.Agents[pa.Name] = existing
					_ = s.deps.AgentMgr.UpdateConfig(pa.Name, existing)
					updated = append(updated, pa.Name)
				} else {
					skipped = append(skipped, pa.Name)
				}
				continue
			}

			includeRepos := pa.IncludeRepos
			agentConfig := config.AgentConfig{
				Backend: pa.Backend,
				Model:   pa.Model,
				// A freshly created agent's model/backend come from the pack, so
				// the pack owns them until an operator overrides in the grid.
				ModelOwner:   config.FieldOwnerPack,
				BackendOwner: config.FieldOwnerPack,
				Enabled:      true,
				DisplayName:  pa.DisplayName,
				Description:  pa.Description,
				Role:         pa.Role,
				SortOrder:    pa.SortOrder,
				Emoji:        pa.Emoji,
				Color:        pa.Color,
				BeadRole:     pa.BeadRole,
				KickTemplate: pa.KickTemplate,
				IncludeRepos: &includeRepos,
				LaneKeywords: pa.LaneKeywords,
				Mode:         pa.Mode,
				OnDemand:     pa.OnDemand,
				StaleTimeout: pa.StaleTimeout,
				Managed:      true,
			}
			if err := config.SaveAgentFile(agentsDir, pa.Name, agentConfig); err != nil {
				// Do not abort: keep reconciling the rest of the roster. The agent
				// is still added to the in-memory config below so it appears in
				// /api/status and is retried on the next apply; the combined error
				// returned at the end prevents the level from being recorded as
				// cleanly applied.
				s.logger.Error("failed to save agent overlay file from pack (continuing)", "agent", pa.Name, "error", err)
				createErrs = append(createErrs, fmt.Sprintf("%s: %v", pa.Name, err))
			}
			candidate.Agents[pa.Name] = agentConfig
			candidate.ApplyAgentDefaults(pa.Name)
			created = append(created, pa.Name)
		}

		candidate.ACMMLevel = &level
		isFirstApplyOrExpansion := len(created) > 0
		if pack.Governor.EvalIntervalS > 0 && (forceLevel || isFirstApplyOrExpansion) {
			candidate.Governor.EvalIntervalS = pack.Governor.EvalIntervalS
		}
		if candidate.Governor.Modes == nil {
			candidate.Governor.Modes = make(map[string]config.ModeConfig)
		}
		for modeName, agentCadences := range pack.Governor.Cadences {
			mode := candidate.Governor.Modes[modeName]
			if forceLevel {
				mode.Cadences = make(map[string]string, len(agentCadences))
			} else if mode.Cadences == nil {
				mode.Cadences = make(map[string]string)
			}
			for agentName, interval := range agentCadences {
				if forceLevel || isFirstApplyOrExpansion {
					mode.Cadences[agentName] = interval
				} else if _, exists := mode.Cadences[agentName]; !exists {
					mode.Cadences[agentName] = interval
				}
			}
			candidate.Governor.Modes[modeName] = mode
		}
		for modeName, threshold := range pack.Governor.Thresholds {
			// On first apply / level expansion (or a forced level set), seed the
			// pack's threshold. On a pure merge, only fill a mode that has NO
			// existing entry — never overwrite an operator-set value. The old
			// `|| mode.Threshold == 0` clause broke this: threshold 0 is a
			// legitimate operator setting (e.g. a low QUIET bound), not "unset",
			// so re-applying a pack — including the apply triggered by an ACMM
			// level bounce — reset the operator's tuning.
			mode, present := candidate.Governor.Modes[modeName]
			if forceLevel || isFirstApplyOrExpansion || !present {
				mode.Threshold = threshold
				candidate.Governor.Modes[modeName] = mode
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if len(created) > 0 || len(updated) > 0 {
		s.reInitSubsystems()
	}

	s.deps.AgentMgr.SetACMMLevel(level)
	s.deps.AgentMgr.ClearAllModeOverrides()
	committed := s.configSnapshot()
	for _, name := range created {
		agentConfig := committed.Agents[name]
		if agentConfig.Enabled && !agentConfig.OnDemand {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, name); err != nil {
				s.logger.Warn("failed to start agent after pack create", "agent", name, "error", err)
			}
		}
	}
	paused, resumed := s.syncAgentVisibility(level)
	s.deps.AgentMgr.SyncModeFiles(level)

	s.persistOnly()
	go s.refreshAsync()
	// Observability (#2439): the counts alone ("tombstoned":0) were the tell in the
	// field report, so also list WHICH agents were tombstoned (deleted by the operator
	// and NOT re-created) vs skipped (already present) — a grep by agent name against
	// this single line answers "did the pack try to re-add the agent I removed?".
	s.logger.Info("ACMM pack applied", "hive_id", s.deps.Config.HiveID, "level", level, "name", pack.Name, "created", len(created), "updated", len(updated), "skipped", len(skipped), "paused", len(paused), "resumed", len(resumed), "tombstoned", len(tombstoned), "tombstoned_agents", tombstoned, "skipped_agents", skipped)
	if len(tombstoned) > 0 {
		// Say it plainly in the log too: an operator reading "this level has 6
		// agents but I see 4" needs the reason, not a silent gap.
		s.logger.Info("ACMM pack: agents NOT re-created because the operator deleted them",
			"agents", strings.Join(tombstoned, ", "),
			"hint", "re-add the agent from the Governor grid to undo the deletion")
	}

	result := &ApplyPackResult{
		Name:       pack.Name,
		Created:    created,
		Updated:    updated,
		Skipped:    skipped,
		Paused:     paused,
		Resumed:    resumed,
		Tombstoned: tombstoned,
	}
	if len(createErrs) > 0 {
		return result, fmt.Errorf("failed to persist %d pack agent(s): %s", len(createErrs), strings.Join(createErrs, "; "))
	}
	return result, nil
}

func (s *Server) handlePackApply(w http.ResponseWriter, r *http.Request) {
	levelStr := r.PathValue("level")
	level, err := strconv.Atoi(levelStr)
	if err != nil {
		jsonError(w, "invalid level: "+levelStr, http.StatusBadRequest)
		return
	}

	s.levelMu.Lock()
	defer s.levelMu.Unlock()

	result, err := s.ApplyPack(level)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditFromRequest(r, "apply_pack", auditDetail("level", levelStr, "name", result.Name), "")
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"status":  "applied",
		"level":   level,
		"name":    result.Name,
		"created": result.Created,
		"updated": result.Updated,
		"skipped": result.Skipped,
		"paused":  result.Paused,
		"resumed": result.Resumed,
	})
}

func (s *Server) handlePackSetLevel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level int `json:"level"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "level must be an integer between 1 and 6", http.StatusBadRequest)
		return
	}

	const maxACMMLevel = 6
	if body.Level < 1 || body.Level > maxACMMLevel {
		jsonError(w, "level must be 1-6", http.StatusBadRequest)
		return
	}

	s.levelMu.Lock()
	defer s.levelMu.Unlock()

	level := body.Level
	packResult, err := s.applyPack(level, true)
	if err != nil {
		jsonError(w, "level set roster reconciliation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pack, packLookupErr := config.ACMMPackByLevel(level)
	var packAgentNames []string
	if packLookupErr == nil {
		for _, a := range pack.Agents {
			if !a.Hidden {
				packAgentNames = append(packAgentNames, a.Name)
			}
		}
	}

	var packUpdated []string
	if packResult != nil {
		packUpdated = packResult.Updated
	}
	s.auditFromRequest(r, "set_acmm_level", auditDetail("level", strconv.Itoa(body.Level)), "")
	s.logger.Info("ACMM level set", "level", body.Level, "paused", len(packResult.Paused), "resumed", len(packResult.Resumed), "packUpdated", packUpdated)
	jsonResponse(w, map[string]interface{}{
		"ok":          true,
		"level":       body.Level,
		"packAgents":  packAgentNames,
		"packUpdated": packUpdated,
		"paused":      packResult.Paused,
		"resumed":     packResult.Resumed,
	})
}

func (s *Server) syncAgentVisibility(level int) (paused, resumed []string) {
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil, nil
	}

	packAgents := make(map[string]bool, len(pack.Agents))
	onDemandAgents := make(map[string]bool, len(pack.Agents))
	for _, a := range pack.Agents {
		if !a.Hidden {
			packAgents[a.Name] = true
		}
		if a.OnDemand {
			onDemandAgents[a.Name] = true
		}
	}

	// Collect agents to resume and agents to pause.
	var toResume []string
	current := s.configSnapshot()
	for name := range current.Agents {
		if packAgents[name] {
			// On-demand agents (e.g. brainstorm) should never auto-resume;
			// they are triggered explicitly by workflows like inception.
			if onDemandAgents[name] {
				continue
			}
			if s.deps.AgentMgr.IsPaused(name) {
				proc, err := s.deps.AgentMgr.GetStatus(name)
				if err == nil && proc.PausedTrigger == "acmm-pack" {
					toResume = append(toResume, name)
				}
			}
		} else {
			// Pause sequentially — it's fast and order can matter.
			if !s.deps.AgentMgr.IsPaused(name) {
				if err := s.deps.AgentMgr.Pause(name, "acmm-pack", fmt.Sprintf("agent not in pack level %d", level)); err == nil {
					paused = append(paused, name)
				}
			}
		}
	}

	// Resume agents in parallel — each Resume() call takes ~2s,
	// so sequential resume of N agents blocks the API for N*2s.
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, name := range toResume {
		wg.Add(1)
		go func(agentName string) {
			defer wg.Done()
			if err := s.deps.AgentMgr.Resume(s.deps.Ctx, agentName, "acmm-pack", fmt.Sprintf("agent included in pack level %d", level)); err == nil {
				mu.Lock()
				resumed = append(resumed, agentName)
				mu.Unlock()
			}
		}(name)
	}
	wg.Wait()

	return paused, resumed
}

func (s *Server) detectCurrentLevel() int {
	return detectACMMLevel(s.deps.Config)
}

func detectACMMLevel(cfg *config.Config) int {
	if cfg.ACMMLevel != nil {
		return *cfg.ACMMLevel
	}
	return 1
}
