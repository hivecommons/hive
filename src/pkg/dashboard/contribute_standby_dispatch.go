package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
	"github.com/hivecommons/hive/pkg/worksource"
)

const taskUnavailableModeCeiling = "mode_ceiling"

type standbyDispatchCandidate struct {
	conn  *ContributorConnection
	state StandbyConnectionState
	tier  standbypkg.Tier
}

func (s *Server) handleContributeStandbyDispatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "contribute hub not ready", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Lane string `json:"lane"`
		Key  string `json:"key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := jsonDecode(r.Body, &body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	lane := strings.ToLower(strings.TrimSpace(body.Lane))
	key := strings.TrimSpace(body.Key)
	msg, err := s.contributeHub.DispatchStandby(lane, key)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), taskUnavailableModeCeiling) {
			status = http.StatusForbidden
		}
		jsonError(w, err.Error(), status)
		return
	}
	s.auditFromRequest(r, "contribute_standby_dispatch", auditDetail("lane", msg.StandbyLane, "task", msg.TaskID), "")
	jsonResponse(w, map[string]any{
		"ok":           true,
		"task_id":      msg.TaskID,
		"standby_lane": msg.StandbyLane,
		"standby_tier": msg.StandbyTier,
		"repo":         msg.Repo,
		"number":       msg.Number,
	})
}

func jsonDecode(r interface{ Read([]byte) (int, error) }, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func (h *ContributeWSHub) DispatchStandby(lane, key string) (*WSMessage, error) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return nil, fmt.Errorf("standby dispatch unavailable")
	}
	cfg := h.server.deps.Config
	item, err := h.standbyDispatchItem(lane, key)
	if err != nil {
		return nil, err
	}
	if lane == "" {
		lane = strings.ToLower(strings.TrimSpace(item.Lane))
	}
	if lane == "" {
		return nil, fmt.Errorf("standby lane required")
	}
	if reason := standbyModeCeilingReason(cfg, lane); reason != "" {
		return nil, fmt.Errorf("%s: %s", taskUnavailableModeCeiling, reason)
	}
	itemTiers := standbyItemTiersFromConfig(cfg.Hub.StandbyItemTiers)
	standbyItem := standbypkg.Item{Repo: item.Repo, Labels: item.Labels}
	itemMatch := itemTiers.Match(standbyItem, standbyProposeItemTier(standbyItem))
	cand, err := h.pickStandbyCandidate(cfg, lane, itemMatch)
	if err != nil {
		return nil, err
	}
	return h.assignStandbyTask(cand.conn, item, lane, cand.tier, cand.state)
}

func standbyModeCeilingReason(cfg *config.Config, lane string) string {
	if cfg == nil {
		return "missing config"
	}
	acmm := cfg.ACMMLevelOrZero()
	agentCfg := cfg.Agents[lane]
	mode := agent.DefaultAgentMode(lane, acmm)
	if parsed, ok := agent.ParseAgentMode(strings.TrimSpace(agentCfg.Mode)); ok && strings.TrimSpace(agentCfg.Mode) != "" {
		mode = parsed
	}
	if !mode.CanCreatePRs() {
		return "lane " + lane + " mode " + mode.String() + " cannot create PRs"
	}
	return ""
}

func (h *ContributeWSHub) standbyDispatchItem(lane, key string) (ReadyQueueItem, error) {
	snap := h.admissionQueueSnapshot(readyQueueDefaultLimit, withheldNone)
	for _, it := range snap.queue {
		if key != "" && it.identityKey() != key {
			continue
		}
		if lane != "" && !strings.EqualFold(strings.TrimSpace(it.Lane), lane) {
			continue
		}
		return it, nil
	}
	if key != "" {
		return ReadyQueueItem{}, fmt.Errorf("standby item %q is not dispatchable", key)
	}
	if lane != "" {
		return ReadyQueueItem{}, fmt.Errorf("no dispatchable standby item in lane %q", lane)
	}
	return ReadyQueueItem{}, fmt.Errorf("no dispatchable standby item")
}

func (h *ContributeWSHub) pickStandbyCandidate(cfg *config.Config, lane string, itemMatch standbypkg.ItemMatch) (standbyDispatchCandidate, error) {
	agentCfg := cfg.Agents[lane]
	policy := standbypkg.LanePolicy{
		Floor:    standbypkg.NormalizeTier(agentCfg.Standby.StandbyFloor()),
		DailyCap: agentCfg.Standby.StandbyDailyCap(),
	}
	tiers := standbyTierMapFromConfig(cfg.Hub.StandbyModelTiers)
	now := time.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, conn := range h.connections {
		if conn == nil || conn.profile == nil {
			continue
		}
		conn.mu.Lock()
		if conn.currentTask != nil {
			conn.mu.Unlock()
			continue
		}
		state, ok := conn.standby[lane]
		if !ok {
			state, ok = conn.standby["all"]
		}
		conn.mu.Unlock()
		if !ok {
			continue
		}
		candidateConfig := standbyConfigFromState(state)
		suspended, _ := h.standbySuspended(standbyOutcomeKey(conn.profile.GitHubUsername, candidateConfig))
		c := standbypkg.Candidate{
			Contributor: conn.profile.GitHubUsername,
			Approved:    cfg.Hub.IsStandbyContributorApproved(conn.profile.GitHubUsername),
			Suspended:   suspended,
			Dispatches:  h.standbyDispatchWindow(conn.profile.GitHubUsername, lane, now),
			Config:      candidateConfig,
		}
		if ok, reason := standbypkg.Qualifies(c, policy, itemMatch, tiers, now); ok {
			return standbyDispatchCandidate{conn: conn, state: state, tier: tiers.Tier(c.Config)}, nil
		} else {
			h.logger.Info("[contribute-ws] standby candidate did not qualify", "lane", lane, "username", conn.profile.GitHubUsername, "reason", reason)
		}
	}
	return standbyDispatchCandidate{}, fmt.Errorf("no qualified standby contributor for lane %q", lane)
}

func (h *ContributeWSHub) assignStandbyTask(c *ContributorConnection, item ReadyQueueItem, lane string, tier standbypkg.Tier, state StandbyConnectionState) (*WSMessage, error) {
	if c == nil || c.profile == nil {
		return nil, fmt.Errorf("standby contributor unavailable")
	}
	h.selectMu.Lock()
	c.mu.Lock()
	if c.currentTask != nil {
		c.mu.Unlock()
		h.selectMu.Unlock()
		return nil, fmt.Errorf("standby contributor already has a task")
	}
	ref := worksource.Ref{Repo: item.Repo, Number: item.Number, SourceType: item.SourceType, ExternalID: item.ExternalID, URL: item.URL}
	taskID := fmt.Sprintf("ct-%s-%s-%d", item.Repo, taskIDSegment(ref), time.Now().Unix())
	gen := h.nextTaskGen()
	assignedAt := time.Now()
	assignment := &WSTaskAssign{
		TaskID:      taskID,
		Kind:        "issue",
		Role:        lane,
		Repo:        item.Repo,
		Number:      item.Number,
		Title:       item.Title,
		Key:         item.identityKey(),
		SourceType:  item.SourceType,
		ExternalID:  item.ExternalID,
		URL:         item.URL,
		Complexity:  taskComplexityFromLabels(item.Labels),
		StandbyLane: lane,
		StandbyTier: tier.String(),
	}
	c.currentTask = assignment
	c.currentTaskGen = gen
	c.lastLeaseRenew = assignedAt
	c.taskAssignedAt = assignedAt
	c.currentLabels = item.Labels
	c.lastIdleReason = ""
	c.currentPrompt = ""
	c.pendingToken = ""
	c.credentialDelivered = false
	c.tokenMintedAt = time.Time{}
	c.mu.Unlock()

	h.recordLeaseForKey(identityOf(c), taskID, item.Repo, item.Number, item.identityKey(), c.profile.TrustTier, gen, assignedAt)
	h.recordAssignment(identityOf(c), assignedAt)
	h.selectMu.Unlock()

	ghToken, err := h.mintScopedToken(c.profile.TrustTier, item.Repo)
	if err != nil {
		h.rollbackAssignment(c, taskID)
		return nil, fmt.Errorf("standby token mint failed: %w", err)
	}
	canPush := h.contributorCanPush(item.Repo, c.profile.GitHubUsername)
	prompt := buildTaskPromptForContributor(ref, item.Title, canPush, h.writingGuideSection())
	if policy := strings.TrimSpace(h.roleKickPrompt(lane)); policy != "" {
		prompt = buildStandbyTaskPrompt(prompt, lane, policy)
	} else {
		prompt = buildStandbyTaskPrompt(prompt, lane, "")
	}
	prompt += attributionPromptInstruction(promptInvocationMeta(c))
	c.mu.Lock()
	if c.currentTask == nil || c.currentTask.TaskID != taskID {
		c.mu.Unlock()
		return nil, fmt.Errorf("standby claim released before dispatch")
	}
	c.currentPrompt = prompt
	c.pendingToken = ghToken
	c.credentialDelivered = false
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()

	turnEnvelopeID := h.persistTurnEnvelopeForAssignment(c, assignment, gen, prompt, item.Labels)
	mcp := h.mintTaskMCPForAssignment(identityOf(c), assignment, assignedAt.Add(leaseTTL), c.profile.GitHubUsername)
	msg := &WSMessage{
		Type:           "task_assign",
		Seq:            h.nextSeq(),
		TaskID:         taskID,
		TaskGen:        gen,
		Kind:           "issue",
		Role:           lane,
		Repo:           item.Repo,
		Number:         item.Number,
		Title:          item.Title,
		URL:            item.URL,
		TaskKey:        item.identityKey(),
		SourceType:     item.SourceType,
		ExternalID:     item.ExternalID,
		Complexity:     taskComplexityFromLabels(item.Labels),
		MCP:            mcp,
		Prompt:         prompt,
		Labels:         item.Labels,
		ContribLabels:  []string{"contributor/" + c.profile.GitHubUsername},
		TurnEnvelopeID: turnEnvelopeID,
		StandbyLane:    lane,
		StandbyTier:    tier.String(),
	}
	if err := c.send(*msg); err != nil {
		h.rollbackAssignment(c, taskID)
		return nil, err
	}
	h.recordStandbyDispatch(c.profile.GitHubUsername, lane, assignedAt)
	cfg := standbyConfigFromConnection(c)
	h.appendStandbyOutcome(standbyOutcomeRecord{
		Key:          standbyOutcomeKey(c.profile.GitHubUsername, cfg),
		Lane:         lane,
		Repo:         item.Repo,
		Number:       item.Number,
		DispatchedAt: assignedAt.UTC(),
		Kind:         standbypkg.OutcomeOpen,
	})
	h.addActivity(c.profile.GitHubUsername, "standby dispatched", c.role, state.CLIBackend, state.Model, state.ReasoningEffort, taskDescOf(assignment), c.advisor())
	return msg, nil
}

func (h *ContributeWSHub) applyDonatedHold(prURL string) error {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	ref, err := ghpkg.ParsePRURL(prURL)
	if err != nil {
		return err
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return h.server.deps.GHClient.AddLabels(ctx, ref.FullName(), ref.Number, []string{"hold"})
}
