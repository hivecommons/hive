package dashboard

import (
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/worksource"
)

const integrationDispatchObservationLimit = 128

type integrationDispatchObservation struct {
	Key        string               `json:"key"`
	Repo       string               `json:"repo"`
	Base       string               `json:"base"`
	Mode       string               `json:"mode"`
	ObservedAt time.Time            `json:"observed_at"`
	Decision   integration.Decision `json:"decision"`
}

func (s *Server) rememberIntegrationDispatch(ref worksource.Ref, base, mode string, decision integration.Decision) {
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	if s.integration.advice == nil {
		s.integration.advice = make(map[string]integrationDispatchObservation)
	}
	key := ref.Key()
	if _, exists := s.integration.advice[key]; !exists && len(s.integration.advice) >= integrationDispatchObservationLimit {
		oldest := ""
		for candidate, observation := range s.integration.advice {
			previous := s.integration.advice[oldest]
			if oldest == "" || observation.ObservedAt.Before(previous.ObservedAt) ||
				observation.ObservedAt.Equal(previous.ObservedAt) && candidate < oldest {
				oldest = candidate
			}
		}
		delete(s.integration.advice, oldest)
	}
	decision.Hints = append([]string(nil), decision.Hints...)
	if decision.Cost.RunnerMinutes != nil {
		minutes := *decision.Cost.RunnerMinutes
		decision.Cost.RunnerMinutes = &minutes
	}
	s.integration.advice[key] = integrationDispatchObservation{Key: key, Repo: ref.Repo, Base: base, Mode: mode, ObservedAt: time.Now(), Decision: decision}
}

func (s *Server) integrationDispatchAdvice() []integrationDispatchObservation {
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	out := make([]integrationDispatchObservation, 0, len(s.integration.advice))
	for _, observation := range s.integration.advice {
		observation.Decision.Hints = append([]string(nil), observation.Decision.Hints...)
		if observation.Decision.Cost.RunnerMinutes != nil {
			minutes := *observation.Decision.Cost.RunnerMinutes
			observation.Decision.Cost.RunnerMinutes = &minutes
		}
		out = append(out, observation)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out
}
