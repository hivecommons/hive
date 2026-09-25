package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentpkg "github.com/hivecommons/hive/pkg/agent"
)

const (
	topicDebounceMS  = 5000
	sseReconnectBase = 5 * time.Second
	sseReconnectMax  = 60 * time.Second
)

type statusSnapshot struct {
	Agents    []agentSnapshot   `json:"agents"`
	Governor  governorSnapshot  `json:"governor"`
	Budget    budgetSnapshot    `json:"budget"`
	Inception inceptionSnapshot `json:"inception"`
	Runs      runSnapshotList   `json:"runs"`
}

type agentSnapshot struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Busy        string `json:"busy"`
	Cadence     string `json:"cadence"`
	Doing       string `json:"doing"`
	LiveSummary string `json:"liveSummary"`
	LastKickAt  string `json:"lastKickAt"`
	Paused      bool   `json:"paused"`
}

type governorSnapshot struct {
	Mode   string `json:"mode"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
}

type budgetSnapshot struct {
	WeeklyBudget int64   `json:"BUDGET_WEEKLY"`
	Used         int64   `json:"BUDGET_USED"`
	PctUsed      float64 `json:"BUDGET_PCT_USED"`
}

// SSE bridge: monitors /api/events and posts agent transitions + governor mode changes
func (s *Service) sseLoop(ctx context.Context) {
	delay := s.sseReconnectBase
	if delay == 0 {
		delay = sseReconnectBase
	}
	maxDelay := s.sseReconnectMax
	if maxDelay == 0 {
		maxDelay = sseReconnectMax
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		connected, err := s.consumeSSE(ctx)
		if err != nil {
			s.logger.Warn("discord SSE disconnected", "error", err)
		}
		if connected {
			delay = s.sseReconnectBase
			if delay == 0 {
				delay = sseReconnectBase
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if !connected {
			delay = min(delay*2, maxDelay)
		}
	}
}

func (s *Service) consumeSSE(ctx context.Context) (bool, error) {
	url := s.dashboardURL + "/api/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if s.dashboardToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.dashboardToken)
	}

	sseClient := &http.Client{Timeout: 0}
	resp, err := sseClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("SSE status %d", resp.StatusCode)
	}

	buf := make([]byte, 4096)
	var buffer string

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			buffer += string(buf[:n])
			for {
				idx := strings.Index(buffer, "\n\n")
				if idx < 0 {
					break
				}
				block := buffer[:idx]
				buffer = buffer[idx+2:]

				for _, line := range strings.Split(block, "\n") {
					if strings.HasPrefix(line, "data:") {
						payload := strings.TrimSpace(line[5:])
						var snap statusSnapshot
						if json.Unmarshal([]byte(payload), &snap) == nil {
							s.onSSEEvent(&snap)
						}
					}
				}
			}
		}
		if err != nil {
			return true, err
		}
	}
}

func (s *Service) onSSEEvent(snap *statusSnapshot) {
	s.mu.Lock()
	prev := s.lastState
	s.lastState = snap
	s.mu.Unlock()

	if prev == nil {
		if len(snap.Runs) > 0 {
			s.mu.Lock()
			s.lastRuns = runSliceMap(snap.Runs)
			s.mu.Unlock()
		} else {
			s.syncRunsFromSSE(context.Background())
		}
		return
	}

	s.diffAgents(prev, snap)
	s.diffGovernor(prev, snap)
	s.diffInception(prev, snap)
	if len(snap.Runs) > 0 {
		s.mu.Lock()
		prevRuns := s.lastRuns
		s.lastRuns = runSliceMap(snap.Runs)
		s.mu.Unlock()
		if prevRuns != nil {
			s.diffRuns(runMapSlice(prevRuns), snap.Runs)
		}
	} else {
		s.syncRunsFromSSE(context.Background())
	}
	s.updateTopic(snap)
}

func (s *Service) diffAgents(prev, cur *statusSnapshot) {
	prevMap := make(map[string]*agentSnapshot)
	for i := range prev.Agents {
		prevMap[prev.Agents[i].Name] = &prev.Agents[i]
	}

	for _, agent := range cur.Agents {
		old, ok := prevMap[agent.Name]
		if !ok {
			continue
		}

		id := getIdentity(agent.Name)
		prefix := fmt.Sprintf("%s **[%s]**", id.Emoji, agent.Name)

		if old.Busy != agent.Busy {
			doing := ""
			if agent.Doing != "" {
				const maxDoingRunes2 = 100
				if len([]rune(agent.Doing)) > maxDoingRunes2 {
					doing = " — " + string([]rune(agent.Doing)[:maxDoingRunes2])
				} else {
					doing = " — " + agent.Doing
				}
			}

			switch {
			case agent.Busy == "idle" && old.Busy == "working":
				s.enqueue(fmt.Sprintf("%s %s", prefix, s.agentCompletionText(agent, *old)))
			case agent.Busy == "working" && old.Busy == "idle":
				s.enqueue(fmt.Sprintf("%s Working%s%s", prefix, doing, s.agentDetailsSuffix(agent.Name)))
			}
		}

		if agent.Paused && !old.Paused {
			s.enqueue(fmt.Sprintf("%s Paused", prefix))
		} else if !agent.Paused && old.Paused {
			s.enqueue(fmt.Sprintf("%s Resumed", prefix))
		}

		if agent.Cadence == "off" && old.Cadence != "off" {
			s.enqueue(fmt.Sprintf("%s Off (cadence rule)", prefix))
		}
	}
}

func (s *Service) agentCompletionText(agent, old agentSnapshot) string {
	summary := agentpkg.SanitizePaneText(agent.LiveSummary, 3)
	work := strings.TrimSpace(agent.Doing)
	if work == "" {
		work = strings.TrimSpace(old.Doing)
	}
	work = strings.TrimPrefix(strings.TrimPrefix(work, "done "), "Done ")
	work = truncateRunes(work, 100)
	duration := agentDurationSuffix(agent.LastKickAt)
	details := s.agentDetailsSuffix(agent.Name)

	if outcome := completionOutcome(summary); outcome != "" {
		if work != "" && outcome != "no new work" {
			return fmt.Sprintf("Completed — %s: %s%s%s", work, outcome, duration, details)
		}
		return fmt.Sprintf("Completed — %s%s%s", outcome, duration, details)
	}
	if work != "" {
		return fmt.Sprintf("Completed — finished a pass on %s%s%s", work, duration, details)
	}
	return fmt.Sprintf("Completed — finished a pass — no new work%s%s", duration, details)
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func completionOutcome(summary string) string {
	lower := strings.ToLower(summary)
	switch {
	case summary == "":
		return ""
	case strings.Contains(lower, "nothing to do") || strings.Contains(lower, "no new work") ||
		strings.Contains(lower, "no actionable") || strings.Contains(lower, "no changes"):
		return "no new work"
	case strings.Contains(lower, "/pull/") || strings.Contains(lower, " pr #") || strings.Contains(lower, "pull request"):
		return firstNonEmptyLine(summary)
	case strings.Contains(lower, "/issues/") || strings.Contains(lower, "issue #") || strings.Contains(lower, "commented"):
		return firstNonEmptyLine(summary)
	default:
		return ""
	}
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			const maxRunes = 180
			return truncateRunes(trimmed, maxRunes)
		}
	}
	return ""
}

func agentDurationSuffix(lastKickAt string) string {
	if strings.TrimSpace(lastKickAt) == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, lastKickAt)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		return ""
	}
	return " (" + compactDuration(d) + ")"
}

func compactDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func (s *Service) agentDetailsSuffix(name string) string {
	if strings.TrimSpace(s.dashboardURL) == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return " — details: " + strings.TrimRight(s.dashboardURL, "/") + "/#" + url.PathEscape(name)
}

func (s *Service) diffGovernor(prev, cur *statusSnapshot) {
	if prev.Governor.Mode != "" && cur.Governor.Mode != "" && prev.Governor.Mode != cur.Governor.Mode {
		queue := cur.Governor.Issues + cur.Governor.PRs
		s.enqueue(fmt.Sprintf("🚦 **Governor mode change:** %s → %s (queue: %d)", prev.Governor.Mode, cur.Governor.Mode, queue))
	}
}

func (s *Service) updateTopic(snap *statusSnapshot) {
	stateIcons := map[string]string{"working": "🟢", "idle": "⚪", "paused": "🔴", "off": "⚫"}

	var parts []string
	for _, a := range snap.Agents {
		id := getIdentity(a.Name)
		state := a.Busy
		if a.Paused {
			state = "paused"
		}
		if a.Cadence == "off" {
			state = "off"
		}
		icon, ok := stateIcons[state]
		if !ok {
			icon = stateIcons["idle"]
		}
		parts = append(parts, id.Emoji+icon)
	}

	topic := fmt.Sprintf("%s · %s · %di %dpr", strings.Join(parts, " "), snap.Governor.Mode, snap.Governor.Issues, snap.Governor.PRs)

	s.mu.Lock()
	changed := topic != s.lastTopic
	s.lastTopic = topic
	s.mu.Unlock()

	if changed {
		go func() {
			time.Sleep(time.Duration(topicDebounceMS) * time.Millisecond)
			if err := s.backend.SetTopic(topic); err != nil {
				s.logger.Debug("topic update failed", "error", err)
			}
		}()
	}
}

func (s *Service) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := s.cmdStatus(ctx)
			if err == nil && status != "" {
				s.enqueue(fmt.Sprintf("📊 **Status heartbeat**\n%s", status))
			}
		}
	}
}
