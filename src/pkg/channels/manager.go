package channels

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
)

// TriggerContext carries metadata about what triggered a kick.
type TriggerContext struct {
	ChannelType string
	Event       string
	Payload     map[string]string
}

// KickFunc sends a kick message to an agent.
type KickFunc func(agent, message string) error

// BuildMsgFunc builds the kick message for a channel trigger.
type BuildMsgFunc func(agent string, ctx TriggerContext) string

// Manager owns all non-kick channel receivers (webhook, cron, bead).
type Manager struct {
	mu       sync.RWMutex
	agents   map[string]config.AgentConfig
	webhook  *WebhookReceiver
	cron     *CronScheduler
	bead     *BeadWatcher
	kickFunc KickFunc
	buildMsg BuildMsgFunc
	logger   *slog.Logger
}

// NewManager creates a new channel manager.
func NewManager(agents map[string]config.AgentConfig, kickFunc KickFunc, buildMsg BuildMsgFunc, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		agents:   agents,
		kickFunc: kickFunc,
		buildMsg: buildMsg,
		logger:   logger,
	}
	m.webhook = NewWebhookReceiver(m)
	m.cron = NewCronScheduler(m)
	m.bead = NewBeadWatcher(m)
	return m
}

// Start begins all channel receivers.
func (m *Manager) Start(ctx context.Context) {
	m.cron.Start(ctx)
	m.bead.Start(ctx)
	m.logger.Info("channels: manager started")
}

// Stop shuts down all channel receivers.
func (m *Manager) Stop() {
	m.cron.Stop()
	m.bead.Stop()
	m.logger.Info("channels: manager stopped")
}

// UpdateConfig hot-reloads agent configs for all receivers.
func (m *Manager) UpdateConfig(agents map[string]config.AgentConfig) {
	m.mu.Lock()
	m.agents = agents
	m.mu.Unlock()
	m.cron.Rebuild(agents)
	m.bead.Rebuild(agents)
	m.logger.Info("channels: config updated", "agents", len(agents))
}

// WebhookHandler returns the HTTP handler for webhook events.
func (m *Manager) WebhookHandler() *WebhookReceiver {
	return m.webhook
}

func (m *Manager) getAgents() map[string]config.AgentConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]config.AgentConfig, len(m.agents))
	for k, v := range m.agents {
		result[k] = v
	}
	return result
}

func (m *Manager) triggerKick(agentName string, ctx TriggerContext) {
	msg := ""
	if m.buildMsg != nil {
		msg = m.buildMsg(agentName, ctx)
	}
	if err := m.kickFunc(agentName, msg); err != nil {
		m.logger.Warn("channels: kick failed", "agent", agentName, "channel", ctx.ChannelType, "error", err)
	} else {
		m.logger.Info("channels: kicked agent", "agent", agentName, "channel", ctx.ChannelType, "event", ctx.Event)
	}
}
