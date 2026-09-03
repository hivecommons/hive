package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

func (h *ContributeWSHub) turnEnvelopeDirOrDefault() string {
	if h != nil && h.turnEnvelopeDir != "" {
		return h.turnEnvelopeDir
	}
	return turnEnvelopeDirPath
}

func (h *ContributeWSHub) persistTurnEnvelopeForAssignment(c *ContributorConnection, task *WSTaskAssign, gen uint64, prompt string, labels []string) string {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil || task == nil {
		return ""
	}
	cfg := h.server.deps.Config
	agentName, agentCfg := turnAgentConfig(cfg, task.Role)
	if !cfg.ReentrantTurnEnabled(agentCfg) {
		return ""
	}
	acmmLevel := 0
	if cfg.ACMMLevel != nil {
		acmmLevel = *cfg.ACMMLevel
	}
	now := time.Now().UTC()
	env := turn.SessionEnvelope{
		SessionID:        task.TaskID,
		Agent:            toolapprove.AgentIdentity{Name: agentName, Role: agentCfg.Role, ToolsConfig: agentCfg.Tools},
		ACMMLevel:        acmmLevel,
		Status:           turn.StatusActive,
		WorkingRepo:      task.Repo,
		BeadID:           task.identityKey(),
		Owner:            identityOf(c),
		Epoch:            gen,
		LeaseExpiry:      now.Add(wsTaskTimeout),
		CreatedAt:        now,
		UpdatedAt:        now,
		Variables:        turnEnvelopeVariables(task, labels),
		PendingApprovals: nil,
	}
	env.Messages = append(env.Messages,
		turn.Message{Role: turn.RoleSystem, Content: "Hive contributor task assignment envelope", Timestamp: now},
		turn.Message{Role: turn.RoleUser, Content: prompt, Timestamp: now},
	)
	if err := (turn.FileStore{Dir: h.turnEnvelopeDirOrDefault()}).Persist(context.Background(), env); err != nil {
		h.logger.Warn("[contribute-ws] failed to persist re-entrant turn envelope", "task_id", task.TaskID, "error", err)
		return ""
	}
	return task.TaskID
}

func turnAgentConfig(cfg *config.Config, role string) (string, config.AgentConfig) {
	agentName := role
	if agentName == "" {
		agentName = "contributor"
	}
	if cfg != nil {
		if ac, ok := cfg.Agents[agentName]; ok {
			return agentName, ac
		}
	}
	return agentName, config.AgentConfig{}
}

func turnEnvelopeVariables(task *WSTaskAssign, labels []string) map[string]string {
	vars := map[string]string{
		"task_id":     task.TaskID,
		"task_kind":   task.Kind,
		"task_key":    task.identityKey(),
		"repo":        task.Repo,
		"issue":       strconv.Itoa(task.Number),
		"title":       task.Title,
		"url":         task.URL,
		"source_type": task.SourceType,
		"external_id": task.ExternalID,
	}
	for i, label := range labels {
		vars[fmt.Sprintf("label_%d", i)] = label
	}
	return vars
}
