package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
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
	slackAPIBase        = "https://slack.com/api"
	httpTimeoutS        = 10
	socketReconnectBase = 5 * time.Second
	socketReconnectMax  = 60 * time.Second
	socketAckTimeout    = 2 * time.Second
	// socketReadTimeout bounds how long a socket may stay silent — no data,
	// ping, or pong frame — before it is treated as dead. socketPingInterval
	// keeps a healthy idle socket inside that bound by eliciting pongs, so a
	// path that died without a FIN (NAT/VPN change, idle-killing middlebox)
	// is detected within socketReadTimeout instead of hanging ReadMessage.
	socketReadTimeout  = 90 * time.Second
	socketPingInterval = 30 * time.Second
	// socketBackoffJitter spreads reconnect sleeps by ±20% so hives that lose
	// Slack together do not retry in lockstep.
	socketBackoffJitter    = 0.2
	slackMessageLimit      = 4000
	socketQueueSize        = 256
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
	// PersonaStore persists each author's persona; nil keeps personas in
	// memory for this process only (hivecommons/hive#9175).
	PersonaStore chat.PersonaStore
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
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
	jitter        func(time.Duration) time.Duration
	reconnectBase time.Duration
	reconnectMax  time.Duration
	readTimeout   time.Duration
	pingInterval  time.Duration
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
		jitter:        jitterBackoff,
		reconnectBase: socketReconnectBase,
		reconnectMax:  socketReconnectMax,
		readTimeout:   socketReadTimeout,
		pingInterval:  socketPingInterval,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaStore:      cfg.PersonaStore,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
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
	// One worker preserves command order across socket reconnects. The reader
	// never waits for a command handler, so it can keep acknowledging and ponging.
	queue := make(chan chat.Message, socketQueueSize)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-queue:
				if ctx.Err() != nil {
					return
				}
				deliver(msg)
			}
		}
	}()
	defer func() { <-workerDone }()
	delay := b.reconnectBase
	if delay == 0 {
		delay = socketReconnectBase
	}
	maxDelay := b.reconnectMax
	if maxDelay == 0 {
		maxDelay = socketReconnectMax
	}
	// next is a replacement socket opened while the previous one was still
	// serving, after Slack announced a disconnect; it is used without a sleep.
	var next *websocket.Conn
	for {
		if ctx.Err() != nil {
			if next != nil {
				_ = next.Close()
			}
			return
		}
		conn := next
		next = nil
		var err error
		if conn == nil {
			conn, err = b.dialSocket(ctx)
		}
		connected := err == nil
		if connected {
			next, err = b.serveSocket(ctx, conn, queue)
		}
		if next != nil {
			if err != nil {
				b.logger.Warn("slack socket ended during refresh", "error", err)
			}
			b.logger.Info("slack socket refreshed")
			delay = b.reconnectBase
			if delay == 0 {
				delay = socketReconnectBase
			}
			continue
		}
		// A canceled ctx closes the socket from under ReadMessage, which then
		// reports net.ErrClosed; that is the clean shutdown path, not a
		// disconnect worth a WARN (hivecommons/hive#9129).
		if err != nil && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			b.logger.Warn("slack socket disconnected", "error", err)
		}
		if connected {
			delay = b.reconnectBase
			if delay == 0 {
				delay = socketReconnectBase
			}
		}
		wait := delay
		if b.jitter != nil {
			wait = b.jitter(delay)
		}
		// callSlack no longer sleeps out a 429 itself, so a rate-limited
		// apps.connections.open must not be redialed before Slack's Retry-After.
		var retryable *chat.RetryableError
		if !connected && errors.As(err, &retryable) && retryable.RetryAfter > wait {
			wait = retryable.RetryAfter
		}
		if !sleepWithContext(ctx, b.sleep, wait) {
			return
		}
		if !connected {
			delay = min(delay*2, maxDelay)
		}
	}
}

// jitterBackoff spreads d uniformly across ±socketBackoffJitter.
func jitterBackoff(d time.Duration) time.Duration {
	spread := (rand.Float64()*2 - 1) * socketBackoffJitter
	return d + time.Duration(spread*float64(d))
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

// dialSocket asks Slack for a fresh Socket Mode URL and connects to it.
func (b *slackBackend) dialSocket(ctx context.Context) (*websocket.Conn, error) {
	url, err := b.openSocketURL(ctx)
	if err != nil {
		return nil, err
	}
	h := http.Header{"Authorization": {"Bearer " + b.appToken}}
	conn, resp, err := b.dial(ctx, url, h)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}

type socketDial struct {
	conn *websocket.Conn
	err  error
}

// serveSocket reads conn until it fails, always closing it before returning.
//
// When Slack sends a `disconnect` envelope (a `warning` ~10 s ahead of the
// cut, or `refresh_requested`), serveSocket dials the replacement while it
// keeps acknowledging on conn, closes conn once the replacement is open, and
// returns the replacement as next so Listen switches without a gap. Envelopes
// left unread on conn are unacknowledged, so Slack redelivers them.
func (b *slackBackend) serveSocket(ctx context.Context, conn *websocket.Conn, queue chan<- chat.Message) (next *websocket.Conn, err error) {
	readTimeout := b.readTimeout
	if readTimeout <= 0 {
		readTimeout = socketReadTimeout
	}
	pingInterval := b.pingInterval
	if pingInterval <= 0 {
		pingInterval = socketPingInterval
	}
	// Any frame from Slack proves the path is alive; silence past readTimeout
	// fails ReadMessage with a timeout, which ends this session.
	extendDeadline := func() { _ = conn.SetReadDeadline(time.Now().Add(readTimeout)) }
	extendDeadline()
	conn.SetPongHandler(func(string) error { extendDeadline(); return nil })
	conn.SetPingHandler(func(data string) error {
		extendDeadline()
		// Same reply and error tolerance as gorilla's default ping handler.
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(socketAckTimeout))
		var netErr net.Error
		if err == nil || errors.Is(err, websocket.ErrCloseSent) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil
		}
		return err
	})

	connCtx, stopConn := context.WithCancel(ctx)
	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				_ = conn.Close()
				return
			case <-ticker.C:
				// WriteControl is safe beside the reader's ack writes. A failed
				// ping needs no handling: the read deadline ends the session.
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(socketAckTimeout))
			}
		}
	}()

	var handoff chan socketDial // non-nil once Slack announced a disconnect
	defer func() {
		stopConn()
		<-connDone
		_ = conn.Close()
		if handoff == nil {
			return
		}
		dialed := <-handoff
		if dialed.err != nil {
			err = errors.Join(err, fmt.Errorf("slack socket replacement: %w", dialed.err))
			return
		}
		next = dialed.conn
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if handoff != nil {
				// Expected: Slack cut the refreshed socket, or the handoff
				// closed it once the replacement was open.
				return nil, nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, fmt.Errorf("slack socket silent for %s: %w", readTimeout, err)
			}
			return nil, err
		}
		extendDeadline()
		var env socketEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		var msg *chat.Message
		if env.Type == "events_api" {
			var payload eventPayload
			if json.Unmarshal(env.Payload, &payload) == nil {
				e := payload.Event
				if e.Type == "message" && e.Channel == b.channelID {
					id := e.ClientMsgID
					if id == "" {
						id = e.TS
					}
					text, _ := ioscan.EnforceInput(e.Text)
					msg = &chat.Message{ID: id, Text: text, AuthorID: e.User, FromBot: e.BotID != "" || e.Subtype == "bot_message"}
				}
			}
		}
		// This loop is the sole producer. Reserve capacity before acknowledging:
		// reconnect without an ack on overload so Slack can retry the message.
		if msg != nil && len(queue) == cap(queue) {
			return nil, fmt.Errorf("slack inbound queue full")
		}
		if env.EnvelopeID != "" {
			if err := conn.SetWriteDeadline(time.Now().Add(socketAckTimeout)); err != nil {
				return nil, err
			}
			if err := conn.WriteJSON(map[string]string{"envelope_id": env.EnvelopeID}); err != nil {
				b.logger.Warn("slack socket ack failed", "error", err, "envelope_id", env.EnvelopeID)
				return nil, err
			}
		}
		if handoff == nil && (env.Type == "disconnect" || env.Type == "refresh_requested" || env.Reason == "refresh_requested") {
			b.logger.Debug("slack socket refresh announced", "reason", env.Reason)
			handoff = make(chan socketDial, 1)
			go func() {
				replacement, err := b.dialSocket(ctx)
				if err == nil {
					_ = conn.Close()
				}
				handoff <- socketDial{conn: replacement, err: err}
			}()
		}
		if msg != nil {
			queue <- *msg
		}
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
		// Do not sleep here: this runs on the spine's drain goroutine. The
		// spine waits out Retry-After (capped, context-aware) and resends.
		return apiResponse{}, chat.Retryable(fmt.Errorf("slack API 429: rate limited"), retryAfter(resp.Header.Get("Retry-After")))
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("slack API %d: %s", resp.StatusCode, string(respBody))
		if resp.StatusCode >= 500 {
			return apiResponse{}, chat.Retryable(err, 0)
		}
		return apiResponse{}, err
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
