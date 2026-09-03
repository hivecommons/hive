package scheduler

import "github.com/hivecommons/hive/pkg/ioscan"

func (s *Scheduler) canariesEnabled() bool {
	return s.cfg != nil && s.cfg.Ioscan.IsEnabled() && s.cfg.Ioscan.Canaries
}

func (s *Scheduler) addCanaryPreamble(agentName, msg string) string {
	if !s.canariesEnabled() || msg == "" {
		return msg
	}
	c, err := ioscan.DefaultCanaries.Add(agentName)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("ioscan canary generation failed", "agent", agentName, "error", err)
		}
		return msg
	}
	return ioscan.CanaryPreamble(c.Token) + msg
}
