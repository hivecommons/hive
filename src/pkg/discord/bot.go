package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/paneclass"
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

	// nonceLimit is Discord's longest message nonce.
	nonceLimit = 25
	// threadNameLimit is Discord's longest thread name, in characters.
	threadNameLimit = 100
	// historyLimit bounds RecentMessages; crash recovery never reads further back.
	historyLimit = 50
	// maxRateLimitRetries bounds the retries of one call after a 429.
	maxRateLimitRetries = 3
	// maxRetryAfter caps a single 429 wait.
	maxRetryAfter = 15 * time.Second
	// threadExistsCode is Discord's error code for a message that already
	// has a thread.
	threadExistsCode = "160004"
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 1 << 20
)

// rateLimitSleep waits out a 429; tests replace it.
var rateLimitSleep = time.Sleep

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

	// self is the bot's own user ID, resolved on first use.
	self string
	// guilds maps a thread ID to its guild, for thread links.
	guilds map[string]string
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
	return b.SendMessageTo(b.channelID, content)
}

// Embed represents a Discord embed object.
type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color,omitempty"`
	Author      *EmbedAuthor `json:"author,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
}

// EmbedAuthor is the author field of a Discord embed.
type EmbedAuthor struct {
	Name    string `json:"name,omitempty"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

// EmbedField is a name/value field within a Discord embed.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// EmbedFooter is the footer field of a Discord embed.
type EmbedFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}

// SendMessageTo posts content to an arbitrary channel the bot token can
// write to, bypassing the status channel and the drain queue. It works on a
// Bot that was never started, so a feature that only needs to post (RFC
// lifecycle notices, pkg/rfcnotify) can share the token without running the
// command loops.
func (b *Bot) SendMessageTo(channelID, content string) error {
	if strings.TrimSpace(channelID) == "" {
		return fmt.Errorf("discord send: channel id not configured")
	}
	return b.do(http.MethodPost, fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, channelID), messagePayload(content), nil)
}

// messagePayload is a plain-text message that cannot ping anyone.
func messagePayload(content string) map[string]any {
	return map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// SendEmbedTo posts an embed (and optional content) to an arbitrary channel
// the bot token can write to. Like SendMessageTo it never pings anyone, and
// it returns the created message ID.
func (b *Bot) SendEmbedTo(channelID, content string, embed Embed) (string, error) {
	if strings.TrimSpace(channelID) == "" {
		return "", fmt.Errorf("discord send: channel id not configured")
	}
	payload := messagePayload(content)
	if content == "" {
		delete(payload, "content")
	}
	payload["embeds"] = []Embed{embed}
	var msg struct {
		ID string `json:"id"`
	}
	if err := b.do(http.MethodPost, fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, channelID), payload, &msg); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// PostMessageNonce posts content to channelID with a Discord nonce and
// enforce_nonce, so a retry inside Discord's dedup window returns the
// message the first attempt created instead of a second one. It returns the
// message ID. The nonce is at most 25 characters.
func (b *Bot) PostMessageNonce(channelID, content, nonce string) (string, error) {
	if strings.TrimSpace(channelID) == "" {
		return "", fmt.Errorf("discord send: channel id not configured")
	}
	if len(nonce) > nonceLimit {
		return "", fmt.Errorf("discord send: nonce is %d characters, the limit is %d", len(nonce), nonceLimit)
	}
	payload := messagePayload(content)
	if nonce != "" {
		payload["nonce"] = nonce
		payload["enforce_nonce"] = true
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := b.do(http.MethodPost, fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, channelID), payload, &msg); err != nil {
		return "", err
	}
	if msg.ID == "" {
		return "", fmt.Errorf("discord send: response carried no message id")
	}
	return msg.ID, nil
}

// StartThreadFromMessage opens a public thread on an existing message and
// returns the thread ID. Threads are channels, so SendMessageTo works on it.
// The name is truncated to Discord's 100 characters.
func (b *Bot) StartThreadFromMessage(channelID, messageID, name string) (string, error) {
	name = truncateRunes(strings.TrimSpace(name), threadNameLimit)
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" || name == "" {
		return "", fmt.Errorf("discord thread: channel id, message id and name are required")
	}
	var th struct {
		ID      string `json:"id"`
		GuildID string `json:"guild_id"`
	}
	url := fmt.Sprintf("%s/channels/%s/messages/%s/threads", discordAPIBase, channelID, messageID)
	if err := b.do(http.MethodPost, url, map[string]any{"name": name}, &th); err != nil {
		// 160004: a thread already exists on this message (an earlier call
		// created it but its answer was lost). A thread started from a
		// message shares that message's ID, so the retry is already done.
		if strings.Contains(err.Error(), threadExistsCode) {
			return messageID, nil
		}
		return "", err
	}
	if th.ID == "" {
		return "", fmt.Errorf("discord thread: response carried no thread id")
	}
	b.rememberGuild(th.ID, th.GuildID)
	return th.ID, nil
}

// ChannelMessage is one message from a channel's history.
type ChannelMessage struct {
	ID      string
	Content string
	// Nonce is the nonce the sender attached, when Discord returned it.
	Nonce string
	// Self is true when this bot authored the message.
	Self bool
}

// RecentMessages returns up to limit (at most 50) of the newest messages in
// channelID. It exists for crash recovery (finding a card this bot already
// posted), not for scanning history.
func (b *Bot) RecentMessages(channelID string, limit int) ([]ChannelMessage, error) {
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("discord history: channel id not configured")
	}
	if limit <= 0 || limit > historyLimit {
		limit = historyLimit
	}
	self, err := b.selfID()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Nonce   any    `json:"nonce"`
		Author  struct {
			ID string `json:"id"`
		} `json:"author"`
	}
	if err := b.do(http.MethodGet, fmt.Sprintf("%s/channels/%s/messages?limit=%d", discordAPIBase, channelID, limit), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]ChannelMessage, 0, len(raw))
	for _, m := range raw {
		nonce, _ := m.Nonce.(string)
		out = append(out, ChannelMessage{
			ID: m.ID, Content: m.Content, Nonce: nonce,
			Self: m.Author.ID == self,
		})
	}
	return out, nil
}

// selfID resolves, once, the bot's own user ID.
func (b *Bot) selfID() (string, error) {
	b.mu.RLock()
	id := b.self
	b.mu.RUnlock()
	if id != "" {
		return id, nil
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := b.do(http.MethodGet, discordAPIBase+"/users/@me", nil, &me); err != nil {
		return "", fmt.Errorf("discord identity: %w", err)
	}
	if me.ID == "" {
		return "", fmt.Errorf("discord identity: response carried no user id")
	}
	b.mu.Lock()
	b.self = me.ID
	b.mu.Unlock()
	return me.ID, nil
}

// ThreadURL is the link to a thread, built from the guild the thread lives
// in. The guild is learned when the thread is created or, failing that,
// looked up once.
func (b *Bot) ThreadURL(threadID string) (string, error) {
	if strings.TrimSpace(threadID) == "" {
		return "", fmt.Errorf("discord thread url: thread id is required")
	}
	b.mu.RLock()
	guild := b.guilds[threadID]
	b.mu.RUnlock()
	if guild == "" {
		var ch struct {
			GuildID string `json:"guild_id"`
		}
		if err := b.do(http.MethodGet, fmt.Sprintf("%s/channels/%s", discordAPIBase, threadID), nil, &ch); err != nil {
			return "", err
		}
		if ch.GuildID == "" {
			return "", fmt.Errorf("discord thread url: thread %s has no guild", threadID)
		}
		guild = ch.GuildID
		b.rememberGuild(threadID, guild)
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s", guild, threadID), nil
}

func (b *Bot) rememberGuild(threadID, guildID string) {
	if guildID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.guilds == nil {
		b.guilds = map[string]string{}
	}
	b.guilds[threadID] = guildID
}

// do sends one Discord REST call with the bot token. A 429 waits out the
// advertised retry_after (each wait capped at maxRetryAfter) and retries at
// most maxRateLimitRetries times; any other status >= 400 is an error. out,
// when non-nil, receives the decoded JSON body.
func (b *Bot) do(method, url string, payload, out any) error {
	var data []byte
	if payload != nil {
		var err error
		if data, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+b.token)
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.client.Do(req)
		if err != nil {
			return fmt.Errorf("discord send: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			rateLimitSleep(retryAfter(resp, raw))
			continue
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("discord API %d: %s", resp.StatusCode, string(raw))
		}
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("discord response: %w", err)
			}
		}
		return nil
	}
}

// retryAfter reads Discord's 429 wait from the JSON body, then the
// Retry-After header, defaulting to one second, capped at maxRetryAfter.
func retryAfter(resp *http.Response, body []byte) time.Duration {
	wait := time.Second
	var rl struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &rl) == nil && rl.RetryAfter > 0 {
		wait = time.Duration(rl.RetryAfter * float64(time.Second))
	} else if secs, err := strconv.ParseFloat(strings.TrimSpace(resp.Header.Get("Retry-After")), 64); err == nil && secs > 0 {
		wait = time.Duration(secs * float64(time.Second))
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	return wait
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
				b.enqueue(fmt.Sprintf("%s %s", prefix, b.agentCompletionText(agent, *old)))
			case agent.Busy == "working" && old.Busy == "idle":
				b.enqueue(fmt.Sprintf("%s Working%s%s", prefix, doing, b.agentDetailsSuffix(agent.Name)))
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

func (b *Bot) agentCompletionText(agent, old agentSnapshot) string {
	summary := paneclass.SanitizePaneText(agent.LiveSummary, 3)
	work := strings.TrimSpace(agent.Doing)
	if work == "" {
		work = strings.TrimSpace(old.Doing)
	}
	work = strings.TrimPrefix(strings.TrimPrefix(work, "done "), "Done ")
	work = truncateRunes(work, 100)
	duration := agentDurationSuffix(agent.LastKickAt)
	details := b.agentDetailsSuffix(agent.Name)

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
			runes := []rune(trimmed)
			if len(runes) > maxRunes {
				return string(runes[:maxRunes])
			}
			return trimmed
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

func (b *Bot) agentDetailsSuffix(name string) string {
	if strings.TrimSpace(b.dashboardURL) == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return " — details: " + strings.TrimRight(b.dashboardURL, "/") + "/#" + url.PathEscape(name)
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
