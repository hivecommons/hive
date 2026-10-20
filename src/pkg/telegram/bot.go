package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	telegramAPIBase           = "https://api.telegram.org"
	httpTimeoutS              = 60
	telegramMessageLimit      = 4096
	telegramDefaultSendPacing = 1200 * time.Millisecond
	longPollTimeoutSeconds    = 50
	pollReconnectBase         = 5 * time.Second
	pollReconnectMax          = 60 * time.Second
	maxRetryAfter             = 60 * time.Second
	// rateLimitRetries is how many times a 429'd request is re-issued after
	// waiting out retry_after before the error is returned.
	rateLimitRetries = 1
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

type Config struct {
	Enabled        bool
	BotToken       string
	ChatID         string
	DashboardURL   string
	PublicURL      string // user-facing dashboard origin; empty omits notification links
	DashboardToken string
	AllowedUsers   []string
	// PersonaStore persists each author's persona; nil keeps personas in
	// memory for this process only (hivecommons/hive#9175).
	PersonaStore chat.PersonaStore
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
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
	sleep       func(context.Context, time.Duration) error
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
		sleep:       sleepContext,
		backoffBase: pollReconnectBase,
		backoffMax:  pollReconnectMax,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		PublicURL:         cfg.PublicURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaStore:      cfg.PersonaStore,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
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
	content = logscrub.ScrubString(content)
	for _, part := range splitTelegramMessage(markdownToHTML(content)) {
		payload := map[string]string{"chat_id": b.chatID, "text": part, "parse_mode": "HTML"}
		err := b.callTelegram(context.Background(), "sendMessage", payload, nil)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusBadRequest {
			// Telegram rejects the whole message when it cannot parse the
			// entities; resend the part as plain text so the content still
			// lands (hivecommons/hive#9158).
			b.logger.Warn("telegram rejected HTML message; resending as plain text", "error", err)
			plain := map[string]string{"chat_id": b.chatID, "text": htmlToPlainText(part)}
			err = b.callTelegram(context.Background(), "sendMessage", plain, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *telegramBackend) SetTopic(topic string) error {
	topic = logscrub.ScrubString(topic)
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
	primed := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var polled bool
		var err error
		if primed {
			polled, err = b.pollOnce(ctx, offset, deliver, func(next int64) { offset = next })
		} else if offset, err = b.skipBacklog(ctx); err == nil {
			primed, polled = true, true
		}
		if err != nil {
			b.logger.Warn("telegram poll failed", "error", err)
		} else if polled {
			delay = b.backoffBase
			if delay == 0 {
				delay = pollReconnectBase
			}
			continue
		}
		if b.sleep(ctx, delay) != nil {
			return
		}
		if err != nil {
			delay = min(delay*2, maxDelay)
		}
	}
}

// skipBacklog drops updates queued before this process started and returns the
// offset to poll from. The offset is only held in memory, so without this a
// restart would replay every unconfirmed update — including a !kick that was
// being handled at crash time, or hours-old commands typed during downtime
// (hivecommons/hive#9158). offset=-1 makes Telegram forget all but the last
// update; confirming that one via the returned offset drops it too. This
// mirrors Matrix skipping its initial sync.
func (b *telegramBackend) skipBacklog(ctx context.Context) (int64, error) {
	payload := map[string]any{"offset": -1, "timeout": 0, "allowed_updates": []string{"message"}}
	var updates []update
	if err := b.callTelegram(ctx, "getUpdates", payload, &updates); err != nil {
		return 0, err
	}
	var next int64
	for _, upd := range updates {
		next = max(next, upd.UpdateID+1)
	}
	if next > 0 {
		b.logger.Info("telegram skipped updates queued before start", "next_offset", next)
	}
	return next, nil
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
		if offset > 0 && upd.UpdateID < offset {
			// Already confirmed; never run a command twice.
			continue
		}
		b.handleUpdate(upd, deliver)
		offset = upd.UpdateID + 1
		advance(offset)
	}
	return true, nil
}

func (b *telegramBackend) handleUpdate(upd update, deliver func(chat.Message)) {
	msg := upd.Message
	if strconv.FormatInt(msg.Chat.ID, 10) != b.chatID || msg.Text == "" {
		return
	}
	text, _ := ioscan.EnforceInput(msg.Text)
	deliver(chat.Message{
		ID:       strconv.FormatInt(msg.MessageID, 10),
		Text:     text,
		AuthorID: strconv.FormatInt(msg.From.ID, 10),
		FromBot:  msg.From.IsBot,
	})
}

// apiError is a non-2xx Bot API response; status lets callers tell a rejected
// request (400) from a transient failure.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// callTelegram re-issues a request once after a 429's retry_after wait, so the
// wait buys a delivery instead of preceding a drop (hivecommons/hive#9158).
func (b *telegramBackend) callTelegram(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		retryAfter, err := b.callTelegramOnce(ctx, method, data, result)
		if retryAfter < 0 || attempt >= rateLimitRetries {
			return err
		}
		if retryAfter > 0 {
			if werr := b.sleep(ctx, min(retryAfter, maxRetryAfter)); werr != nil {
				return b.redactError(fmt.Errorf("telegram %s retry wait: %w", method, werr))
			}
		}
	}
}

// callTelegramOnce issues one request. retryAfter is >= 0 only for a 429, and
// is the server-requested wait before the request may be re-issued.
func (b *telegramBackend) callTelegramOnce(ctx context.Context, method string, data []byte, result any) (retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiBase+"/bot"+b.botToken+"/"+method, bytes.NewReader(data))
	if err != nil {
		return -1, b.redactError(fmt.Errorf("telegram %s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok {
			return -1, b.redactError(fmt.Errorf("telegram %s: %w", method, urlErr.Err))
		}
		return -1, b.redactError(fmt.Errorf("telegram %s: %w", method, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed apiResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			return -1, err
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := max(time.Duration(parsed.Parameters.RetryAfter)*time.Second, 0)
		return wait, &apiError{status: resp.StatusCode, msg: b.redactError(fmt.Errorf("telegram API 429: %s", parsed.Description)).Error()}
	}
	if resp.StatusCode >= 400 {
		return -1, &apiError{status: resp.StatusCode, msg: b.redactError(fmt.Errorf("telegram API %d: %s", resp.StatusCode, string(body))).Error()}
	}
	if !parsed.OK {
		if parsed.Description == "" {
			parsed.Description = "not ok"
		}
		return -1, b.redactError(fmt.Errorf("telegram API error: %s", parsed.Description))
	}
	if result != nil && len(parsed.Result) > 0 {
		if err := json.Unmarshal(parsed.Result, result); err != nil {
			return -1, err
		}
	}
	return -1, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *telegramBackend) redactError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if b.botToken != "" {
		msg = strings.ReplaceAll(msg, b.botToken, "<redacted>")
	}
	return fmt.Errorf("%s", msg)
}

// markdownToHTML always returns balanced HTML: Telegram rejects the whole
// message when a <code>/<pre> is left open, so an unmatched backtick or fence
// is closed at the end, and code spans never nest (backticks inside inline code
// are literal) (hivecommons/hive#9158).
func markdownToHTML(s string) string {
	var out strings.Builder
	inCode := false
	inFence := false
	for i := 0; i < len(s); {
		if !inCode && strings.HasPrefix(s[i:], "```") {
			i += 3
			if inFence {
				out.WriteString("</pre>")
			} else {
				out.WriteString("<pre>")
				i += fenceInfoLen(s[i:])
			}
			inFence = !inFence
			continue
		}
		if inCode && strings.HasPrefix(s[i:], "```") {
			out.WriteString("```")
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
	if inCode {
		out.WriteString("</code>")
	}
	if inFence {
		out.WriteString("</pre>")
	}
	return out.String()
}

// fenceInfoLen returns the length of a fence's language tag ("go" in
// "```go\n"), which is dropped rather than rendered as the block's first line.
// Anything other than a single word followed by a newline is block content.
func fenceInfoLen(s string) int {
	end := strings.IndexByte(s, '\n')
	if end <= 0 {
		return 0
	}
	for _, r := range s[:end] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("+-_.#", r) {
			return 0
		}
	}
	return end
}

// htmlToPlainText undoes markdownToHTML for a plain-text resend: tags are
// dropped, entities unescaped, and a link keeps its target as "text (url)".
func htmlToPlainText(s string) string {
	var out strings.Builder
	var href string
	for pos := 0; pos < len(s); {
		token, width := nextHTMLToken(s[pos:])
		pos += width
		name, closing, ok := telegramHTMLTag(token)
		switch {
		case !ok:
			out.WriteString(html.UnescapeString(token))
		case name == "a" && !closing:
			href = linkTarget(token)
		case name == "a" && closing && href != "":
			out.WriteString(" (" + href + ")")
			href = ""
		}
	}
	return out.String()
}

func linkTarget(tag string) string {
	const attr = `href="`
	start := strings.Index(tag, attr)
	if start < 0 {
		return ""
	}
	rest := tag[start+len(attr):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return html.UnescapeString(rest[:end])
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
	var current strings.Builder
	var openTags []htmlTag

	emit := func(force bool) {
		if !force && current.Len() == 0 {
			return
		}
		part := current.String() + closeTags(openTags)
		if part != "" {
			parts = append(parts, part)
		}
		current.Reset()
		current.WriteString(reopenTags(openTags))
	}

	for pos := 0; pos < len(s); {
		token, width := nextHTMLToken(s[pos:])
		nextOpen := updateOpenTags(openTags, token)
		if current.Len() > 0 && len([]rune(current.String()+token+closeTags(nextOpen))) > limit {
			emit(false)
			if current.Len() > 0 && len([]rune(current.String()+token+closeTags(nextOpen))) > limit {
				parts = append(parts, current.String()+closeTags(openTags))
				current.Reset()
				current.WriteString(reopenTags(openTags))
			}
		}
		current.WriteString(token)
		openTags = nextOpen
		pos += width
	}
	if current.Len() > 0 {
		emit(false)
	}
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

type htmlTag struct {
	name string
	open string
}

func nextHTMLToken(s string) (string, int) {
	if s == "" {
		return "", 0
	}
	if s[0] == '<' {
		if end := strings.IndexByte(s, '>'); end >= 0 {
			return s[:end+1], end + 1
		}
	}
	if s[0] == '&' {
		if end := strings.IndexByte(s, ';'); end >= 0 {
			return s[:end+1], end + 1
		}
	}
	_, size := utf8.DecodeRuneInString(s)
	return s[:size], size
}

func updateOpenTags(tags []htmlTag, token string) []htmlTag {
	name, closing, ok := telegramHTMLTag(token)
	if !ok {
		return tags
	}
	next := append([]htmlTag(nil), tags...)
	if closing {
		for i := len(next) - 1; i >= 0; i-- {
			if next[i].name == name {
				return append(next[:i], next[i+1:]...)
			}
		}
		return next
	}
	return append(next, htmlTag{name: name, open: token})
}

func telegramHTMLTag(token string) (name string, closing bool, ok bool) {
	if !strings.HasPrefix(token, "<") || !strings.HasSuffix(token, ">") {
		return "", false, false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(token, "<"), ">"))
	if inner == "" {
		return "", false, false
	}
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(strings.TrimPrefix(inner, "/"))
	}
	name = strings.Fields(inner)[0]
	switch name {
	case "b", "i", "code", "a", "pre":
		return name, closing, true
	default:
		return "", false, false
	}
}

func closeTags(tags []htmlTag) string {
	var out strings.Builder
	for i := len(tags) - 1; i >= 0; i-- {
		out.WriteString("</")
		out.WriteString(tags[i].name)
		out.WriteString(">")
	}
	return out.String()
}

func reopenTags(tags []htmlTag) string {
	var out strings.Builder
	for _, tag := range tags {
		out.WriteString(tag.open)
	}
	return out.String()
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
