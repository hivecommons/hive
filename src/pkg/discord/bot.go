package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	discordAPIBase = "https://discord.com/api/v10"

	httpTimeoutS       = 10
	pollIntervalS      = 5
	msgQueueIntervalMS = 1200
	heartbeatInterval  = 15 * time.Minute
	topicDebounceMS    = 5000
	discordMsgLimit    = 1900
	sseReconnectBase   = 5 * time.Second
	sseReconnectMax    = 60 * time.Second
)

// AgentIdentity holds the Discord display metadata for an agent.
type AgentIdentity struct {
	Emoji string
	Color int
}

var discordMu sync.RWMutex

var agentIdentities = map[string]AgentIdentity{
	"governor": {Emoji: "🚦", Color: 0xf1c40f},
	"pipeline": {Emoji: "⚙️", Color: 0x95a5a6},
}

var aliases = map[string]string{
	"s": "status", "st": "status",
	"g": "governor", "gov": "governor",
	"h": "help", "?": "help",
	"k": "kick", "p": "pause", "r": "resume",
	"sc": "scanner", "ar": "architect", "ou": "outreach",
	"su": "supervisor", "ci": "ci-maintainer", "se": "sec-check",
	"sg": "strategist", "te": "quality", "qa": "quality",
	"tm": "telemetry", "op": "operations",
}

func SetAgentIdentities(identities map[string]AgentIdentity) {
	merged := map[string]AgentIdentity{
		"governor": {Emoji: "🚦", Color: 0xf1c40f},
		"pipeline": {Emoji: "⚙️", Color: 0x95a5a6},
	}
	for k, v := range identities {
		merged[k] = v
	}
	discordMu.Lock()
	agentIdentities = merged
	discordMu.Unlock()
}

func SetAgentAliases(agentAliases map[string]string) {
	discordMu.Lock()
	defer discordMu.Unlock()
	for k, v := range agentAliases {
		aliases[k] = v
	}
}

type CommandHandler func(ctx context.Context, args string) (string, error)

type Config struct {
	Token          string
	ChannelID      string
	DashboardURL   string
	DashboardToken string
	// AllowedUsers is the set of Discord user IDs permitted to issue commands.
	// Empty = commands disabled (fail closed).
	AllowedUsers []string
}

type Bot struct {
	token          string
	channelID      string
	dashboardURL   string
	dashboardToken string
	allowedUsers   map[string]struct{} // Discord user IDs allowed to issue commands
	commands       map[string]CommandHandler
	agentNames     []string
	mu             sync.RWMutex
	logger         *slog.Logger
	client         *http.Client

	msgQueue  chan msgItem
	lastState *statusSnapshot
	lastTopic string
}

type msgItem struct {
	content string
}

type statusSnapshot struct {
	Agents   []agentSnapshot  `json:"agents"`
	Governor governorSnapshot `json:"governor"`
	Budget   budgetSnapshot   `json:"budget"`
}

type agentSnapshot struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Busy        string `json:"busy"`
	Cadence     string `json:"cadence"`
	Doing       string `json:"doing"`
	LiveSummary string `json:"liveSummary"`
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

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	allowed := make(map[string]struct{}, len(cfg.AllowedUsers))
	for _, id := range cfg.AllowedUsers {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{}
		}
	}
	return &Bot{
		token:          cfg.Token,
		channelID:      cfg.ChannelID,
		dashboardURL:   cfg.DashboardURL,
		dashboardToken: cfg.DashboardToken,
		allowedUsers:   allowed,
		commands:       make(map[string]CommandHandler),
		logger:         logger,
		client: &http.Client{
			Timeout: httpTimeoutS * time.Second,
		},
		msgQueue: make(chan msgItem, 100),
	}
}

func (b *Bot) SetAgentNames(names []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.agentNames = names
}

func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands[name] = handler
}

func (b *Bot) Start(ctx context.Context) error {
	if b.token == "" {
		return fmt.Errorf("discord bot token not configured")
	}

	b.logger.Info("discord bot starting", "channel", b.channelID)

	b.registerBuiltinCommands()

	go b.drainLoop(ctx)
	go b.pollLoop(ctx)
	go b.sseLoop(ctx)
	go b.heartbeatLoop(ctx)

	b.enqueue("⚙️ **[pipeline]** Hive v2 Discord bot online")
	return nil
}

func (b *Bot) SendMessage(content string) error {
	payload := map[string]string{"content": content}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, b.channelID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord API %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (b *Bot) enqueue(content string) {
	select {
	case b.msgQueue <- msgItem{content: content}:
	default:
		b.logger.Warn("discord message queue full, dropping message")
	}
}

func (b *Bot) drainLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-b.msgQueue:
			if err := b.SendMessage(item.content); err != nil {
				b.logger.Warn("discord send failed", "error", err)
			}
			time.Sleep(time.Duration(msgQueueIntervalMS) * time.Millisecond)
		}
	}
}

func (b *Bot) registerBuiltinCommands() {
	b.RegisterCommand("status", func(_ context.Context, _ string) (string, error) {
		return b.cmdStatus()
	})
	b.RegisterCommand("governor", func(_ context.Context, _ string) (string, error) {
		return b.cmdGovernor()
	})
	b.RegisterCommand("help", func(_ context.Context, _ string) (string, error) {
		return b.cmdHelp(), nil
	})
	b.RegisterCommand("kick", func(_ context.Context, args string) (string, error) {
		return b.cmdAgentAction("kick", args)
	})
	b.RegisterCommand("pause", func(_ context.Context, args string) (string, error) {
		return b.cmdAgentAction("pause", args)
	})
	b.RegisterCommand("resume", func(_ context.Context, args string) (string, error) {
		return b.cmdAgentAction("resume", args)
	})
}

func (b *Bot) cmdStatus() (string, error) {
	data, err := b.dashboardGet("/api/status")
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
	if len(result) > discordMsgLimit {
		truncated := result[:discordMsgLimit]
		for !utf8.ValidString(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		result = truncated + "…"
	}
	return result, nil
}

func (b *Bot) cmdGovernor() (string, error) {
	data, err := b.dashboardGet("/api/status")
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

func (b *Bot) cmdHelp() string {
	b.mu.RLock()
	agents := b.agentNames
	b.mu.RUnlock()

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

func (b *Bot) cmdAgentAction(action, args string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	agentName := strings.ToLower(parts[0])
	prompt := ""
	if len(parts) > 1 {
		prompt = parts[1]
	}

	agentName = resolveAlias(agentName)

	if !b.isValidAgent(agentName) {
		return fmt.Sprintf("❌ Unknown agent: `%s`", agentName), nil
	}

	switch action {
	case "kick":
		return b.dashboardKick(agentName, prompt)
	case "pause":
		return b.dashboardPause(agentName)
	case "resume":
		return b.dashboardResume(agentName)
	}
	return "❌ Unknown action", nil
}

func (b *Bot) dashboardKick(agent, prompt string) (string, error) {
	var body []byte
	if prompt != "" {
		var err error
		body, err = json.Marshal(map[string]string{"prompt": prompt})
		if err != nil {
			return fmt.Sprintf("❌ Failed to marshal kick payload: %s", err), nil
		}
	}
	err := b.dashboardPost(fmt.Sprintf("/api/kick/%s", agent), body)
	if err != nil {
		return fmt.Sprintf("❌ Failed to kick %s: %s", agent, err), nil
	}
	if prompt != "" {
		return fmt.Sprintf("✅ Sent to %s: \"%s\"", agent, prompt), nil
	}
	return fmt.Sprintf("✅ Kicked %s", agent), nil
}

func (b *Bot) dashboardPause(agent string) (string, error) {
	err := b.dashboardPost(fmt.Sprintf("/api/pause/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to pause %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Paused %s", agent), nil
}

func (b *Bot) dashboardResume(agent string) (string, error) {
	err := b.dashboardPost(fmt.Sprintf("/api/resume/%s", agent), nil)
	if err != nil {
		return fmt.Sprintf("❌ Failed to resume %s: %s", agent, err), nil
	}
	return fmt.Sprintf("✅ Resumed %s", agent), nil
}

// SSE bridge: monitors /api/events and posts agent transitions + governor mode changes
func (b *Bot) sseLoop(ctx context.Context) {
	delay := sseReconnectBase
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := b.consumeSSE(ctx)
		if err != nil {
			b.logger.Warn("discord SSE disconnected", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, sseReconnectMax)
	}
}

func (b *Bot) consumeSSE(ctx context.Context) error {
	url := b.dashboardURL + "/api/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	sseClient := &http.Client{Timeout: 0}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE status %d", resp.StatusCode)
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
							b.onSSEEvent(&snap)
						}
					}
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

func (b *Bot) onSSEEvent(snap *statusSnapshot) {
	b.mu.Lock()
	prev := b.lastState
	b.lastState = snap
	b.mu.Unlock()

	if prev == nil {
		return
	}

	b.diffAgents(prev, snap)
	b.diffGovernor(prev, snap)
	b.updateTopic(snap)
}

func (b *Bot) diffAgents(prev, cur *statusSnapshot) {
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
				summary := ""
				if agent.LiveSummary != "" {
					lines := strings.SplitN(agent.LiveSummary, "\n", 4)
					if len(lines) > 3 {
						lines = lines[:3]
					}
					s := strings.Join(lines, "\n")
					const maxSnippetRunes = 300
					if len([]rune(s)) > maxSnippetRunes {
						s = string([]rune(s)[:maxSnippetRunes])
					}
					summary = "\n```\n" + s + "\n```"
				}
				b.enqueue(fmt.Sprintf("%s Completed%s%s", prefix, doing, summary))
			case agent.Busy == "working" && old.Busy == "idle":
				b.enqueue(fmt.Sprintf("%s Working%s", prefix, doing))
			}
		}

		if agent.Paused && !old.Paused {
			b.enqueue(fmt.Sprintf("%s Paused", prefix))
		} else if !agent.Paused && old.Paused {
			b.enqueue(fmt.Sprintf("%s Resumed", prefix))
		}

		if agent.Cadence == "off" && old.Cadence != "off" {
			b.enqueue(fmt.Sprintf("%s Off (cadence rule)", prefix))
		}
	}
}

func (b *Bot) diffGovernor(prev, cur *statusSnapshot) {
	if prev.Governor.Mode != "" && cur.Governor.Mode != "" && prev.Governor.Mode != cur.Governor.Mode {
		queue := cur.Governor.Issues + cur.Governor.PRs
		b.enqueue(fmt.Sprintf("🚦 **Governor mode change:** %s → %s (queue: %d)", prev.Governor.Mode, cur.Governor.Mode, queue))
	}
}

func (b *Bot) updateTopic(snap *statusSnapshot) {
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

	b.mu.Lock()
	changed := topic != b.lastTopic
	b.lastTopic = topic
	b.mu.Unlock()

	if changed {
		go func() {
			time.Sleep(time.Duration(topicDebounceMS) * time.Millisecond)
			if err := b.setChannelTopic(topic); err != nil {
				b.logger.Debug("topic update failed", "error", err)
			}
		}()
	}
}

func (b *Bot) setChannelTopic(topic string) error {
	payload, err := json.Marshal(map[string]string{"topic": topic})
	if err != nil {
		return fmt.Errorf("marshal topic payload: %w", err)
	}
	url := fmt.Sprintf("%s/channels/%s", discordAPIBase, b.channelID)

	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("topic update %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (b *Bot) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := b.cmdStatus()
			if err == nil && status != "" {
				b.enqueue(fmt.Sprintf("📊 **Status heartbeat**\n%s", status))
			}
		}
	}
}

// Command routing with aliases (matches old hive `!` prefix)
func (b *Bot) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollIntervalS * time.Second)
	defer ticker.Stop()

	var lastMessageID string
	firstPoll := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messages, err := b.fetchMessages(lastMessageID)
			if err != nil {
				b.logger.Warn("discord poll failed", "error", err)
				continue
			}

			for i := len(messages) - 1; i >= 0; i-- {
				msg := messages[i]
				lastMessageID = msg.ID
				if !firstPoll {
					b.routeMessage(ctx, msg)
				}
			}
			firstPoll = false
		}
	}
}

type discordMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
}

func (b *Bot) fetchMessages(after string) ([]discordMessage, error) {
	url := fmt.Sprintf("%s/channels/%s/messages?limit=10", discordAPIBase, b.channelID)
	if after != "" {
		url += "&after=" + after
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("discord API %d: %s", resp.StatusCode, string(body))
	}

	var messages []discordMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func (b *Bot) routeMessage(ctx context.Context, msg discordMessage) {
	if msg.Author.Bot {
		return
	}

	content := strings.TrimSpace(msg.Content)
	if !strings.HasPrefix(content, "!") {
		return
	}

	// SECURITY (F8, CWE-862): command dispatch is gated on the allowlist and FAILS
	// CLOSED. Commands (!kick / agent commands) inject prompts and can reach
	// dashboardKick, which POSTs attacker-supplied text with the privileged
	// dashboard bearer — so an unconfigured allowlist must mean "no one is
	// authorized", not "everyone is authorized". This matches the documented
	// contract on Config.AllowedUsers ("Empty = commands disabled (fail closed)").
	// An empty allowlist rejects every command; operators enable command control
	// by populating allowed_users with the specific Discord user IDs they trust.
	if len(b.allowedUsers) == 0 {
		b.logger.Warn("discord: ignoring command — allowlist is empty (commands disabled; set allowed_users to enable)",
			"user_id", msg.Author.ID, "content", content)
		return
	}
	if _, ok := b.allowedUsers[msg.Author.ID]; !ok {
		b.logger.Warn("discord: ignoring command from non-allowlisted user",
			"user_id", msg.Author.ID, "content", content)
		return
	}

	content = content[1:]

	parts := strings.SplitN(content, " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	cmd = resolveAlias(cmd)

	b.mu.RLock()
	handler, hasCmd := b.commands[cmd]
	b.mu.RUnlock()

	if hasCmd {
		reply, err := handler(ctx, args)
		if err != nil {
			reply = fmt.Sprintf("❌ %s", err)
		}
		if reply != "" {
			b.enqueue(reply)
		}
		return
	}

	if b.isValidAgent(cmd) {
		subParts := strings.SplitN(args, " ", 2)
		action := strings.ToLower(subParts[0])
		action = resolveAlias(action)
		rest := ""
		if len(subParts) > 1 {
			rest = subParts[1]
		}

		var reply string
		var err error
		switch action {
		case "pause":
			reply, err = b.dashboardPause(cmd)
		case "resume":
			reply, err = b.dashboardResume(cmd)
		case "kick":
			reply, err = b.dashboardKick(cmd, rest)
		default:
			reply, err = b.dashboardKick(cmd, args)
		}
		if err != nil {
			reply = fmt.Sprintf("❌ %s", err)
		}
		if reply != "" {
			b.enqueue(reply)
		}
		return
	}

	b.enqueue(fmt.Sprintf("❌ Unknown command: `%s`. Try `!help`", cmd))
}

func (b *Bot) isValidAgent(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, a := range b.agentNames {
		if a == name {
			return true
		}
	}
	return false
}

func (b *Bot) dashboardGet(path string) ([]byte, error) {
	resp, err := b.client.Get(b.dashboardURL + path)
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

func (b *Bot) dashboardPost(path string, body []byte) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, b.dashboardURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.dashboardToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.dashboardToken)
	}

	resp, err := b.client.Do(req)
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

func resolveAlias(s string) string {
	discordMu.RLock()
	defer discordMu.RUnlock()
	if v, ok := aliases[s]; ok {
		return v
	}
	return s
}

func getIdentity(name string) AgentIdentity {
	discordMu.RLock()
	defer discordMu.RUnlock()
	if id, ok := agentIdentities[name]; ok {
		return id
	}
	return agentIdentities["pipeline"]
}
