package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func (s *Server) handlePacksList(w http.ResponseWriter, r *http.Request) {
	packs := config.ACMMPacks()

	currentLevel := s.detectCurrentLevel()

	type packSummary struct {
		Level       int                `json:"level"`
		Name        string             `json:"name"`
		Description string             `json:"description"`
		AgentCount  int                `json:"agentCount"`
		Governor    config.PackGovernor `json:"governor"`
		Current     bool               `json:"current"`
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
	for _, pa := range pack.Agents {
		if existing, exists := s.deps.Config.Agents[pa.Name]; exists {
			changed := false

			// Merge pack fields as defaults — user overrides take precedence.
			// Backend/Model: use pack value only if user left it empty.
			if existing.Backend == "" && pa.Backend != "" {
				existing.Backend = pa.Backend
				changed = true
			}
			if existing.Model == "" && pa.Model != "" {
				existing.Model = pa.Model
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
			// Fill in descriptive fields from pack if user didn't set them.
			if existing.DisplayName == "" && pa.DisplayName != "" {
				existing.DisplayName = pa.DisplayName
				changed = true
			}
			if existing.Description == "" && pa.Description != "" {
				existing.Description = pa.Description
				changed = true
			}
			if existing.Role == "" && pa.Role != "" {
				existing.Role = pa.Role
				changed = true
			}
			if existing.BeadRole == "" && pa.BeadRole != "" {
				existing.BeadRole = pa.BeadRole
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
			Backend:      pa.Backend,
			Model:        pa.Model,
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
			s.logger.Error("failed to save agent from pack", "agent", pa.Name, "error", err)
			return nil, fmt.Errorf("failed to save agent %s: %w", pa.Name, err)
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
				mode.Cadences = make(map[string]string)
			}
			for agent, interval := range agentCadences {
				if isFirstApplyOrExpansion {
					mode.Cadences[agent] = interval
				} else if _, exists := mode.Cadences[agent]; !exists {
					mode.Cadences[agent] = interval
				}
			}
			s.deps.Config.Governor.Modes[modeName] = mode
		}
		for modeName, threshold := range pack.Governor.Thresholds {
			mode := s.deps.Config.Governor.Modes[modeName]
			if isFirstApplyOrExpansion || mode.Threshold == 0 {
				mode.Threshold = threshold
			}
			s.deps.Config.Governor.Modes[modeName] = mode
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
	s.logger.Info("ACMM pack applied", "level", level, "name", pack.Name, "created", len(created), "updated", len(updated), "skipped", len(skipped), "paused", len(paused), "resumed", len(resumed))

	return &ApplyPackResult{
		Name:    pack.Name,
		Created: created,
		Updated: updated,
		Skipped: skipped,
		Paused:  paused,
		Resumed: resumed,
	}, nil
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
		"ok":         true,
		"status":     "applied",
		"level":      level,
		"name":       result.Name,
		"created":    result.Created,
		"updated":    result.Updated,
		"skipped":    result.Skipped,
		"paused":     result.Paused,
		"resumed":    result.Resumed,
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
	// description, etc.) into the live config. Without this, the mode-clear
	// above leaves every agent with mode="" and the gh proxy blocks merges.
	packResult, packErr := s.ApplyPack(level)
	if packErr != nil {
		s.logger.Warn("failed to apply pack after level change", "level", level, "error", packErr)
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
				mode.Cadences = make(map[string]string)
				for agent, interval := range agentCadences {
					mode.Cadences[agent] = interval
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
