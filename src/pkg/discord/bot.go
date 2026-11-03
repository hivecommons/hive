package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
)

const (
	discordAPIBase        = "https://discord.com/api/v10"
	discordSuppressEmbeds = 1 << 2

	httpTimeoutS  = 10
	pollIntervalS = 5

	// pollBackoffMaxFactor bounds Listen's error backoff at pollBackoffMaxFactor
	// * pollInterval (12 * 5s = 60s in production), so a revoked token or an
	// outage warns with decreasing frequency instead of every poll forever
	// (hivecommons/hive#9142).
	pollBackoffMaxFactor = 12
	// pollBackoffJitterFrac randomizes each backoff delay by up to this
	// fraction so a fleet of bots hitting the same outage doesn't retry in
	// lockstep.
	pollBackoffJitterFrac = 0.2
	msgQueueIntervalMS    = 1200
	heartbeatInterval     = 15 * time.Minute
	topicDebounceMS       = 5000
	discordMsgLimit       = 1900
	sseReconnectBase      = 5 * time.Second
	sseReconnectMax       = 60 * time.Second
	nonceLimit            = 25
	threadNameLimit       = 100
	historyLimit          = 50
	maxRateLimitRetries   = 3
	maxRetryAfter         = 15 * time.Second
	threadExistsCode      = "160004"
	maxResponseBytes      = 1 << 20
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

var rateLimitSleep = time.Sleep

type Config struct {
	Token          string
	ChannelID      string
	DashboardURL   string
	PublicURL      string // user-facing dashboard origin; empty omits notification links
	DashboardToken string
	// AllowedUsers is the set of Discord user IDs permitted to issue commands.
	// Empty = commands disabled (fail closed).
	AllowedUsers []string
	// PersonaStore persists each author's persona; nil keeps personas in
	// memory for this process only (hivecommons/hive#9175).
	PersonaStore chat.PersonaStore
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
}

type Bot struct {
	*discordBackend
	service *chat.Service
	mu      sync.RWMutex
	self    string
	guilds  map[string]string
}

type discordBackend struct {
	token     string
	channelID string
	logger    *slog.Logger
	client    *http.Client
	// pollInterval is the Listen ticker period; zero means pollIntervalS seconds.
	pollInterval time.Duration
}

type discordMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
	Embeds      []json.RawMessage `json:"embeds"`
	Attachments []json.RawMessage `json:"attachments"`
}

// discordAPIError carries the HTTP status and any Retry-After hint from a
// Discord API response so callers can distinguish a 429 (back off, maybe
// retry) from a 401/403 (won't succeed until reconfigured) or a transient
// 5xx (hivecommons/hive#9142).
type discordAPIError struct {
	StatusCode int
	RetryAfter time.Duration
	Body       string
}

func (e *discordAPIError) Error() string {
	return fmt.Sprintf("discord API %d: %s", e.StatusCode, e.Body)
}

func SetAgentIdentities(identities map[string]AgentIdentity) {
	chat.SetAgentIdentities(identities)
}

func SetAgentAliases(agentAliases map[string]string) {
	chat.SetAgentAliases(agentAliases)
}

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	backend := &discordBackend{
		token:     cfg.Token,
		channelID: cfg.ChannelID,
		logger:    logger,
		client: &http.Client{
			Timeout: httpTimeoutS * time.Second,
		},
		pollInterval: pollIntervalS * time.Second,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:    cfg.DashboardURL,
		PublicURL:       cfg.PublicURL,
		DashboardToken:  cfg.DashboardToken,
		AllowedUsers:    cfg.AllowedUsers,
		PersonaStore:    cfg.PersonaStore,
		PersonaLearning: cfg.PersonaLearning,
		AuditSink:       cfg.AuditSink,
	}, logger)
	return &Bot{discordBackend: backend, service: service}
}

func (b *Bot) SetAgentNames(names []string) {
	b.service.SetAgentNames(names)
}

func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.service.RegisterCommand(name, handler)
}

func (b *Bot) Start(ctx context.Context) error {
	if b.token == "" {
		return fmt.Errorf("discord bot token not configured")
	}

	b.logger.Info("discord bot starting", "channel", b.channelID)
	return b.service.Start(ctx)
}

func (b *Bot) SendMessage(content string) error {
	return b.discordBackend.Send(content)
}

func (b *Bot) SetTopic(topic string) error {
	return b.discordBackend.SetTopic(topic)
}

func (b *Bot) Listen(ctx context.Context, deliver func(chat.Message)) {
	b.discordBackend.Listen(ctx, deliver)
}

func (b *discordBackend) Name() string {
	return "discord"
}

func (b *discordBackend) Send(content string) error {
	return b.sendTo(b.channelID, content)
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
		"content":          logscrub.ScrubString(content),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"flags":            discordSuppressEmbeds,
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

// EditMessage replaces the content of a message the bot posted in
// channelID (PATCH /channels/{c}/messages/{m}). Like SendMessageTo it never
// pings anyone. A 429 is retried within the same bound as every call.
func (b *Bot) EditMessage(channelID, messageID, content string) error {
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("discord edit: channel id and message id are required")
	}
	url := fmt.Sprintf("%s/channels/%s/messages/%s", discordAPIBase, channelID, messageID)
	return b.do(http.MethodPatch, url, messagePayload(content), nil)
}

// RenameChannel renames a channel or thread (PATCH /channels/{c}). The name
// is truncated to Discord's 100 characters. A 429 is retried within the
// same bound as every call.
func (b *Bot) RenameChannel(channelID, name string) error {
	name = truncateRunes(strings.TrimSpace(name), threadNameLimit)
	if strings.TrimSpace(channelID) == "" || name == "" {
		return fmt.Errorf("discord rename: channel id and name are required")
	}
	return b.do(http.MethodPatch, fmt.Sprintf("%s/channels/%s", discordAPIBase, channelID), map[string]any{"name": name}, nil)
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

func (b *discordBackend) SetTopic(topic string) error {
	return b.setChannelTopic(topic)
}

func (b *discordBackend) setChannelTopic(topic string) error {
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

	if err := b.responseError(resp); err != nil {
		return err
	}
	return nil
}

// responseError maps a >=400 response to an error; it reads (but does not
// close) resp.Body and returns nil for non-error responses. A 429 carries
// Discord's retry_after hint and a 5xx a zero hint as a chat.RetryableError,
// so the chat spine's drain loop waits it out and resends instead of
// dropping the message (hivecommons/hive#9127/#9142); the wrapped
// *discordAPIError keeps the status code readable for Listen's log-level
// decisions. Other statuses are terminal.
func (b *discordBackend) responseError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &discordAPIError{StatusCode: resp.StatusCode, Body: string(body)}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		apiErr.RetryAfter = discordRetryAfter(body, resp.Header.Get("Retry-After"))
		return chat.Retryable(apiErr, apiErr.RetryAfter)
	case resp.StatusCode >= 500:
		return chat.Retryable(apiErr, 0)
	}
	return apiErr
}

// discordRetryAfter extracts a 429's retry delay, preferring the JSON body's
// "retry_after" (seconds, possibly fractional, per Discord's rate-limit
// response shape) and falling back to the Retry-After header. Zero means no
// usable hint.
func discordRetryAfter(body []byte, header string) time.Duration {
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.RetryAfter > 0 {
		return time.Duration(parsed.RetryAfter * float64(time.Second))
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}

func (b *discordBackend) Listen(ctx context.Context, deliver func(chat.Message)) {
	interval := b.pollInterval
	if interval <= 0 {
		interval = pollIntervalS * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastMessageID string
	firstPoll := true
	// consecutiveFailures drives backoffDelay; nextAttempt lets a slow tick
	// interval skip polls that land inside an active backoff window instead
	// of firing on every tick regardless of backoff (hivecommons/hive#9142).
	consecutiveFailures := 0
	var nextAttempt time.Time
	authErrorLogged := false
	intentWarned := false

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if consecutiveFailures > 0 && now.Before(nextAttempt) {
				continue
			}

			messages, err := b.fetchMessages(ctx, lastMessageID)
			if err != nil {
				consecutiveFailures++
				delay := backoffDelay(interval, consecutiveFailures)
				var retryable *chat.RetryableError
				if errors.As(err, &retryable) && retryable.RetryAfter > delay {
					delay = retryable.RetryAfter
				}
				nextAttempt = time.Now().Add(delay)

				var apiErr *discordAPIError
				if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
					// A bad token or missing permissions will not clear on its
					// own; logging every 5s forever (~17k lines/day) drowns
					// out everything else, so only the first occurrence is an
					// Error and later ones are Debug while backoff still
					// grows to the cap.
					if !authErrorLogged {
						b.logger.Error("discord poll failed: authorization rejected (check bot token / channel permissions)", "error", err, "next_retry", delay)
						authErrorLogged = true
					} else {
						b.logger.Debug("discord poll still failing: authorization rejected", "error", err, "next_retry", delay)
					}
				} else {
					b.logger.Warn("discord poll failed", "error", err, "next_retry", delay)
				}
				continue
			}
			consecutiveFailures = 0
			authErrorLogged = false

			for i := len(messages) - 1; i >= 0; i-- {
				msg := messages[i]
				lastMessageID = msg.ID
				if !firstPoll {
					if !intentWarned && missingMessageContent(msg) {
						// hivecommons/hive#9141: an app without the
						// MESSAGE_CONTENT privileged intent gets empty
						// content/embeds/attachments on every message, REST
						// reads included. Nothing else distinguishes this
						// from a legitimately empty message, so warn once
						// rather than per-message.
						b.logger.Warn("discord message has empty content/embeds/attachments; the MESSAGE_CONTENT privileged intent may not be enabled (Developer Portal → Bot → Privileged Gateway Intents)", "message_id", msg.ID, "author_id", msg.Author.ID)
						intentWarned = true
					}
					deliver(discordChatMessage(msg))
				}
			}
			firstPoll = false
		}
	}
}

// backoffDelay returns the delay before the next poll attempt after
// `failures` consecutive errors: exponential growth from one poll interval,
// capped at pollBackoffMaxFactor intervals, with up to pollBackoffJitterFrac
// jitter so a fleet of bots hitting the same outage doesn't retry in lockstep
// (hivecommons/hive#9142).
func backoffDelay(interval time.Duration, failures int) time.Duration {
	if interval <= 0 {
		interval = pollIntervalS * time.Second
	}
	if failures < 1 {
		failures = 1
	}
	shift := failures - 1
	if shift > pollBackoffMaxFactor {
		shift = pollBackoffMaxFactor
	}
	max := interval * pollBackoffMaxFactor
	delay := interval * time.Duration(int64(1)<<uint(shift))
	if delay > max || delay <= 0 {
		delay = max
	}
	jitter := time.Duration(rand.Int63n(int64(float64(delay) * pollBackoffJitterFrac)))
	return delay + jitter
}

// missingMessageContent reports whether a non-bot message looks like it was
// affected by a missing MESSAGE_CONTENT intent: empty content with no embeds
// or attachments either, which is otherwise possible only for a genuinely
// blank message.
func missingMessageContent(msg discordMessage) bool {
	return !msg.Author.Bot && msg.Content == "" && len(msg.Embeds) == 0 && len(msg.Attachments) == 0
}

func discordChatMessage(msg discordMessage) chat.Message {
	text, _ := ioscan.EnforceInput(msg.Content)
	return chat.Message{ID: msg.ID, Text: text, AuthorID: msg.Author.ID, FromBot: msg.Author.Bot}
}

func (b *discordBackend) fetchMessages(ctx context.Context, after string) ([]discordMessage, error) {
	url := fmt.Sprintf("%s/channels/%s/messages?limit=10", discordAPIBase, b.channelID)
	if after != "" {
		url += "&after=" + after
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if apiErr := b.responseError(resp); apiErr != nil {
		return nil, apiErr
	}

	var messages []discordMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, err
	}
	return messages, nil
}
