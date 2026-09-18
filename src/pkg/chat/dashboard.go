package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

func (s *Service) cmdStatus() (string, error) {
	data, err := s.dashboardGet("/api/status")
	if err != nil {
		return "❌ Could not reach dashboard", nil
	}

	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return "❌ Failed to parse dashboard status", nil
	}

	lines := []string{
		fmt.Sprintf("**Governor:** %s (issues: %d, PRs: %d)", snap.Governor.Mode, snap.Governor.Issues, snap.Governor.PRs),
	}
	if snap.Budget.WeeklyBudget > 0 {
		lines = append(lines, fmt.Sprintf("**Budget:** $%d / $%d (%.0f%% used)", snap.Budget.Used, snap.Budget.WeeklyBudget, snap.Budget.PctUsed))
	}

	for _, a := range snap.Agents {
		id := getIdentity(a.Name)
		state := a.Busy
		if a.Paused {
			state = "paused"
		}
		cadence := a.Cadence
		doing := ""
		const maxDoingRunes = 80
		if a.Doing != "" && len([]rune(a.Doing)) > maxDoingRunes {
			doing = " — " + string([]rune(a.Doing)[:maxDoingRunes])
		} else if a.Doing != "" {
			doing = " — " + a.Doing
		}
		lines = append(lines, fmt.Sprintf("  %s **[%s]** %s (%s)%s", id.Emoji, a.Name, state, cadence, doing))
	}

	result := strings.Join(lines, "\n")
	if len(result) > s.messageLimit {
		truncated := result[:s.messageLimit]
		for !utf8.ValidString(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		result = truncated + "…"
	}
	return result, nil
}

func (s *Service) cmdGovernor() (string, error) {
	data, err := s.dashboardGet("/api/status")
	if err != nil {
		return "❌ Could not reach dashboard", nil
	}

	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return "❌ Failed to parse dashboard status", nil
	}

	lines := []string{
		fmt.Sprintf("**Mode:** %s", snap.Governor.Mode),
		fmt.Sprintf("**Queue:** %d issues, %d PRs", snap.Governor.Issues, snap.Governor.PRs),
	}
	if snap.Budget.WeeklyBudget > 0 {
		lines = append(lines, fmt.Sprintf("**Budget:** $%d / $%d (%.0f%% used)", snap.Budget.Used, snap.Budget.WeeklyBudget, snap.Budget.PctUsed))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdHelp() string {
	s.mu.RLock()
	agents := s.agentNames
	s.mu.RUnlock()

	lines := []string{
		"**Hive v2 Discord Bot Commands**",
		"`!status` (`!s`) — show system status",
		"`!governor` (`!g`, `!gov`) — show governor mode and budget",
		"`!kick <agent> [prompt]` (`!k`) — kick an agent with optional prompt",
		"`!pause <agent>` (`!p`) — pause an agent",
		"`!resume <agent>` (`!r`) — resume an agent",
		"`!<agent> [prompt]` — send prompt to agent (kick shorthand)",
		"`!help` (`!h`, `!?`) — show this message",
		"",
		fmt.Sprintf("Valid agents: %s", strings.Join(agents, ", ")),
	}
	return strings.Join(lines, "\n")
}

func (s *Service) cmdAgentAction(action, args string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	agentName := strings.ToLower(parts[0])
	prompt := ""
	if len(parts) > 1 {
		prompt = parts[1]
	}

	agentName = resolveAlias(agentName)

	if !s.isValidAgent(agentName) {
		return fmt.Sprintf("❌ Unknown agent: `%s`", agentName), nil
	}

	switch action {
	case "kick":
		return s.dashboardKick(agentName, prompt)
	case "pause":
		return s.dashboardPause(agentName)
	case "resume":
		return s.dashboardResume(agentName)
	}
	return "❌ Unknown action", nil
}

func (s *Service) dashboardKick(agent, prompt string) (string, error) {
	var body []byte
	if prompt != "" {
		var err error
		body, err = json.Marshal(map[string]string{"prompt": prompt})
		if err != nil {
			return fmt.Sprintf("❌ Failed to marshal kick payload: %s", err), nil
		}
	}
	err := s.dashboardPost(fmt.Sprintf("/api/kick/%s", agent), body)
	if err != nil {
		return fmt.Sprintf("❌ Failed to kick %s: %s", agent, err), nil
	}
	if prompt != "" {
		return fmt.Sprintf("✅ Sent to %s: \"%s\"", agent, prompt), nil
	}
	return fmt.Sprintf("✅ Kicked %s", agent), nil
}

func (s *Service) dashboardPause(agent string) (string, error) {
	err := s.dashboardPost(fmt.Sprintf("/api/pause/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to pause %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Paused %s", agent), nil
}

func (s *Service) dashboardResume(agent string) (string, error) {
	err := s.dashboardPost(fmt.Sprintf("/api/resume/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to resume %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Resumed %s", agent), nil
}

func (s *Service) dashboardGet(path string) ([]byte, error) {
	resp, err := s.client.Get(s.dashboardURL + path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxDiscordResponseBytes = 10 << 20 // 10 MiB
	return io.ReadAll(io.LimitReader(resp.Body, maxDiscordResponseBytes))
}

func (s *Service) dashboardPost(path string, body []byte) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, s.dashboardURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.dashboardToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.dashboardToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
