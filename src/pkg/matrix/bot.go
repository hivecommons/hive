// Package matrix implements Hive's Matrix chat transport for unencrypted rooms only.
//
// The backend uses the Matrix Client-Server API directly over HTTPS. On startup,
// Listen intentionally discards the first /sync batch and keeps only its next_batch
// token so room history from before the process started is not delivered.
package matrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	httpTimeoutS           = 40
	syncTimeoutMS          = 30000
	matrixAPIPath          = "/_matrix/client/v3"
	matrixReconnectBase    = 5 * time.Second
	matrixReconnectMax     = 60 * time.Second
	matrixMessageLimit     = 4000
	matrixDefaultSendPace  = 1200 * time.Millisecond
	maxResponseBodyBytes   = 1 << 20
	maxSyncResponseBytes   = 20 << 20
	dedupeEventIDCacheSize = 1024
	syncTimelineLimit      = 20
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

type Config struct {
	Enabled        bool
	HomeserverURL  string
	AccessToken    string
	RoomID         string
	DashboardURL   string
	DashboardToken string
	// AllowedUsers is the set of full Matrix IDs permitted to issue commands.
	// Empty = commands disabled (fail closed) by the chat spine.
	AllowedUsers []string
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
}

type Bot struct {
	*matrixBackend
	service *chat.Service
}

type matrixBackend struct {
	homeserverURL string
	accessToken   string
	roomID        string
	userID        string
	logger        *slog.Logger
	client        *http.Client
	sleep         func(time.Duration)
	reconnectBase time.Duration
	reconnectMax  time.Duration
	txnCounter    uint64
}

type whoamiResponse struct {
	UserID string `json:"user_id"`
}

type matrixMessagePayload struct {
	MsgType       string `json:"msgtype"`
	Body          string `json:"body"`
	Format        string `json:"format,omitempty"`
	FormattedBody string `json:"formatted_body,omitempty"`
}

type topicPayload struct {
	Topic string `json:"topic"`
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]joinedRoom `json:"join"`
	} `json:"rooms"`
}

type joinedRoom struct {
	Timeline struct {
		Events []matrixEvent `json:"events"`
	} `json:"timeline"`
}

type matrixEvent struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	Content struct {
		MsgType string `json:"msgtype"`
		Body    string `json:"body"`
	} `json:"content"`
}

type matrixAPIError struct {
	ErrCode      string `json:"errcode"`
	ErrorMessage string `json:"error"`
	RetryAfterMS int64  `json:"retry_after_ms"`
	StatusCode   int    `json:"-"`
}

func (e matrixAPIError) Error() string {
	if e.ErrCode != "" || e.ErrorMessage != "" {
		return fmt.Sprintf("matrix API %d: %s %s", e.StatusCode, e.ErrCode, e.ErrorMessage)
	}
	return fmt.Sprintf("matrix API %d", e.StatusCode)
}

func SetAgentIdentities(identities map[string]AgentIdentity) { chat.SetAgentIdentities(identities) }
func SetAgentAliases(agentAliases map[string]string)         { chat.SetAgentAliases(agentAliases) }

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	backend := &matrixBackend{
		homeserverURL: strings.TrimRight(strings.TrimSpace(cfg.HomeserverURL), "/"),
		accessToken:   strings.TrimSpace(cfg.AccessToken),
		roomID:        strings.TrimSpace(cfg.RoomID),
		logger:        logger,
		client:        &http.Client{Timeout: httpTimeoutS * time.Second},
		sleep:         time.Sleep,
		reconnectBase: matrixReconnectBase,
		reconnectMax:  matrixReconnectMax,
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
		MessageLimit:      matrixMessageLimit,
		SendInterval:      matrixDefaultSendPace,
		HeartbeatInterval: 15 * time.Minute,
	}, logger)
	return &Bot{matrixBackend: backend, service: service}
}

func (b *Bot) SetAgentNames(names []string) { b.service.SetAgentNames(names) }
func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.service.RegisterCommand(name, handler)
}

func (b *Bot) Start(ctx context.Context) error {
	if b.homeserverURL == "" || b.accessToken == "" || b.roomID == "" {
		return fmt.Errorf("matrix homeserver_url, access_token, and room_id must be configured")
	}
	userID, err := b.matrixBackend.Whoami(ctx)
	if err != nil {
		return err
	}
	b.userID = userID
	b.logger.Info("matrix bot starting", "room", b.roomID, "user_id", b.userID)
	return b.service.Start(ctx)
}

func (b *Bot) SendMessage(content string) error { return b.Send(content) }
func (b *Bot) Listen(ctx context.Context, deliver func(chat.Message)) {
	b.matrixBackend.Listen(ctx, deliver)
}
func (b *Bot) SetTopic(topic string) error { return b.matrixBackend.SetTopic(topic) }

func (b *matrixBackend) Name() string { return "matrix" }

func (b *matrixBackend) Whoami(ctx context.Context) (string, error) {
	var parsed whoamiResponse
	if err := b.doJSON(ctx, http.MethodGet, "/account/whoami", nil, &parsed); err != nil {
		return "", fmt.Errorf("matrix whoami: %w", err)
	}
	if strings.TrimSpace(parsed.UserID) == "" {
		return "", fmt.Errorf("matrix whoami returned no user_id")
	}
	return parsed.UserID, nil
}

func (b *matrixBackend) Send(content string) error {
	content = logscrub.ScrubString(content)
	for _, part := range splitMatrixMessage(content) {
		payload := matrixMessagePayload{
			MsgType:       "m.text",
			Body:          part,
			Format:        "org.matrix.custom.html",
			FormattedBody: markdownToMatrixHTML(part),
		}
		path := fmt.Sprintf("/rooms/%s/send/m.room.message/%s", pathEscape(b.roomID), pathEscape(b.nextTxnID()))
		if err := b.doJSON(context.Background(), http.MethodPut, path, payload, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *matrixBackend) SetTopic(topic string) error {
	topic = logscrub.ScrubString(topic)
	err := b.doJSON(context.Background(), http.MethodPut, "/rooms/"+pathEscape(b.roomID)+"/state/m.room.topic", topicPayload{Topic: topic}, nil)
	var apiErr matrixAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		b.logger.Warn("matrix topic update forbidden; leaving room topic unchanged")
		return nil
	}
	return err
}

func (b *matrixBackend) Listen(ctx context.Context, deliver func(chat.Message)) {
	delay := defaultDuration(b.reconnectBase, matrixReconnectBase)
	maxDelay := defaultDuration(b.reconnectMax, matrixReconnectMax)
	since := ""
	firstSync := true
	seenOrder := make([]string, 0, dedupeEventIDCacheSize)
	seen := make(map[string]struct{}, dedupeEventIDCacheSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resp, err := b.sync(ctx, since)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return
			}
			wait := delay
			var apiErr matrixAPIError
			if errors.As(err, &apiErr) && apiErr.RetryAfterMS > 0 {
				wait = b.retryAfterDelay(apiErr.RetryAfterMS)
			} else {
				if delay < maxDelay {
					delay = min(delay*2, maxDelay)
				}
			}
			b.logger.Warn("matrix sync failed", "error", err, "retry_after", wait)
			if !waitContext(ctx, wait) {
				return
			}
			continue
		}

		delay = defaultDuration(b.reconnectBase, matrixReconnectBase)
		if firstSync {
			firstSync = false
			since = resp.NextBatch
			continue
		}

		if room, ok := resp.Rooms.Join[b.roomID]; ok {
			for _, event := range room.Timeline.Events {
				if event.Type != "m.room.message" || event.Content.Body == "" || event.EventID == "" {
					continue
				}
				if _, ok := seen[event.EventID]; ok {
					continue
				}
				rememberEvent(seen, &seenOrder, event.EventID)
				text, _ := ioscan.EnforceInput(event.Content.Body)
				deliver(chat.Message{ID: event.EventID, Text: text, AuthorID: event.Sender, FromBot: event.Sender == b.userID})
			}
		}
		since = resp.NextBatch
	}
}

func (b *matrixBackend) sync(ctx context.Context, since string) (syncResponse, error) {
	values := url.Values{"timeout": {fmt.Sprintf("%d", syncTimeoutMS)}}
	values.Set("filter", b.syncFilter())
	if since != "" {
		values.Set("since", since)
	}
	var parsed syncResponse
	err := b.doJSON(ctx, http.MethodGet, "/sync?"+values.Encode(), nil, &parsed)
	return parsed, err
}

func (b *matrixBackend) doJSON(ctx context.Context, method, path string, payload any, out any) error {
	body, err := marshalBody(payload)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		err = b.doJSONOnce(ctx, method, path, body, out)
		var apiErr matrixAPIError
		if !errors.As(err, &apiErr) || apiErr.RetryAfterMS <= 0 || attempt == 1 {
			return err
		}
		if !sleepWithContext(ctx, b.sleep, b.retryAfterDelay(apiErr.RetryAfterMS)) {
			return ctx.Err()
		}
	}
	return err
}

func (b *matrixBackend) doJSONOnce(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.homeserverURL+matrixAPIPath+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, b.responseBodyLimit(path)))
	if resp.StatusCode >= 400 {
		return parseMatrixError(resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return err
		}
	}
	return nil
}

func (b *matrixBackend) syncFilter() string {
	filter := map[string]any{
		"room": map[string]any{
			"rooms": []string{b.roomID},
			"state": map[string]any{
				"lazy_load_members": true,
			},
			"timeline": map[string]any{
				"limit": syncTimelineLimit,
			},
		},
	}
	data, err := json.Marshal(filter)
	if err != nil {
		return ""
	}
	return string(data)
}

func (b *matrixBackend) responseBodyLimit(path string) int64 {
	if path == "/sync" || strings.HasPrefix(path, "/sync?") {
		return maxSyncResponseBytes
	}
	return maxResponseBodyBytes
}

func (b *matrixBackend) retryAfterDelay(retryAfterMS int64) time.Duration {
	delay := time.Duration(retryAfterMS) * time.Millisecond
	maxDelay := defaultDuration(b.reconnectMax, matrixReconnectMax)
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func marshalBody(payload any) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func parseMatrixError(status int, body []byte) error {
	apiErr := matrixAPIError{StatusCode: status}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &apiErr)
		apiErr.StatusCode = status
	}
	return apiErr
}

func (b *matrixBackend) nextTxnID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("hive-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&b.txnCounter, 1))
	}
	return fmt.Sprintf("hive-%s-%d", hex.EncodeToString(random[:]), atomic.AddUint64(&b.txnCounter, 1))
}

func pathEscape(s string) string {
	return url.PathEscape(s)
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func waitContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func sleepWithContext(ctx context.Context, sleep func(time.Duration), d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if sleep == nil {
		return waitContext(ctx, d)
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

func rememberEvent(seen map[string]struct{}, order *[]string, id string) {
	seen[id] = struct{}{}
	*order = append(*order, id)
	if len(*order) <= dedupeEventIDCacheSize {
		return
	}
	oldest := (*order)[0]
	delete(seen, oldest)
	copy(*order, (*order)[1:])
	*order = (*order)[:len(*order)-1]
}

func markdownToMatrixHTML(s string) string {
	var out strings.Builder
	inFence := false
	inCode := false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "```") {
			if inFence {
				out.WriteString("</code></pre>")
			} else {
				out.WriteString("<pre><code>")
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
		if !inFence && !inCode && strings.HasPrefix(s[i:], "**") {
			end := strings.Index(s[i+2:], "**")
			if end >= 0 {
				text := s[i+2 : i+2+end]
				out.WriteString("<strong>" + html.EscapeString(text) + "</strong>")
				i += 2 + end + 2
				continue
			}
		}
		if !inFence && !inCode && s[i] == '[' {
			if text, href, width, ok := parseMarkdownLink(s[i:]); ok {
				out.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(text) + `</a>`)
				i += width
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		out.WriteString(html.EscapeString(string(r)))
		i += size
	}
	if inCode {
		out.WriteString("</code>")
	}
	if inFence {
		out.WriteString("</code></pre>")
	}
	return strings.ReplaceAll(out.String(), "\n", "<br />\n")
}

func parseMarkdownLink(s string) (text, href string, width int, ok bool) {
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

func splitMatrixMessage(s string) []string {
	if len([]rune(s)) <= matrixMessageLimit {
		return []string{s}
	}
	var parts []string
	for s != "" {
		part := takeRunes(s, matrixMessageLimit)
		parts = append(parts, part)
		s = strings.TrimPrefix(s, part)
	}
	return parts
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
