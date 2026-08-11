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
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil, err
	}

	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir == "" {
		return nil, fmt.Errorf("agents_dir not configured")
	}

	var created []string
	var skipped []string

	var updated []string
	// Collect per-agent create failures instead of aborting the whole apply on
	// the first one. Aborting mid-loop leaves a partially reconciled roster
	// (some of the level's agents added, the rest missing) that no longer
	// matches acmm_level — the exact "level says N, roster says fewer" drift we
	// are fixing. We reconcile every agent we can, then surface a combined
	// error so the caller does NOT record the level as cleanly applied.
	var createErrs []string
	// tombstoned collects pack agents skipped because the operator deleted
	// them. Reported back so the caller (and the apply-pack response) can say
	// so out loud instead of silently under-delivering the level's roster.
	var tombstoned []string
	for _, pa := range pack.Agents {
		// A deliberately deleted agent is NOT re-created, at any level. The
		// pack listing it is exactly why deletion did not stick: ApplyPack
		// runs on every restart, so the pack re-added `brainstorm`/`guide` to
		// a hive whose operator had removed them, four times over.
		if s.deps.Config.IsAgentRemoved(pa.Name) {
			tombstoned = append(tombstoned, pa.Name)
			continue
		}
		if existing, exists := s.deps.Config.Agents[pa.Name]; exists {
			changed := false

			// Pack-behavior fields define what an agent DOES at a given ACMM
			// level (which kick template it runs, its issue/PR mode, which model,
			// its role/description). These MUST be reconciled to the current pack
			// on every level change — otherwise a value written by a PREVIOUS
			// level's pack is indistinguishable from a user override and sticks
			// forever, so the agent keeps behaving at its old level. That is the
			// exact drift observed in the field: hives that climbed levels kept
			// scanner/ci-maintainer/quality on their lower-level advisory
			// templates and models even though acmm_level had moved up. Reconcile
			// these replace-on-diff (kick_template and mode already did; model,
			// description, role, bead_role, display_name did NOT — that gap is the
			// bug).
			//
			// Model/Backend are the exception: an operator sets them from the
			// Governor grid, so they carry an ownership marker and are reconciled
			// only while still pack-owned (see below). The in-memory
			// ModelOverride/BackendOverride on the agent process are NOT a
			// durable home for that choice — they are replayed from
			// /data/hive-state.json, while hive.yaml (which this writes) is what
			// the pack re-reads on the next restart.
			if pa.KickTemplate != "" && existing.KickTemplate != pa.KickTemplate {
				existing.KickTemplate = pa.KickTemplate
				changed = true
			}
			if pa.Mode != "" && existing.Mode != pa.Mode {
				existing.Mode = pa.Mode
				changed = true
			}
			// Model is reconciled to the pack ONLY while the pack still owns
			// it. Once an operator picks a model in the Governor grid the
			// field becomes operator-owned and the pack must leave it alone:
			// ApplyPack runs on every restart ("merging pack updates"), so an
			// unconditional replace-on-diff here silently reverted the
			// operator's choice on the next pod restart — repeatedly, which is
			// exactly the reported "they always come back".
			if pa.Model != "" && existing.Model != pa.Model && !existing.ModelIsOperatorOwned() {
				existing.Model = pa.Model
				existing.ModelOwner = config.FieldOwnerPack
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

			// Backend is fill-if-empty: it never varies by level (always the same
			// per agent across all packs), and users legitimately pin it, so the
			// pack must not stomp a user's choice.
			if existing.Backend == "" && pa.Backend != "" && !existing.BackendIsOperatorOwned() {
				existing.Backend = pa.Backend
				existing.BackendOwner = config.FieldOwnerPack
				changed = true
			}

			// Respect user's enabled: false — don't override it.
			if changed {
				s.deps.Config.Agents[pa.Name] = existing
				_ = s.deps.AgentMgr.UpdateConfig(pa.Name, existing)
				updated = append(updated, pa.Name)
			} else {
				skipped = append(skipped, pa.Name)
			}
			continue
		}

		includeRepos := pa.IncludeRepos
		agentCfg := config.AgentConfig{
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
			Managed:      true,
		}

		if err := config.SaveAgentFile(agentsDir, pa.Name, agentCfg); err != nil {
			// Do not abort: keep reconciling the rest of the roster. The agent
			// is still added to the in-memory config below so it appears in
			// /api/status and is retried on the next apply; the combined error
			// returned at the end prevents the level from being recorded as
			// cleanly applied.
			s.logger.Error("failed to save agent overlay file from pack (continuing)", "agent", pa.Name, "error", err)
			createErrs = append(createErrs, fmt.Sprintf("%s: %v", pa.Name, err))
		}

		s.deps.Config.Agents[pa.Name] = agentCfg
		s.deps.Config.ApplyAgentDefaults(pa.Name)

		finalCfg := s.deps.Config.Agents[pa.Name]
		s.deps.AgentMgr.AddAgent(pa.Name, finalCfg)
		// On-demand agents are only triggered explicitly (e.g. by inception).
		// Also skip starting agents the user explicitly disabled.
		if !pa.OnDemand && finalCfg.Enabled {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, pa.Name); err != nil {
				s.logger.Warn("failed to start agent after pack create", "agent", pa.Name, "error", err)
			}
		}

		created = append(created, pa.Name)
	}

	s.deps.Config.ACMMLevel = &level
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to save ACMM level to hive.yaml", "error", err)
	}

	// Only apply governor config (thresholds, cadences, eval interval) when
	// new agents are being created. On a pure merge (all agents already exist),
	// preserve user's governor customizations.
	isFirstApplyOrExpansion := len(created) > 0

	if pack.Governor.EvalIntervalS > 0 && isFirstApplyOrExpansion {
		s.deps.Config.Governor.EvalIntervalS = pack.Governor.EvalIntervalS
	}

	if len(pack.Governor.Cadences) > 0 || len(pack.Governor.Thresholds) > 0 {
		if s.deps.Config.Governor.Modes == nil {
			s.deps.Config.Governor.Modes = make(map[string]config.ModeConfig)
		}
		for modeName, agentCadences := range pack.Governor.Cadences {
			mode := s.deps.Config.Governor.Modes[modeName]
			if mode.Cadences == nil {
				mode.Cadences = make(map[string]config.Cadence)
			}
			for agent, interval := range agentCadences {
				if isFirstApplyOrExpansion {
					mode.Cadences[agent] = config.Cadence(interval)
				} else if _, exists := mode.Cadences[agent]; !exists {
					mode.Cadences[agent] = config.Cadence(interval)
				}
			}
			s.deps.Config.Governor.Modes[modeName] = mode
		}
		for modeName, threshold := range pack.Governor.Thresholds {
			// On first apply / level expansion, seed the pack's threshold. On a
			// pure merge, only fill a mode that has NO existing entry — never
			// overwrite an operator-set value. The old `|| mode.Threshold == 0`
			// clause broke this: threshold 0 is a legitimate operator setting
			// (e.g. a low QUIET bound), not "unset", so re-applying a pack —
			// including the apply triggered by an ACMM level bounce — reset the
			// operator's tuning.
			mode, present := s.deps.Config.Governor.Modes[modeName]
			if isFirstApplyOrExpansion || !present {
				mode.Threshold = threshold
				s.deps.Config.Governor.Modes[modeName] = mode
			}
		}
	}

	for _, pa := range pack.Agents {
		if pa.StaleTimeout > 0 {
			if ac, ok := s.deps.Config.Agents[pa.Name]; ok {
				ac.StaleTimeout = pa.StaleTimeout
				s.deps.Config.Agents[pa.Name] = ac
			}
		}
	}

	if len(created) > 0 {
		s.reInitSubsystems()
	}

	s.deps.AgentMgr.SetACMMLevel(level)
	s.deps.AgentMgr.ClearAllModeOverrides()
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
		// Return the result alongside the error so callers can still see what
		// was reconciled, but the non-nil error signals the roster is not
		// fully persisted and the level must not be reported as cleanly set.
		return result, fmt.Errorf("failed to persist %d pack agent(s): %s", len(createErrs), strings.Join(createErrs, "; "))
	}
	return result, nil
}

func (s *Server) handlePackApply(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
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
		// Empty unless the operator deleted one of this level's agents.
		"tombstoned": result.Tombstoned,
	})
}

func (s *Server) handlePackSetLevel(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
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
	s.deps.Config.ACMMLevel = &level
	// Clear per-agent mode from the persisted config so the fsnotify watcher
	// does not re-apply stale pack modes when it reloads the file. Without
	// this, Config.Save → fsnotify reload → old mode restored → governor
	// kick writes wrong mode file.
	for name, ac := range s.deps.Config.Agents {
		ac.Mode = ""
		s.deps.Config.Agents[name] = ac
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to save ACMM level to hive.yaml", "error", err)
	}

	s.deps.AgentMgr.SetACMMLevel(level)
	s.deps.AgentMgr.ClearAllModeOverrides()

	// ApplyPack cascades pack-defined agent fields (mode, kick_template,
	// description, etc.) into the live config AND reconciles the roster —
	// adding every agent the level introduces (e.g. strategist/architect at
	// L5) plus their governor cadences. Without this, the mode-clear above
	// leaves every agent with mode="" and the gh proxy blocks merges.
	//
	// If ApplyPack fails the roster is NOT reconciled to the level, so we must
	// NOT report success: acmm_level was already written above, and returning
	// 200 here is exactly what left kellyaa at "acmm_level: 5, 8 agents,
	// strategist missing". Surface the error so the operator sees the drift
	// and can retry instead of trusting a false success.
	packResult, packErr := s.ApplyPack(level)
	if packErr != nil {
		s.logger.Error("failed to reconcile roster after level change", "level", level, "error", packErr)
		jsonError(w, "level set but roster reconciliation failed: "+packErr.Error(), http.StatusInternalServerError)
		return
	}

	paused, resumed := s.syncAgentVisibility(level)
	s.deps.AgentMgr.SyncModeFiles(level)

	s.persistOnly()
	s.refreshAsync()

	pack, packLookupErr := config.ACMMPackByLevel(level)
	var packAgentNames []string
	if packLookupErr == nil {
		for _, a := range pack.Agents {
			if !a.Hidden {
				packAgentNames = append(packAgentNames, a.Name)
			}
		}
		if len(pack.Governor.Cadences) > 0 || len(pack.Governor.Thresholds) > 0 {
			if s.deps.Config.Governor.Modes == nil {
				s.deps.Config.Governor.Modes = make(map[string]config.ModeConfig)
			}
			for modeName, agentCadences := range pack.Governor.Cadences {
				mode := s.deps.Config.Governor.Modes[modeName]
				mode.Cadences = make(map[string]config.Cadence)
				for agent, interval := range agentCadences {
					mode.Cadences[agent] = config.Cadence(interval)
				}
				s.deps.Config.Governor.Modes[modeName] = mode
			}
			for modeName, threshold := range pack.Governor.Thresholds {
				mode := s.deps.Config.Governor.Modes[modeName]
				mode.Threshold = threshold
				s.deps.Config.Governor.Modes[modeName] = mode
			}
			if pack.Governor.EvalIntervalS > 0 {
				s.deps.Config.Governor.EvalIntervalS = pack.Governor.EvalIntervalS
			}
		}
	}

	if packLookupErr == nil && (len(pack.Governor.Cadences) > 0 || len(pack.Governor.Thresholds) > 0 || pack.Governor.EvalIntervalS > 0) {
		if err := s.saveConfig(); err != nil {
			s.logger.Error("failed to persist config after pack cadence update", "error", err)
		}
	}

	var packUpdated []string
	if packResult != nil {
		packUpdated = packResult.Updated
	}
	s.auditFromRequest(r, "set_acmm_level", auditDetail("level", strconv.Itoa(body.Level)), "")
	s.logger.Info("ACMM level set", "level", body.Level, "paused", len(paused), "resumed", len(resumed), "packUpdated", packUpdated)
	jsonResponse(w, map[string]interface{}{
		"ok":          true,
		"level":       body.Level,
		"packAgents":  packAgentNames,
		"packUpdated": packUpdated,
		"paused":      paused,
		"resumed":     resumed,
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
	for name := range s.deps.Config.Agents {
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
