package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/chat"
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
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:   cfg.DashboardURL,
		DashboardToken: cfg.DashboardToken,
		AllowedUsers:   cfg.AllowedUsers,
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
					deliver(chat.Message{ID: msg.ID, Text: msg.Content, AuthorID: msg.Author.ID, FromBot: msg.Author.Bot})
				}
			}
			firstPoll = false
		}
	}
}

func (b *discordBackend) fetchMessages(after string) ([]discordMessage, error) {
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
