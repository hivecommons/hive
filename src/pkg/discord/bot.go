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
	"time"

	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	discordAPIBase = "https://discord.com/api/v10"

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
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

type Config struct {
	Token          string
	ChannelID      string
	DashboardURL   string
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
	content = logscrub.ScrubString(content)
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

	if err := b.responseError(resp); err != nil {
		return err
	}

	return nil
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
