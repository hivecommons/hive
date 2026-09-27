// Package msteams implements a Microsoft Teams chat transport for Hive's
// pkg/chat spine. Hive commonly runs on pull-only clusters that cannot expose
// the inbound webhook endpoint required by Azure Bot Service, so this backend
// deliberately avoids Bot Framework webhooks and inbound HTTP listeners.
// Instead, it polls inbound messages with Microsoft Graph channel-message delta
// queries using app-only client-credentials auth. Microsoft Graph channel-message
// send is delegated-only for normal runtime channels (its application permission
// is for migration/import), so outbound messages are posted to a Teams
// Workflows (Power Automate) "post to a channel when a webhook request is
// received" URL as an Adaptive Card message envelope. Legacy Office 365
// Incoming Webhook connectors were retired by Microsoft in May 2026 and are not
// supported. This keeps Hive pull-only: no inbound listener or externally
// reachable webhook endpoint is required.
//
// Graph channel-message delta only returns root posts, so thread replies are
// fetched separately via /messages/{id}/replies for the most recent
// bot-authored root posts (e.g. checkpoint prompts). Replies under other
// users' posts are not observed; commands there must be new top-level posts.
package msteams

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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	graphAPIBase       = "https://graph.microsoft.com/v1.0"
	loginAPIBase       = "https://login.microsoftonline.com"
	httpTimeoutS       = 10
	pollInterval       = 5 * time.Second
	pollBackoffBase    = 5 * time.Second
	pollBackoffMax     = 60 * time.Second
	teamsMessageLimit  = 4000
	teamsDefaultPacing = 1200 * time.Millisecond
	tokenRefreshSkew   = time.Minute
	readLimit          = 1 << 20
	deltaReadLimit     = 10 << 20
	// seenIDCacheSize bounds the inbound dedupe set, mirroring Matrix's
	// dedupeEventIDCacheSize.
	seenIDCacheSize = 1024
	// replyThreadLimit is how many recent bot-authored root posts are polled
	// for thread replies each delta round.
	replyThreadLimit   = 5
	replyPageSize      = 50
	replyMaxPages      = 5
	adaptiveCardType   = "application/vnd.microsoft.card.adaptive"
	adaptiveCardSchema = "http://adaptivecards.io/schemas/adaptive-card.json"
)

type AgentIdentity = chat.AgentIdentity
type CommandHandler = chat.CommandHandler

// Config configures the Teams transport. AllowedUsers contains AAD object IDs;
// an empty list leaves command authorization fail-closed in the chat spine.
type Config struct {
	Enabled        bool
	TenantID       string
	ClientID       string
	ClientSecret   string
	TeamID         string
	ChannelID      string
	WebhookURL     string
	DashboardURL   string
	DashboardToken string
	AllowedUsers   []string
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
}

type Bot struct {
	*Backend
	service *chat.Service
}

type Backend struct {
	tenantID     string
	clientID     string
	clientSecret string
	teamID       string
	channelID    string
	webhookURL   string
	graphBase    string
	loginBase    string
	logger       *slog.Logger
	client       *http.Client
	sleep        func(time.Duration)
	ctxSleep     func(context.Context, time.Duration) bool
	now          func() time.Time

	tokenMu    sync.Mutex
	token      string
	tokenUntil time.Time

	seenMu      sync.Mutex
	seen        map[string]struct{}
	seenOrder   []string
	deltaURL    string
	deltaReady  bool
	lastDeltaOK time.Time
	// threads holds recent bot-authored root post IDs (oldest first) whose
	// replies are polled; threadReady marks threads whose existing replies
	// have been baselined and whose new replies are therefore delivered.
	threads     []string
	threadReady map[string]bool
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

type graphMessageBody struct {
	ContentType string `json:"contentType,omitempty"`
	Content     string `json:"content,omitempty"`
}

type graphIdentity struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type graphFrom struct {
	Application *graphIdentity `json:"application,omitempty"`
	User        *graphIdentity `json:"user,omitempty"`
}

type graphMessage struct {
	ID   string           `json:"id"`
	Body graphMessageBody `json:"body"`
	From graphFrom        `json:"from"`
}

type deltaResponse struct {
	Value     []graphMessage `json:"value"`
	NextLink  string         `json:"@odata.nextLink"`
	DeltaLink string         `json:"@odata.deltaLink"`
}

type repliesResponse struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

type adaptiveTextBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Wrap     bool   `json:"wrap"`
	FontType string `json:"fontType,omitempty"`
}

type adaptiveCard struct {
	Schema  string              `json:"$schema"`
	Type    string              `json:"type"`
	Version string              `json:"version"`
	Body    []adaptiveTextBlock `json:"body"`
	MSTeams map[string]string   `json:"msteams,omitempty"`
}

type cardAttachment struct {
	ContentType string       `json:"contentType"`
	ContentURL  *string      `json:"contentUrl"`
	Content     adaptiveCard `json:"content"`
}

// workflowMessage is the envelope Teams Workflows webhooks expect.
type workflowMessage struct {
	Type        string           `json:"type"`
	Attachments []cardAttachment `json:"attachments"`
}

type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type rateLimitError struct{ after time.Duration }

func (e rateLimitError) Error() string {
	return fmt.Sprintf("msteams API 429: retry after %s", e.after)
}

func SetAgentIdentities(identities map[string]AgentIdentity) { chat.SetAgentIdentities(identities) }
func SetAgentAliases(agentAliases map[string]string)         { chat.SetAgentAliases(agentAliases) }

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	backend := &Backend{
		tenantID:     strings.TrimSpace(cfg.TenantID),
		clientID:     strings.TrimSpace(cfg.ClientID),
		clientSecret: strings.TrimSpace(cfg.ClientSecret),
		teamID:       strings.TrimSpace(cfg.TeamID),
		channelID:    strings.TrimSpace(cfg.ChannelID),
		webhookURL:   strings.TrimSpace(cfg.WebhookURL),
		graphBase:    graphAPIBase,
		loginBase:    loginAPIBase,
		logger:       logger,
		client:       &http.Client{Timeout: httpTimeoutS * time.Second},
		sleep:        time.Sleep,
		ctxSleep:     sleepContext,
		now:          time.Now,
		seen:         make(map[string]struct{}),
		threadReady:  make(map[string]bool),
	}
	service := chat.NewService(backend, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
		MessageLimit:      teamsMessageLimit,
		SendInterval:      teamsDefaultPacing,
		HeartbeatInterval: 15 * time.Minute,
	}, logger)
	return &Bot{Backend: backend, service: service}
}

func (b *Bot) SetAgentNames(names []string) { b.service.SetAgentNames(names) }
func (b *Bot) RegisterCommand(name string, handler CommandHandler) {
	b.service.RegisterCommand(name, handler)
}
func (b *Bot) Start(ctx context.Context) error {
	if err := b.validate(); err != nil {
		return err
	}
	b.logger.Info("msteams bot starting", "team", b.teamID, "channel", b.channelID)
	return b.service.Start(ctx)
}
func (b *Bot) SendMessage(content string) error { return b.Send(content) }
func (b *Bot) Listen(ctx context.Context, deliver func(chat.Message)) {
	b.Backend.Listen(ctx, deliver)
}
func (b *Bot) SetTopic(topic string) error { return b.Backend.SetTopic(topic) }

func (b *Backend) Name() string { return "msteams" }

func (b *Backend) validate() error {
	if b.tenantID == "" || b.clientID == "" || b.clientSecret == "" || b.teamID == "" || b.channelID == "" || b.webhookURL == "" {
		return fmt.Errorf("msteams tenant_id, client_id, client_secret, team_id, channel_id, and webhook_url must be configured")
	}
	return nil
}

func (b *Backend) Send(content string) error {
	content = logscrub.ScrubString(content)
	for _, part := range splitTeamsMessage(content) {
		payload := workflowCardMessage(part)
		if err := b.postWebhook(context.Background(), payload); err != nil {
			return b.sanitizeWebhookError(err)
		}
	}
	return nil
}

func (b *Backend) postWebhook(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.webhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("msteams webhook request: %w", b.sanitizeWebhookError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, b.webhookURL, bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("msteams webhook request: %w", b.sanitizeWebhookError(err))
			}
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.client.Do(req)
		if err != nil {
			return fmt.Errorf("msteams webhook send: %w", b.sanitizeWebhookError(err))
		}
		respBody, _, _ := readLimited(resp.Body, readLimit)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			after := cappedRetryAfter(resp.Header.Get("Retry-After"))
			if attempt == 0 && after > 0 && b.ctxSleep(ctx, after) {
				continue
			}
			return rateLimitError{after: after}
		}
		if resp.StatusCode >= 400 {
			return b.sanitizeWebhookError(fmt.Errorf("msteams webhook %d: %s", resp.StatusCode, string(respBody)))
		}
		return nil
	}
	return nil
}

func (b *Backend) SetTopic(topic string) error {
	topic = logscrub.ScrubString(topic)
	payload := map[string]string{"description": topic}
	err := b.callGraphJSON(context.Background(), http.MethodPatch, b.channelPath(), payload, nil)
	if isForbidden(err) {
		b.logger.Info("msteams topic update forbidden; continuing", "error", err)
		return nil
	}
	return err
}

func (b *Backend) Listen(ctx context.Context, deliver func(chat.Message)) {
	delay := pollBackoffBase
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		immediate, err := b.pollOnce(ctx, deliver)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return
			}
			if rl, ok := err.(rateLimitError); ok && rl.after > 0 {
				if !b.ctxSleep(ctx, minDuration(rl.after, pollBackoffMax)) {
					return
				}
				continue
			}
			b.logger.Warn("msteams poll failed", "error", err)
			if !b.ctxSleep(ctx, delay) {
				return
			}
			delay *= 2
			if delay > pollBackoffMax {
				delay = pollBackoffMax
			}
			continue
		}
		delay = pollBackoffBase
		if immediate {
			continue
		}
		if !b.ctxSleep(ctx, pollInterval) {
			return
		}
	}
}

func (b *Backend) pollOnce(ctx context.Context, deliver func(chat.Message)) (bool, error) {
	path := b.deltaURL
	baseline := !b.deltaReady
	if path == "" {
		path = b.channelMessagesPath() + "/delta"
	}
	var page deltaResponse
	if err := b.callGraphJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
		return false, err
	}
	for _, msg := range page.Value {
		if msg.ID == "" || b.wasSeen(msg.ID) {
			continue
		}
		b.markSeen(msg.ID)
		isBot := fromBot(msg, b.clientID)
		if isBot {
			b.trackThread(msg.ID, !baseline)
		}
		if !baseline {
			b.deliverMessage(msg, deliver)
		}
	}
	if page.NextLink != "" {
		b.deltaURL = page.NextLink
		return true, nil
	}
	if page.DeltaLink != "" {
		b.deltaURL = page.DeltaLink
		b.deltaReady = true
		b.lastDeltaOK = b.now()
	}
	if !b.deltaReady {
		return false, nil
	}
	return false, b.pollReplies(ctx, deliver)
}

func (b *Backend) deliverMessage(msg graphMessage, deliver func(chat.Message)) {
	text, _ := ioscan.EnforceInput(inboundText(msg.Body))
	deliver(chat.Message{ID: msg.ID, Text: text, AuthorID: authorID(msg), FromBot: fromBot(msg, b.clientID)})
}

// pollReplies fetches thread replies for recent bot-authored root posts, which
// the channel-message delta never returns.
func (b *Backend) pollReplies(ctx context.Context, deliver func(chat.Message)) error {
	b.seenMu.Lock()
	roots := append([]string(nil), b.threads...)
	b.seenMu.Unlock()
	for _, root := range roots {
		b.seenMu.Lock()
		ready := b.threadReady[root]
		b.seenMu.Unlock()
		path := b.channelMessagesPath() + "/" + url.PathEscape(root) + "/replies?$top=" + strconv.Itoa(replyPageSize)
		for page := 0; path != "" && page < replyMaxPages; page++ {
			var resp repliesResponse
			if err := b.callGraphJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
				if isNotFound(err) {
					b.dropThread(root)
					break
				}
				return err
			}
			for _, msg := range resp.Value {
				if msg.ID == "" || b.wasSeen(msg.ID) {
					continue
				}
				b.markSeen(msg.ID)
				if ready {
					b.deliverMessage(msg, deliver)
				}
			}
			path = resp.NextLink
		}
		b.seenMu.Lock()
		if _, tracked := b.threadReady[root]; tracked {
			b.threadReady[root] = true
		}
		b.seenMu.Unlock()
	}
	return nil
}

func (b *Backend) callGraphJSON(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	_, isDelta := out.(*deltaResponse)
	resp, err := b.doGraphWithPrefer(ctx, method, path, body, payload != nil, isDelta)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return rateLimitError{after: cappedRetryAfter(resp.Header.Get("Retry-After"))}
	}
	limit := int64(readLimit)
	if isDelta {
		limit = deltaReadLimit
	}
	respBody, truncated, err := readLimited(resp.Body, limit)
	if err != nil {
		return err
	}
	if isDelta && isDeltaResync(resp.StatusCode, respBody) {
		b.resyncDelta(resp.StatusCode, resp.Header.Get("Location"))
		return nil
	}
	if truncated && isDelta {
		b.logger.Warn("msteams delta response exceeded read limit; resetting delta baseline", "limit", limit)
		b.resetDelta()
		return nil
	}
	if truncated {
		return fmt.Errorf("msteams API response exceeded %d bytes", limit)
	}
	if resp.StatusCode >= 400 {
		return graphStatusError(resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) doGraph(ctx context.Context, method, path string, body io.Reader, jsonBody bool) (*http.Response, error) {
	return b.doGraphWithPrefer(ctx, method, path, body, jsonBody, false)
}

func (b *Backend) doGraphWithPrefer(ctx context.Context, method, path string, body io.Reader, jsonBody, preferSmallPages bool) (*http.Response, error) {
	token, err := b.getToken(ctx)
	if err != nil {
		return nil, err
	}
	reqURL := path
	if !strings.HasPrefix(reqURL, "http://") && !strings.HasPrefix(reqURL, "https://") {
		reqURL = strings.TrimRight(b.graphBase, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if preferSmallPages {
		req.Header.Set("Prefer", "odata.maxpagesize=20")
	}
	return b.client.Do(req)
}

func (b *Backend) getToken(ctx context.Context) (string, error) {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	if b.token != "" && b.now().Add(tokenRefreshSkew).Before(b.tokenUntil) {
		return b.token, nil
	}
	form := url.Values{}
	form.Set("client_id", b.clientID)
	form.Set("client_secret", b.clientSecret)
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "https://graph.microsoft.com/.default")
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(b.loginBase, "/"), url.PathEscape(b.tenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, readLimit))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("msteams token API %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed tokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("msteams token API returned no access_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}
	b.token = parsed.AccessToken
	b.tokenUntil = b.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	return b.token, nil
}

func (b *Backend) channelPath() string {
	return "/teams/" + url.PathEscape(b.teamID) + "/channels/" + url.PathEscape(b.channelID)
}

func (b *Backend) channelMessagesPath() string { return b.channelPath() + "/messages" }

func (b *Backend) wasSeen(id string) bool {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	_, ok := b.seen[id]
	return ok
}

func (b *Backend) markSeen(id string) {
	b.seenMu.Lock()
	if _, ok := b.seen[id]; !ok {
		rememberID(b.seen, &b.seenOrder, id)
	}
	b.seenMu.Unlock()
}

func rememberID(seen map[string]struct{}, order *[]string, id string) {
	seen[id] = struct{}{}
	*order = append(*order, id)
	if len(*order) <= seenIDCacheSize {
		return
	}
	oldest := (*order)[0]
	delete(seen, oldest)
	copy(*order, (*order)[1:])
	*order = (*order)[:len(*order)-1]
}

// trackThread records a bot-authored root post for reply polling. ready is
// true for posts that arrived after the delta baseline, so all their replies
// are new; baseline-era posts first have their existing replies marked seen.
func (b *Backend) trackThread(id string, ready bool) {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if _, ok := b.threadReady[id]; ok {
		return
	}
	b.threads = append(b.threads, id)
	b.threadReady[id] = ready
	if len(b.threads) > replyThreadLimit {
		delete(b.threadReady, b.threads[0])
		b.threads = append([]string(nil), b.threads[1:]...)
	}
}

func (b *Backend) dropThread(id string) {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	delete(b.threadReady, id)
	for i, root := range b.threads {
		if root == id {
			b.threads = append(b.threads[:i:i], b.threads[i+1:]...)
			break
		}
	}
}

func (b *Backend) resetDelta() {
	b.seenMu.Lock()
	b.deltaURL = ""
	b.deltaReady = false
	b.seen = make(map[string]struct{})
	b.seenOrder = nil
	b.threads = nil
	b.threadReady = make(map[string]bool)
	b.seenMu.Unlock()
}

// resyncDelta restarts the delta stream after Graph invalidates the delta
// token (410 Gone / resyncRequired). Messages posted between the last
// successful round and the fresh baseline are not delivered.
func (b *Backend) resyncDelta(status int, location string) {
	attrs := []any{"status", status}
	if !b.lastDeltaOK.IsZero() {
		attrs = append(attrs, "last_successful_poll", b.lastDeltaOK, "gap", b.now().Sub(b.lastDeltaOK).Round(time.Second))
	}
	b.logger.Warn("msteams delta token expired; restarting delta baseline, messages posted in the gap are dropped", attrs...)
	b.resetDelta()
	if location != "" {
		b.seenMu.Lock()
		b.deltaURL = location
		b.seenMu.Unlock()
	}
}

func isDeltaResync(status int, body []byte) bool {
	if status == http.StatusGone {
		return true
	}
	if status < 400 {
		return false
	}
	var parsed graphError
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	switch strings.ToLower(parsed.Error.Code) {
	case "resyncrequired", "syncstatenotfound", "syncstateinvalid":
		return true
	}
	return false
}

func (b *Backend) sanitizeWebhookError(err error) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := err.(*url.Error); ok && urlErr.Err != nil {
		err = urlErr.Err
	}
	msg := err.Error()
	if b.webhookURL != "" {
		msg = strings.ReplaceAll(msg, b.webhookURL, "[redacted]")
		if escaped := url.QueryEscape(b.webhookURL); escaped != b.webhookURL {
			msg = strings.ReplaceAll(msg, escaped, "[redacted]")
		}
	}
	return errors.New(msg)
}

func authorID(msg graphMessage) string {
	if msg.From.User != nil {
		return msg.From.User.ID
	}
	return ""
}

func fromBot(msg graphMessage, appID string) bool {
	if msg.From.Application != nil {
		return true
	}
	return msg.From.User != nil && msg.From.User.ID != "" && msg.From.User.ID == appID
}

func inboundText(body graphMessageBody) string {
	if !strings.EqualFold(body.ContentType, "html") {
		return body.Content
	}
	return htmlToText(body.Content)
}

func htmlToText(s string) string {
	var out strings.Builder
	inTag := false
	tag := strings.Builder{}
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
			tag.Reset()
		case '>':
			name := strings.ToLower(strings.TrimSpace(tag.String()))
			name = strings.TrimPrefix(name, "/")
			if name == "br" || name == "br/" || name == "p" || name == "div" {
				out.WriteByte('\n')
			}
			inTag = false
		default:
			if inTag {
				tag.WriteRune(r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(html.UnescapeString(out.String()))
}

func graphStatusError(status int, body []byte) error {
	var parsed graphError
	if len(body) > 0 && json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return fmt.Errorf("msteams API %d: %s", status, parsed.Error.Message)
	}
	return fmt.Errorf("msteams API %d: %s", status, string(body))
}

func isForbidden(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "msteams API 403")
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "msteams API 404")
}

func retryAfter(s string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func cappedRetryAfter(s string) time.Duration {
	return minDuration(retryAfter(s), pollBackoffMax)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func readLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
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

func workflowCardMessage(s string) workflowMessage {
	return workflowMessage{
		Type: "message",
		Attachments: []cardAttachment{{
			ContentType: adaptiveCardType,
			Content: adaptiveCard{
				Schema:  adaptiveCardSchema,
				Type:    "AdaptiveCard",
				Version: "1.4",
				Body:    markdownToCardBody(s),
				MSTeams: map[string]string{"width": "Full"},
			},
		}},
	}
}

// markdownToCardBody maps chat markdown onto Adaptive Card TextBlocks, which
// render a markdown subset natively; fenced code becomes monospace blocks.
func markdownToCardBody(s string) []adaptiveTextBlock {
	var blocks []adaptiveTextBlock
	var cur []string
	inFence := false
	flush := func(fence bool) {
		text := strings.Join(cur, "\n")
		cur = nil
		if !fence {
			text = strings.Trim(text, "\n")
		}
		if strings.TrimSpace(text) == "" {
			return
		}
		block := adaptiveTextBlock{Type: "TextBlock", Text: text, Wrap: true}
		if fence {
			block.FontType = "Monospace"
		}
		blocks = append(blocks, block)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			flush(inFence)
			inFence = !inFence
			continue
		}
		cur = append(cur, line)
	}
	flush(inFence)
	if len(blocks) == 0 {
		blocks = []adaptiveTextBlock{{Type: "TextBlock", Text: " ", Wrap: true}}
	}
	return blocks
}

func splitTeamsMessage(s string) []string {
	if len([]rune(s)) <= teamsMessageLimit {
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
			limit := teamsMessageLimit - reserve
			if limit < 1 {
				limit = teamsMessageLimit
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
		reserve := 0
		if inFence || strings.Contains(para, "```") {
			reserve = len([]rune("\n```"))
		}
		if len([]rune(current+para)) <= teamsMessageLimit-reserve {
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
