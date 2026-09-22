package slack

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
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	slackAPIBase           = "https://slack.com/api"
	httpTimeoutS           = 10
	socketReconnectBase    = 5 * time.Second
	socketReconnectMax     = 60 * time.Second
	slackMessageLimit      = 4000
	slackDefaultSendPacing = 1200 * time.Millisecond
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

type Config struct {
	Enabled        bool
	AppToken       string
	BotToken       string
	ChannelID      string
	DashboardURL   string
	DashboardToken string
	AllowedUsers   []string
}

type Bot struct {
	*slackBackend
	service *chat.Service
}

type slackBackend struct {
	appToken  string
	botToken  string
	channelID string
	apiBase   string
	logger    *slog.Logger
	client    *http.Client
	dial      func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)

	sleep         func(time.Duration)
	reconnectBase time.Duration
	reconnectMax  time.Duration
}

type apiResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	URL   string `json:"url"`
}

type socketEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Reason     string          `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
}

type eventPayload struct {
	Event slackEvent `json:"event"`
}

type slackEvent struct {
	Type        string `json:"type"`
	Channel     string `json:"channel"`
	Text        string `json:"text"`
	User        string `json:"user"`
	ClientMsgID string `json:"client_msg_id"`
	TS          string `json:"ts"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
}

func SetAgentIdentities(identities map[string]AgentIdentity) { chat.SetAgentIdentities(identities) }
func SetAgentAliases(agentAliases map[string]string)         { chat.SetAgentAliases(agentAliases) }

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	backend := &slackBackend{
		appToken:      strings.TrimSpace(cfg.AppToken),
		botToken:      strings.TrimSpace(cfg.BotToken),
		channelID:     strings.TrimSpace(cfg.ChannelID),
		apiBase:       slackAPIBase,
		logger:        logger,
		client:        &http.Client{Timeout: httpTimeoutS * time.Second},
		dial:          websocket.DefaultDialer.DialContext,
		sleep:         time.Sleep,
		reconnectBase: socketReconnectBase,
		reconnectMax:  socketReconnectMax,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		MessageLimit:      slackMessageLimit,
		SendInterval:      slackDefaultSendPacing,
		HeartbeatInterval: 15 * time.Minute,
	}, logger)
	return &Bot{slackBackend: backend, service: service}
}

func (b *Bot) SetAgentNames(names []string) { b.service.SetAgentNames(names) }
func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.service.RegisterCommand(name, handler)
}

func (b *Bot) Start(ctx context.Context) error {
	if b.appToken == "" || b.botToken == "" || b.channelID == "" {
		return fmt.Errorf("slack app_token, bot_token, and channel_id must be configured")
	}
	b.logger.Info("slack bot starting", "channel", b.channelID)
	return b.service.Start(ctx)
}

func (b *Bot) SendMessage(content string) error { return b.Send(content) }
func (b *Bot) Listen(ctx context.Context, deliver func(chat.Message)) {
	b.slackBackend.Listen(ctx, deliver)
}
func (b *Bot) SetTopic(topic string) error { return b.slackBackend.SetTopic(topic) }

func (b *slackBackend) Name() string { return "slack" }

func (b *slackBackend) Send(content string) error {
	content = logscrub.ScrubString(content)
	for _, part := range splitSlackMessage(markdownToMrkdwn(content)) {
		if err := b.postJSON("/chat.postMessage", map[string]string{"channel": b.channelID, "text": part}); err != nil {
			return err
		}
	}
	return nil
}

func (b *slackBackend) SetTopic(topic string) error {
	err := b.postJSON("/conversations.setTopic", map[string]string{"channel": b.channelID, "topic": topic})
	if errors.Is(err, chat.ErrTopicUnsupported) {
		b.logger.Debug("slack topic update unsupported", "error", err)
	}
	return err
}

func (b *slackBackend) Listen(ctx context.Context, deliver func(chat.Message)) {
	delay := b.reconnectBase
	if delay == 0 {
		delay = socketReconnectBase
	}
	maxDelay := b.reconnectMax
	if maxDelay == 0 {
		maxDelay = socketReconnectMax
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		connected, err := b.consumeSocket(ctx, deliver)
		if err != nil && !errors.Is(err, context.Canceled) {
			b.logger.Warn("slack socket disconnected", "error", err)
		}
		if connected {
			delay = b.reconnectBase
			if delay == 0 {
				delay = socketReconnectBase
			}
		}
		if !sleepWithContext(ctx, b.sleep, delay) {
			return
		}
		if !connected {
			delay = min(delay*2, maxDelay)
		}
	}
}

// sleepWithContext waits for d using the injected sleep so tests can observe
// the computed reconnect delays without wall-clock timing; it returns false
// when ctx is canceled before the sleep completes.
func sleepWithContext(ctx context.Context, sleep func(time.Duration), d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if sleep == nil {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sleep(d)
	}()
	select {
	case <-ctx.Done():
		return false
	case <-done:
		return true
	}
}

func (b *slackBackend) consumeSocket(ctx context.Context, deliver func(chat.Message)) (bool, error) {
	url, err := b.openSocketURL(ctx)
	if err != nil {
		return false, err
	}
	h := http.Header{"Authorization": {"Bearer " + b.appToken}}
	conn, resp, err := b.dial(ctx, url, h)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, err
	}
	connCtx, stopConnWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-connCtx.Done()
		_ = conn.Close()
	}()
	defer func() {
		stopConnWatcher()
		<-watcherDone
		_ = conn.Close()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		var env socketEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.EnvelopeID != "" {
			if err := conn.WriteJSON(map[string]string{"envelope_id": env.EnvelopeID}); err != nil {
				b.logger.Warn("slack socket ack failed", "error", err, "envelope_id", env.EnvelopeID)
				continue
			}
		}
		if env.Type == "disconnect" || env.Type == "refresh_requested" || env.Reason == "refresh_requested" {
			return true, fmt.Errorf("slack socket refresh requested")
		}
		if env.Type != "events_api" {
			continue
		}
		var payload eventPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			continue
		}
		e := payload.Event
		if e.Type != "message" || e.Channel != b.channelID {
			continue
		}
		id := e.ClientMsgID
		if id == "" {
			id = e.TS
		}
		text, _ := ioscan.EnforceInput(e.Text)
		deliver(chat.Message{ID: id, Text: text, AuthorID: e.User, FromBot: e.BotID != "" || e.Subtype == "bot_message"})
	}
}

func (b *slackBackend) openSocketURL(ctx context.Context) (string, error) {
	data, err := b.callSlack(ctx, "/apps.connections.open", nil, b.appToken)
	if err != nil {
		return "", err
	}
	if data.URL == "" {
		return "", fmt.Errorf("slack apps.connections.open returned no url")
	}
	return data.URL, nil
}

func (b *slackBackend) postJSON(path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = b.callSlack(context.Background(), path, bytes.NewReader(data), b.botToken)
	return err
}

func (b *slackBackend) callSlack(ctx context.Context, path string, body io.Reader, token string) (apiResponse, error) {
	method := http.MethodPost
	req, err := http.NewRequestWithContext(ctx, method, b.apiBase+path, body)
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		if retry := retryAfter(resp.Header.Get("Retry-After")); retry > 0 {
			b.sleep(retry)
		}
		return apiResponse{}, fmt.Errorf("slack API 429: rate limited")
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return apiResponse{}, fmt.Errorf("slack API %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed apiResponse
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return apiResponse{}, err
		}
	}
	if !parsed.OK {
		if parsed.Error == "missing_scope" || parsed.Error == "not_allowed_token_type" {
			return parsed, chat.ErrTopicUnsupported
		}
		if parsed.Error == "" {
			parsed.Error = "not_ok"
		}
		return parsed, fmt.Errorf("slack API error: %s", parsed.Error)
	}
	return parsed, nil
}

func retryAfter(s string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func markdownToMrkdwn(s string) string {
	var out strings.Builder
	inCode := false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "`") {
			inCode = !inCode
			out.WriteByte('`')
			i++
			continue
		}
		if !inCode && strings.HasPrefix(s[i:], "**") {
			out.WriteByte('*')
			i += 2
			continue
		}
		if !inCode && s[i] == '[' {
			if text, url, width, ok := parseMarkdownLink(s[i:]); ok {
				out.WriteString("<" + url + "|" + text + ">")
				i += width
				continue
			}
		}
		out.WriteByte(s[i])
		i++
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

func splitSlackMessage(s string) []string {
	if len([]rune(s)) <= slackMessageLimit {
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
			part += "\n```"
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
				current = "```\n"
			}
			reserve := 0
			if inFence || strings.Count(current, "```")%2 == 1 || strings.Contains(segment, "```") {
				reserve = len([]rune("\n```"))
			}
			limit := slackMessageLimit - reserve
			if limit < 1 {
				limit = slackMessageLimit
			}
			available := limit - len([]rune(current))
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
		if inFence || strings.Contains(para, "```") {
			reserve = len([]rune("\n```"))
		}
		if len([]rune(current+para)) <= slackMessageLimit-reserve {
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
	if strings.Count(s, "```")%2 == 1 {
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
