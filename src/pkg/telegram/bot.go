package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/chat"
)

const (
	telegramAPIBase           = "https://api.telegram.org"
	httpTimeoutS              = 60
	telegramMessageLimit      = 4096
	telegramDefaultSendPacing = 1200 * time.Millisecond
	longPollTimeoutSeconds    = 50
	pollReconnectBase         = 5 * time.Second
	pollReconnectMax          = 60 * time.Second
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

type Config struct {
	Enabled        bool
	BotToken       string
	ChatID         string
	DashboardURL   string
	DashboardToken string
	AllowedUsers   []string
}

type Bot struct {
	*telegramBackend
	service *chat.Service
}

type telegramBackend struct {
	botToken    string
	chatID      string
	apiBase     string
	logger      *slog.Logger
	client      *http.Client
	sleep       func(time.Duration)
	backoffBase time.Duration
	backoffMax  time.Duration
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Parameters  responseParams  `json:"parameters"`
	Result      json.RawMessage `json:"result"`
}

type responseParams struct {
	RetryAfter int `json:"retry_after"`
}

type update struct {
	UpdateID int64           `json:"update_id"`
	Message  telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Text string `json:"text"`
}

func SetAgentIdentities(identities map[string]AgentIdentity) { chat.SetAgentIdentities(identities) }
func SetAgentAliases(agentAliases map[string]string)         { chat.SetAgentAliases(agentAliases) }

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	backend := &telegramBackend{
		botToken:    strings.TrimSpace(cfg.BotToken),
		chatID:      strings.TrimSpace(cfg.ChatID),
		apiBase:     telegramAPIBase,
		logger:      logger,
		client:      &http.Client{Timeout: httpTimeoutS * time.Second},
		sleep:       time.Sleep,
		backoffBase: pollReconnectBase,
		backoffMax:  pollReconnectMax,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		MessageLimit:      telegramMessageLimit,
		SendInterval:      telegramDefaultSendPacing,
		HeartbeatInterval: 15 * time.Minute,
	}, logger)
	return &Bot{telegramBackend: backend, service: service}
}

func (b *Bot) SetAgentNames(names []string) { b.service.SetAgentNames(names) }
func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.service.RegisterCommand(name, handler)
}

func (b *Bot) Start(ctx context.Context) error {
	if b.botToken == "" || b.chatID == "" {
		return fmt.Errorf("telegram bot_token and chat_id must be configured")
	}
	b.logger.Info("telegram bot starting", "chat", b.chatID)
	return b.service.Start(ctx)
}

func (b *Bot) SendMessage(content string) error { return b.Send(content) }
func (b *Bot) Listen(ctx context.Context, deliver func(chat.Message)) {
	b.telegramBackend.Listen(ctx, deliver)
}
func (b *Bot) SetTopic(topic string) error { return b.telegramBackend.SetTopic(topic) }

func (b *telegramBackend) Name() string { return "telegram" }

func (b *telegramBackend) Send(content string) error {
	// Telegram's HTML parse mode has a much smaller escaping surface than MarkdownV2,
	// so the backend translates the spine's Markdown-ish messages to HTML here.
	for _, part := range splitTelegramMessage(markdownToHTML(content)) {
		payload := map[string]string{"chat_id": b.chatID, "text": part, "parse_mode": "HTML"}
		if err := b.callTelegram(context.Background(), "sendMessage", payload, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *telegramBackend) SetTopic(topic string) error {
	b.logger.Debug("telegram topic update unsupported", "topic", topic)
	return nil
}

func (b *telegramBackend) Listen(ctx context.Context, deliver func(chat.Message)) {
	delay := b.backoffBase
	if delay == 0 {
		delay = pollReconnectBase
	}
	maxDelay := b.backoffMax
	if maxDelay == 0 {
		maxDelay = pollReconnectMax
	}
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		polled, err := b.pollOnce(ctx, offset, deliver, func(next int64) { offset = next })
		if err != nil {
			b.logger.Warn("telegram poll failed", "error", err)
		} else if polled {
			delay = b.backoffBase
			if delay == 0 {
				delay = pollReconnectBase
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err != nil {
			delay = min(delay*2, maxDelay)
		}
	}
}

func (b *telegramBackend) pollOnce(ctx context.Context, offset int64, deliver func(chat.Message), advance func(int64)) (bool, error) {
	payload := map[string]any{"timeout": longPollTimeoutSeconds, "allowed_updates": []string{"message"}}
	if offset > 0 {
		payload["offset"] = offset
	}
	var updates []update
	if err := b.callTelegram(ctx, "getUpdates", payload, &updates); err != nil {
		return false, err
	}
	for _, upd := range updates {
		b.handleUpdate(upd, deliver)
		advance(upd.UpdateID + 1)
	}
	return true, nil
}

func (b *telegramBackend) handleUpdate(upd update, deliver func(chat.Message)) {
	msg := upd.Message
	if strconv.FormatInt(msg.Chat.ID, 10) != b.chatID || msg.Text == "" {
		return
	}
	deliver(chat.Message{
		ID:       strconv.FormatInt(msg.MessageID, 10),
		Text:     msg.Text,
		AuthorID: strconv.FormatInt(msg.From.ID, 10),
		FromBot:  msg.From.IsBot,
	})
}

func (b *telegramBackend) callTelegram(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiBase+"/bot"+b.botToken+"/"+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed apiResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			return err
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if parsed.Parameters.RetryAfter > 0 {
			b.sleep(time.Duration(parsed.Parameters.RetryAfter) * time.Second)
		}
		return fmt.Errorf("telegram API 429: %s", parsed.Description)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram API %d: %s", resp.StatusCode, string(body))
	}
	if !parsed.OK {
		if parsed.Description == "" {
			parsed.Description = "not ok"
		}
		return fmt.Errorf("telegram API error: %s", parsed.Description)
	}
	if result != nil && len(parsed.Result) > 0 {
		if err := json.Unmarshal(parsed.Result, result); err != nil {
			return err
		}
	}
	return nil
}

func markdownToHTML(s string) string {
	var out strings.Builder
	inCode := false
	inFence := false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "```") {
			if inFence {
				out.WriteString("</pre>")
			} else {
				out.WriteString("<pre>")
			}
			inFence = !inFence
			i += 3
			continue
		}
		if !inFence && strings.HasPrefix(s[i:], "`") {
			if inCode {
				out.WriteString("</code>")
			} else {
				out.WriteString("<code>")
			}
			inCode = !inCode
			i++
			continue
		}
		if !inCode && !inFence && strings.HasPrefix(s[i:], "**") {
			end := strings.Index(s[i+2:], "**")
			if end < 0 {
				out.WriteString("**")
				i += 2
				continue
			}
			i += 2
			out.WriteString("<b>")
			out.WriteString(html.EscapeString(s[i : i+end]))
			out.WriteString("</b>")
			i += end + 2
			continue
		}
		if !inCode && !inFence && s[i] == '[' {
			if text, url, width, ok := parseMarkdownLink(s[i:]); ok {
				out.WriteString(`<a href="` + html.EscapeString(url) + `">` + html.EscapeString(text) + `</a>`)
				i += width
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		out.WriteString(html.EscapeString(s[i : i+size]))
		i += size
	}
	return out.String()
}

func parseMarkdownLink(s string) (text, url string, width int, ok bool) {
	closeText := strings.Index(s, "](")
	if closeText <= 1 {
		return "", "", 0, false
	}
	closeURL := strings.IndexByte(s[closeText+2:], ')')
	if closeURL < 1 {
		return "", "", 0, false
	}
	closeURL += closeText + 2
	return s[1:closeText], s[closeText+2 : closeURL], closeURL + 1, true
}

func splitTelegramMessage(s string) []string {
	return splitFencedMessage(s, telegramMessageLimit)
}

func splitFencedMessage(s string, limit int) []string {
	if len([]rune(s)) <= limit {
		return []string{s}
	}
	var parts []string
	current := ""
	inFence := false

	emit := func() {
		if current == "" {
			return
		}
		part := current
		if inFence {
			part += "\n</pre>"
		}
		parts = append(parts, part)
		current = ""
	}

	appendSegment := func(segment string) {
		for segment != "" {
			if current == "" && strings.HasPrefix(segment, "\n\n") {
				segment = strings.TrimPrefix(segment, "\n\n")
			}
			if current == "" && inFence {
				current = "<pre>\n"
			}
			reserve := 0
			if inFence || strings.Count(current, "<pre>") > strings.Count(current, "</pre>") || strings.Contains(segment, "<pre>") {
				reserve = len([]rune("\n</pre>"))
			}
			available := limit - reserve - len([]rune(current))
			if available <= 0 {
				emit()
				continue
			}
			if len([]rune(segment)) <= available {
				current += segment
				inFence = updateFenceState(inFence, segment)
				return
			}
			chunk := takeRunes(segment, available)
			current += chunk
			inFence = updateFenceState(inFence, chunk)
			segment = strings.TrimPrefix(segment, chunk)
			emit()
		}
	}

	for i, para := range strings.Split(s, "\n\n") {
		if i > 0 {
			para = "\n\n" + para
		}
		if current == "" && inFence {
			appendSegment(para)
			continue
		}
		reserve := 0
		if inFence || strings.Contains(para, "<pre>") {
			reserve = len([]rune("\n</pre>"))
		}
		if len([]rune(current+para)) <= limit-reserve {
			current += para
			inFence = updateFenceState(inFence, para)
		} else {
			emit()
			appendSegment(para)
		}
	}
	emit()
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

func updateFenceState(inFence bool, s string) bool {
	if (strings.Count(s, "<pre>")-strings.Count(s, "</pre>"))%2 != 0 {
		return !inFence
	}
	return inFence
}

func takeRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	idx := 0
	for count := 0; idx < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[idx:])
		idx += size
	}
	return s[:idx]
}
