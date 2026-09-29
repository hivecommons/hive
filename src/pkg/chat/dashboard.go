package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (s *Service) cmdStatus(ctx context.Context) (string, error) {
	data, err := s.dashboardGet(ctx, "/api/status")
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

func (s *Service) cmdGovernor(ctx context.Context) (string, error) {
	data, err := s.dashboardGet(ctx, "/api/status")
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
		fmt.Sprintf("**Hive Bot Commands** (%s)", s.backend.Name()),
		"`!status` (`!s`) — show system status",
		"`!governor` (`!g`, `!gov`) — show governor mode and budget",
		"`!kick <agent> [prompt]` (`!k`) — kick an agent with optional prompt",
		"`!pause <agent>` (`!p`) — pause an agent",
		"`!resume <agent>` (`!r`) — resume an agent",
		"`!runs list` / `!runs status <key>` — list active runs or show one run with artifacts",
		"`!runs spec <owner/repo#n>` — start a Spektacular spec-stage run (owner)",
		"`!runs approve <key>` / `!runs reject <key> <reason>` — answer a run checkpoint (owner)",
		"`approve` or `reject <reason>` — answer your single pending run checkpoint",
		"`!standby <lane> [owner/repo#number]` — manually dispatch paused-lane work to a qualified standby contributor",
		"`!standby-clear <contributor> <backend> <model> [effort]` — clear a standby suspension",
		"`!<agent> [prompt]` — send prompt to agent (kick shorthand)",
		"`!help` (`!h`, `!?`) — show this message",
		"",
		fmt.Sprintf("Valid agents: %s", strings.Join(agents, ", ")),
	}
	return strings.Join(lines, "\n")
}

func (s *Service) cmdStandbyDispatch(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return "❌ Usage: `!standby <lane> [owner/repo#number]`", nil
	}
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	body, err := json.Marshal(map[string]string{"lane": fields[0], "key": func() string {
		if len(fields) > 1 {
			return fields[1]
		}
		return ""
	}()})
	if err != nil {
		return fmt.Sprintf("❌ Failed to marshal standby payload: %s", err), nil
	}
	if err := s.dashboardPost(ctx, "/api/contribute/standby/dispatch", body); err != nil {
		return fmt.Sprintf("❌ Failed to dispatch standby work: %s", err), nil
	}
	return fmt.Sprintf("✅ Dispatched standby work for %s", fields[0]), nil
}

func (s *Service) cmdStandbyClear(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) < 3 {
		return "❌ Usage: `!standby-clear <contributor> <backend> <model> [effort]`", nil
	}
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	payload := map[string]string{
		"contributor": fields[0],
		"backend":     fields[1],
		"model":       fields[2],
	}
	if len(fields) > 3 {
		payload["reasoning_effort"] = fields[3]
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("❌ Failed to marshal standby clear payload: %s", err), nil
	}
	if err := s.dashboardPost(ctx, "/api/contribute/standby/clear", body); err != nil {
		return fmt.Sprintf("❌ Failed to clear standby suspension: %s", err), nil
	}
	return fmt.Sprintf("✅ Cleared standby suspension for %s", fields[0]), nil
}

func (s *Service) cmdAgentAction(ctx context.Context, action, args string) (string, error) {
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
		return s.dashboardKick(ctx, agentName, prompt)
	case "pause":
		return s.dashboardPause(ctx, agentName)
	case "resume":
		return s.dashboardResume(ctx, agentName)
	}
	return "❌ Unknown action", nil
}

func (s *Service) dashboardKick(ctx context.Context, agent, prompt string) (string, error) {
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	var body []byte
	if prompt != "" {
		var err error
		body, err = json.Marshal(map[string]string{"prompt": prompt})
		if err != nil {
			return fmt.Sprintf("❌ Failed to marshal kick payload: %s", err), nil
		}
	}
	err := s.dashboardPost(ctx, fmt.Sprintf("/api/kick/%s", agent), body)
	if err != nil {
		return fmt.Sprintf("❌ Failed to kick %s: %s", agent, err), nil
	}
	if prompt != "" {
		return fmt.Sprintf("✅ Sent to %s: \"%s\"", agent, prompt), nil
	}
	return fmt.Sprintf("✅ Kicked %s", agent), nil
}

func (s *Service) dashboardPause(ctx context.Context, agent string) (string, error) {
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	err := s.dashboardPost(ctx, fmt.Sprintf("/api/pause/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to pause %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Paused %s", agent), nil
}

func (s *Service) dashboardResume(ctx context.Context, agent string) (string, error) {
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	err := s.dashboardPost(ctx, fmt.Sprintf("/api/resume/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to resume %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Resumed %s", agent), nil
}

// authorizeDashboardRequest attaches the spine's credential for the local
// dashboard API. It sends X-Hive-Internal, the server-to-server header the
// dashboard middleware honors on every deployment shape. Authorization: Bearer
// is deliberately disabled on a direct-route spoke (authorized_users allowlist,
// no hub proxy) because it carries no per-user identity, so a chat service that
// sent only the bearer answered 401 there on every command and the SSE stream
// (#9134). The chat service is the same process as the dashboard and talks to
// it over localhost, which is exactly the trusted path X-Hive-Internal exists for.
//
// A command-triggered request also carries X-Hive-Chat-Actor (the
// transport-qualified author) so the dashboard audit log attributes the action
// to the chat user rather than the shared token (#9125); background requests
// (SSE, heartbeats) carry none.
func (s *Service) authorizeDashboardRequest(ctx context.Context, req *http.Request) {
	if s.dashboardToken == "" {
		return
	}
	req.Header.Set("X-Hive-Internal", s.dashboardToken)
	if author, _ := ctx.Value(commandAuthorContextKey{}).(string); author != "" && s.backend != nil {
		req.Header.Set("X-Hive-Chat-Actor", s.backend.Name()+":"+author)
	}
}

func (s *Service) dashboardGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.dashboardURL+path, nil)
	if err != nil {
		return nil, err
	}
	s.authorizeDashboardRequest(ctx, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxDashboardResponseBytes = 10 << 20 // 10 MiB
	return io.ReadAll(io.LimitReader(resp.Body, maxDashboardResponseBytes))
}

func (s *Service) dashboardPost(ctx context.Context, path string, body []byte) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.dashboardURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.authorizeDashboardRequest(ctx, req)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyReadBytes))
		if summary := summarizeErrorBody(respBody); summary != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, summary)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

const (
	// maxErrorBodyReadBytes bounds how much of a dashboard error body is read
	// before summarising it; the reply that reaches the channel is bounded
	// further by maxErrorBodySummaryBytes (hivecommons/hive#9129).
	maxErrorBodyReadBytes    = 4 << 10
	maxErrorBodySummaryBytes = 200
	errorBodySummaryEllipsis = "…"
)

// summarizeErrorBody reduces a dashboard error body to something safe to echo
// into a chat channel: the first non-empty line, control characters dropped,
// and never more than maxErrorBodySummaryBytes including the ellipsis, cut on
// a rune boundary. HTML error pages and multi-line stack traces collapse to
// one short line instead of being posted verbatim.
func summarizeErrorBody(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.Map(func(r rune) rune {
			if r == utf8.RuneError || unicode.IsControl(r) {
				return -1
			}
			return r
		}, line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxErrorBodySummaryBytes {
			cut := maxErrorBodySummaryBytes - len(errorBodySummaryEllipsis)
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = strings.TrimSpace(line[:cut]) + errorBodySummaryEllipsis
		}
		return line
	}
	return ""
}
