package agent

import (
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	providerErrorBackoffBaseDefault = 2 * time.Minute
	providerErrorBackoffMaxDefault  = 30 * time.Minute
	ProviderErrorBackoffBaseEnv     = "HIVE_PROVIDER_ERROR_BACKOFF_BASE"
	ProviderErrorBackoffMaxEnv      = "HIVE_PROVIDER_ERROR_BACKOFF_MAX"
)

type providerErrorMatch struct {
	Class string
	Line  string
}

var (
	providerAPIErrorStatusRe = regexp.MustCompile(`(?i)\bAPI Error:\s*(\d{3})\b`)
	providerRetryingRe       = regexp.MustCompile(`(?i)\bRetrying in \d+s\s+·\s+attempt \d+/\d+\b`)
	providerHTTPStatusRe     = regexp.MustCompile(`\b(401|403|429|500|502|503|529)\b`)
)

func (m *Manager) markProviderErrorLocked(agent *AgentProcess, match providerErrorMatch, now time.Time) time.Duration {
	if !agent.ProviderErrorBackoffUntil.IsZero() && now.Before(agent.ProviderErrorBackoffUntil) &&
		agent.ProviderErrorClass == match.Class && agent.ProviderErrorLine == match.Line {
		return agent.ProviderErrorBackoffUntil.Sub(now)
	}
	agent.providerErrorBackoffAttempt++
	delay := providerErrorBackoffDelay(agent.providerErrorBackoffAttempt)
	agent.ProviderErrorBackoffUntil = now.Add(delay)
	agent.ProviderErrorClass = match.Class
	agent.ProviderErrorLine = match.Line
	agent.LastError = match.Line
	agent.lastInferKickPane = ""
	agent.actionNudgeSent = false
	return delay
}

func (m *Manager) clearProviderErrorLocked(agent *AgentProcess) {
	if agent.ProviderErrorClass == "" && agent.ProviderErrorLine == "" && agent.ProviderErrorBackoffUntil.IsZero() {
		return
	}
	if agent.LastError == agent.ProviderErrorLine {
		agent.LastError = ""
	}
	agent.ProviderErrorClass = ""
	agent.ProviderErrorLine = ""
	agent.ProviderErrorBackoffUntil = time.Time{}
	agent.providerErrorBackoffAttempt = 0
}

func (m *Manager) providerErrorBackoffRemainingLocked(agent *AgentProcess, now time.Time) time.Duration {
	if agent == nil || agent.ProviderErrorBackoffUntil.IsZero() || !now.Before(agent.ProviderErrorBackoffUntil) {
		return 0
	}
	return agent.ProviderErrorBackoffUntil.Sub(now)
}

func (m *Manager) ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	if !ok {
		return 0, "", "", false
	}
	remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now())
	if remaining <= 0 {
		return 0, agent.ProviderErrorClass, agent.ProviderErrorLine, false
	}
	return remaining, agent.ProviderErrorClass, agent.ProviderErrorLine, true
}

func providerErrorBackoffBase() time.Duration {
	if v := os.Getenv(ProviderErrorBackoffBaseEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return providerErrorBackoffBaseDefault
}

func providerErrorBackoffMax() time.Duration {
	if v := os.Getenv(ProviderErrorBackoffMaxEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return providerErrorBackoffMaxDefault
}

func providerErrorBackoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := providerErrorBackoffBase()
	maxDelay := providerErrorBackoffMax()
	delay := base
	for i := 1; i < attempt && delay < maxDelay; i++ {
		delay *= 2
		if delay > maxDelay {
			return maxDelay
		}
	}
	return delay
}

func classifyProviderError(pane string) (providerErrorMatch, bool) {
	for _, line := range strings.Split(stripExplainLines(pane), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.Contains(lower, "insufficient_quota"):
			return providerErrorMatch{Class: "quota", Line: trimmed}, true
		case strings.Contains(lower, "rate_limit") ||
			((strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests")) && providerLineHasAPIContext(lower)):
			return providerErrorMatch{Class: "rate_limit", Line: trimmed}, true
		case strings.Contains(lower, "overloaded_error") ||
			(strings.Contains(lower, "overloaded") && providerLineHasAPIContext(lower)):
			return providerErrorMatch{Class: "overloaded", Line: trimmed}, true
		case strings.Contains(lower, `"type":"api_error"`) || strings.Contains(lower, `"type": "api_error"`) ||
			strings.Contains(lower, "inference backend unreachable"):
			return providerErrorMatch{Class: "api_error", Line: trimmed}, true
		case providerRetryingRe.MatchString(trimmed):
			return providerErrorMatch{Class: "retrying", Line: trimmed}, true
		}
		if m := providerAPIErrorStatusRe.FindStringSubmatch(trimmed); len(m) == 2 {
			return providerErrorMatch{Class: providerErrorStatusClass(m[1]), Line: trimmed}, true
		}
		if providerLineHasAPIContext(lower) {
			if m := providerHTTPStatusRe.FindStringSubmatch(trimmed); len(m) == 2 {
				return providerErrorMatch{Class: providerErrorStatusClass(m[1]), Line: trimmed}, true
			}
		}
	}
	return providerErrorMatch{}, false
}

func providerLineHasAPIContext(lower string) bool {
	return strings.Contains(lower, "api error") ||
		strings.Contains(lower, "api_error") ||
		strings.Contains(lower, "inference") ||
		strings.Contains(lower, "backend") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden")
}

func providerErrorStatusClass(status string) string {
	switch status {
	case "401", "403":
		return "auth"
	case "429":
		return "rate_limit"
	case "529":
		return "overloaded"
	default:
		return "api_error"
	}
}
