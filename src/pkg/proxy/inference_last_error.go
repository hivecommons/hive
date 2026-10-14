package proxy

import (
	"sync"
	"time"
)

// Per-agent last inference error (#11761).
//
// While an inference backend is failing, Claude Code shows the error in its
// TUI retry status line, which it clips to a single pane row and shares with a
// right-aligned "Retrying in Ns · attempt N/M" countdown. The full message is
// only visible after every retry is exhausted, and the dashboard's pane-derived
// copy (agent.classifyProviderError) inherits the same truncation. The proxy
// generated that message itself, so it records the exact text it returned to
// each agent here and the dashboard shows it in full while the CLI is retrying.

// inferenceLastError is the most recent inference error the proxy returned to
// one agent. message is the same log-safe text the CLI receives
// (inferenceBackendErrorMessage / inferenceBackendUnreachableMessage), never
// key material.
type inferenceLastError struct {
	message string
	status  int
	at      time.Time
}

// inferenceLastErrorState is a small leaf-locked map keyed by agent name:
// nothing here calls back into GitHubProxy, so its mutex is taken and released
// within one method.
type inferenceLastErrorState struct {
	mu     sync.Mutex
	byName map[string]inferenceLastError
}

func newInferenceLastErrorState() *inferenceLastErrorState {
	return &inferenceLastErrorState{byName: map[string]inferenceLastError{}}
}

// record stores msg as agentName's most recent inference error. An empty agent
// name (an unattributed request) is ignored.
func (s *inferenceLastErrorState) record(agentName, msg string, status int, now time.Time) {
	if s == nil || agentName == "" || msg == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[agentName] = inferenceLastError{message: msg, status: status, at: now}
}

// clear drops agentName's entry; called on that agent's next successful
// inference call so a recovered agent stops showing a stale error.
func (s *inferenceLastErrorState) clear(agentName string) {
	if s == nil || agentName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byName, agentName)
}

func (s *inferenceLastErrorState) get(agentName string) (inferenceLastError, bool) {
	if s == nil || agentName == "" {
		return inferenceLastError{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byName[agentName]
	return e, ok
}

// recordAgentInferenceError records the full agent-facing error text for
// agentName. status is the upstream HTTP status, or 0 for a transport failure.
func (p *GitHubProxy) recordAgentInferenceError(agentName, msg string, status int) {
	if p == nil {
		return
	}
	p.inferenceLastErrors.record(agentName, msg, status, time.Now())
}

// clearAgentInferenceError forgets agentName's last inference error after a
// successful inference call.
func (p *GitHubProxy) clearAgentInferenceError(agentName string) {
	if p == nil {
		return
	}
	p.inferenceLastErrors.clear(agentName)
}

// LastInferenceError reports the most recent inference error the proxy
// returned to agentName — the full text, not the single pane line Claude Code
// renders during its retry countdown. ok is false when the agent has no
// recorded error or its last inference call succeeded. Safe on a nil proxy.
func (p *GitHubProxy) LastInferenceError(agentName string) (msg string, status int, at time.Time, ok bool) {
	if p == nil {
		return "", 0, time.Time{}, false
	}
	e, found := p.inferenceLastErrors.get(agentName)
	if !found {
		return "", 0, time.Time{}, false
	}
	return e.message, e.status, e.at, true
}
