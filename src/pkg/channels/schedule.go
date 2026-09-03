package channels

import (
	"context"
	"sync"

	"github.com/robfig/cron/v3"

	"github.com/hivecommons/hive/pkg/config"
)

// CronScheduler manages cron-based channel triggers.
type CronScheduler struct {
	mgr    *Manager
	mu     sync.Mutex
	runner *cron.Cron
}

// NewCronScheduler creates a new cron scheduler.
func NewCronScheduler(mgr *Manager) *CronScheduler {
	return &CronScheduler{mgr: mgr}
}

// Start initializes cron jobs from agent configs and begins the scheduler.
func (s *CronScheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agents := s.mgr.getAgents()
	s.runner = cron.New()
	s.registerJobs(agents)
	s.runner.Start()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()
}

// Stop halts the cron scheduler.
func (s *CronScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil {
		s.runner.Stop()
		s.runner = nil
	}
}

// Rebuild tears down and rebuilds the job list from new config.
func (s *CronScheduler) Rebuild(agents map[string]config.AgentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil {
		s.runner.Stop()
	}
	s.runner = cron.New()
	s.registerJobs(agents)
	s.runner.Start()
}

func (s *CronScheduler) registerJobs(agents map[string]config.AgentConfig) {
	for agentName, agentCfg := range agents {
		for _, ch := range agentCfg.ChannelsOfType("schedule") {
			if !ch.IsEnabled() {
				continue
			}
			name := agentName
			schedule := ch.Schedule
			_, err := s.runner.AddFunc(schedule, func() {
				ctx := TriggerContext{
					ChannelType: "schedule",
					Event:       schedule,
					Payload:     map[string]string{"schedule": schedule},
				}
				s.mgr.triggerKick(name, ctx)
			})
			if err != nil {
				s.mgr.logger.Warn("channels: invalid cron schedule", "agent", agentName, "schedule", schedule, "error", err)
			} else {
				s.mgr.logger.Info("channels: registered cron job", "agent", agentName, "schedule", schedule)
			}
		}
	}
}
