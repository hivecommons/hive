package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	if err := discordAPIError(resp); err != nil {
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

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("topic update %d: %s", resp.StatusCode, string(body))
	}
	return nil
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messages, err := b.fetchMessages(ctx, lastMessageID)
			if err != nil {
				b.logger.Warn("discord poll failed", "error", err)
				// A rate-limited poll waits out Discord's retry_after before the
				// next tick so the poller does not keep tripping the same bucket.
				var retryable *chat.RetryableError
				if errors.As(err, &retryable) && retryable.RetryAfter > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(retryable.RetryAfter):
					}
				}
				continue
			}

			for i := len(messages) - 1; i >= 0; i-- {
				msg := messages[i]
				lastMessageID = msg.ID
				if !firstPoll {
					deliver(discordChatMessage(msg))
				}
			}
			firstPoll = false
		}
	}
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

	if err := discordAPIError(resp); err != nil {
		return nil, err
	}

	var messages []discordMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// discordAPIError maps a >= 400 response to an error. A 429 carries the
// `retry_after` Discord reports (seconds, fractional; the `Retry-After`
// header is the integer fallback) as a chat.RetryableError so the spine can
// wait it out and resend instead of dropping the message; 5xx responses are
// retryable with no hint. Other statuses are terminal.
func discordAPIError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	err := fmt.Errorf("discord API %d: %s", resp.StatusCode, string(body))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return chat.Retryable(err, discordRetryAfter(body, resp.Header.Get("Retry-After")))
	case resp.StatusCode >= 500:
		return chat.Retryable(err, 0)
	}
	return err
}

// discordRetryAfter reads the wait from a 429 body's `retry_after` field,
// falling back to the Retry-After header. Zero means no usable hint.
func discordRetryAfter(body []byte, header string) time.Duration {
	var rl struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &rl) == nil && rl.RetryAfter > 0 {
		return time.Duration(rl.RetryAfter * float64(time.Second))
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}
