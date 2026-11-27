package proxy

import "time"

// InferenceActivity returns a detached snapshot of the latest authenticated
// inference request for each agent. Housekeeping and token-count calls do not
// count; requests count on arrival even if the route or upstream fails.
func (p *GitHubProxy) InferenceActivity() map[string]time.Time {
	p.inferenceActivityMu.RLock()
	defer p.inferenceActivityMu.RUnlock()
	out := make(map[string]time.Time, len(p.inferenceActivity))
	for agent, at := range p.inferenceActivity {
		out[agent] = at
	}
	return out
}

func (p *GitHubProxy) recordInferenceActivity(agent string) {
	p.inferenceActivityMu.Lock()
	defer p.inferenceActivityMu.Unlock()
	if p.inferenceActivity == nil {
		p.inferenceActivity = make(map[string]time.Time)
	}
	p.inferenceActivity[agent] = time.Now()
}
