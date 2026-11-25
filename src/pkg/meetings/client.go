package meetings

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/prosefilter"
)

// MCP over streamable HTTP (POST /mcp), the transport Quorum serves. The
// client is deliberately small: initialize, notifications/initialized and
// tools/call are all hive needs, so no SDK dependency is pulled in.
const (
	mcpProtocolVersion = "2025-03-26"
	mcpSessionHeader   = "Mcp-Session-Id"
	mcpVersionHeader   = "MCP-Protocol-Version"
	// maxSSELine bounds one SSE line from Quorum.
	maxSSELine = 4 << 20
	// maxResponseBytes bounds one JSON response body.
	maxResponseBytes = 16 << 20
)

// Tool error codes (contract section 1).
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeNotFound            = "not_found"
	CodePlatformUnavailable = "platform_unavailable"
	CodeBlocked             = "blocked"
	CodeLobbyTimeout        = "lobby_timeout"
	CodeNotLive             = "not_live"
	CodeRateLimited         = "rate_limited"
	CodeUpstreamError       = "upstream_error"
)

// ToolError is a Quorum tool's {ok:false, error:{code,message}}.
type ToolError struct {
	Tool    string `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("quorum %s: %s: %s", e.Tool, e.Code, e.Message)
}

// IsCode reports whether err is a ToolError with code.
func IsCode(err error, code string) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Code == code
}

// ErrStreamGone is returned by Events when Quorum no longer knows the
// meeting (404 on /events).
var ErrStreamGone = errors.New("meetings: quorum has no such meeting")

// Client talks to one Quorum server.
type Client struct {
	base  string
	token string
	http  *http.Client

	nextID    atomic.Int64
	mu        sync.Mutex
	sessionID string
	inited    bool
}

// NewClient returns a client for base (http://127.0.0.1:8765) with the
// per-boot bearer token. A nil hc uses a client without a timeout; calls
// are bounded by their context.
func NewClient(base, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: hc}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// errSessionExpired: Quorum restarted and forgot our MCP session.
var errSessionExpired = errors.New("mcp session expired")

func (c *Client) post(ctx context.Context, body any, wantID int64, beforeDispatch func() error) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// Every chat line and spoken line (chat.post, say, say.prewarm) leaves
	// through here: apply the shared outbound dash rule (#19103).
	data, _ = prosefilter.SanitizeJSON(data, meetingProseKeys)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/mcp", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	c.mu.Lock()
	sid, inited := c.sessionID, c.inited
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set(mcpSessionHeader, sid)
	}
	if inited {
		req.Header.Set(mcpVersionHeader, mcpProtocolVersion)
	}
	if beforeDispatch != nil {
		if err := beforeDispatch(); err != nil {
			return nil, err
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quorum: %w", err)
	}
	defer resp.Body.Close()
	if v := resp.Header.Get(mcpSessionHeader); v != "" {
		c.mu.Lock()
		c.sessionID = v
		c.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound && sid != "" {
		return nil, errSessionExpired
	}
	if resp.StatusCode == http.StatusAccepted && wantID == 0 {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("quorum: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if wantID == 0 {
		return nil, nil
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var msg rpcResponse
	if ct == "text/event-stream" {
		found := false
		err = readSSE(resp.Body, func(data []byte) bool {
			var m rpcResponse
			if json.Unmarshal(data, &m) != nil || string(m.ID) != fmt.Sprint(wantID) {
				return true
			}
			msg, found = m, true
			return false
		})
		if err != nil {
			return nil, fmt.Errorf("quorum: reading stream: %w", err)
		}
		if !found {
			return nil, errors.New("quorum: stream ended without a response")
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			return nil, fmt.Errorf("quorum: reading response: %w", err)
		}
		if err := json.Unmarshal(b, &msg); err != nil {
			return nil, fmt.Errorf("quorum: decoding response: %w", err)
		}
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("quorum: rpc error %d: %s", msg.Error.Code, msg.Error.Message)
	}
	return msg.Result, nil
}

func (c *Client) request(ctx context.Context, method string, params any, beforeDispatch func() error) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	return c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, id, beforeDispatch)
}

func (c *Client) initialize(ctx context.Context) error {
	c.mu.Lock()
	c.sessionID, c.inited = "", false
	c.mu.Unlock()
	_, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "hive-meetings", "version": "1"},
	}, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.inited = true
	c.mu.Unlock()
	_, err = c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, 0, nil)
	return err
}

func (c *Client) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	inited := c.inited
	c.mu.Unlock()
	if inited {
		return nil
	}
	return c.initialize(ctx)
}

// Call invokes a contract tool and decodes its {ok:true,...} result into
// out (which may be nil). {ok:false} comes back as a *ToolError.
func (c *Client) Call(ctx context.Context, tool string, args, out any) error {
	return c.call(ctx, tool, args, out, nil)
}

func (c *Client) call(ctx context.Context, tool string, args, out any, beforeDispatch func() error) error {
	if err := c.ensureInit(ctx); err != nil {
		return err
	}
	params := map[string]any{"name": tool, "arguments": args}
	raw, err := c.request(ctx, "tools/call", params, beforeDispatch)
	if errors.Is(err, errSessionExpired) {
		if err = c.initialize(ctx); err == nil {
			raw, err = c.request(ctx, "tools/call", params, beforeDispatch)
		}
	}
	if err != nil {
		return err
	}
	return decodeToolResult(tool, raw, out)
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

type envelope struct {
	OK    bool       `json:"ok"`
	Error *ToolError `json:"error"`
}

// decodeToolResult reads the tool's JSON envelope from structuredContent,
// or from the first text content block.
func decodeToolResult(tool string, raw json.RawMessage, out any) error {
	var tr toolResult
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("quorum %s: decoding result: %w", tool, err)
	}
	payload := []byte(tr.StructuredContent)
	if len(payload) == 0 || string(payload) == "null" {
		for _, c := range tr.Content {
			if c.Type == "text" {
				payload = []byte(c.Text)
				break
			}
		}
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return fmt.Errorf("quorum %s: result is not the contract envelope: %w", tool, err)
	}
	if !env.OK {
		te := env.Error
		if te == nil {
			te = &ToolError{Code: CodeUpstreamError, Message: "tool reported failure without an error"}
		}
		te.Tool = tool
		return te
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("quorum %s: decoding result: %w", tool, err)
	}
	return nil
}

// JoinArgs are meeting.join's arguments.
type JoinArgs struct {
	URL         string `json:"url,omitempty"`
	DisplayName string `json:"display_name"`
	Platform    string `json:"platform,omitempty"`
	GuildID     string `json:"guild_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	// Voice starts the bot able to speak; Vexa fixes this at join time, so
	// a bot joined without it can never use say.
	Voice bool `json:"voice,omitempty"`
	// VoiceID is the TTS voice the bot speaks with (Discord's Kokoro voice,
	// Vexa's per-speak voice). Empty uses Quorum's default.
	VoiceID string `json:"voice_id,omitempty"`
	// HostName names the companion's mic channel speaker.
	HostName       string `json:"host_name,omitempty"`
	beforeDispatch func() error
}

// JoinResult is meeting.join's result.
type JoinResult struct {
	MeetingID string `json:"meeting_id"`
	Platform  string `json:"platform"`
	State     State  `json:"state"`
}

// StatusResult is meeting.status's result.
type StatusResult struct {
	State        State    `json:"state"`
	Participants []Person `json:"participants"`
}

// TranscriptResult is transcript.read's result.
type TranscriptResult struct {
	Segments []json.RawMessage `json:"segments"`
	NextSeq  int64             `json:"next_seq"`
}

type idArgs struct {
	MeetingID string `json:"meeting_id"`
}

type textArgs struct {
	MeetingID string `json:"meeting_id"`
	Text      string `json:"text"`
}

// Join calls meeting.join.
func (c *Client) Join(ctx context.Context, a JoinArgs) (JoinResult, error) {
	var r JoinResult
	err := c.call(ctx, "meeting.join", a, &r, a.beforeDispatch)
	return r, err
}

// Leave calls meeting.leave.
func (c *Client) Leave(ctx context.Context, nativeID string) (State, error) {
	var r struct {
		State State `json:"state"`
	}
	err := c.Call(ctx, "meeting.leave", idArgs{nativeID}, &r)
	return r.State, err
}

// Status calls meeting.status.
func (c *Client) Status(ctx context.Context, nativeID string) (StatusResult, error) {
	var r StatusResult
	err := c.Call(ctx, "meeting.status", idArgs{nativeID}, &r)
	return r, err
}

// TranscriptRead calls transcript.read.
func (c *Client) TranscriptRead(ctx context.Context, nativeID string, afterSeq int64) (TranscriptResult, error) {
	var r TranscriptResult
	args := map[string]any{"meeting_id": nativeID}
	if afterSeq > 0 {
		args["after_seq"] = afterSeq
	}
	err := c.Call(ctx, "transcript.read", args, &r)
	return r, err
}

// meetingProseKeys are the tool-argument fields that become meeting chat or
// speech.
var meetingProseKeys = prosefilter.Keys{"text": prosefilter.Prose, "texts": prosefilter.Prose}

// ChatPost calls chat.post.
func (c *Client) ChatPost(ctx context.Context, nativeID, text string) error {
	return c.Call(ctx, "chat.post", textArgs{nativeID, text}, nil)
}

// confirmArgs is chat.post with confirm:true (quorum 0.2.2+).
type confirmArgs struct {
	MeetingID string `json:"meeting_id"`
	Text      string `json:"text"`
	Confirm   bool   `json:"confirm"`
}

// DeliveryConfirmed is chat.post's delivery when the platform reported the
// line in the meeting.
const DeliveryConfirmed = "confirmed"

// ChatPostConfirmed calls chat.post with confirm:true and reports whether
// Quorum confirmed the line reached the meeting. A send the platform
// reports as failed is an error (platform_unavailable); an older Quorum that
// answers no delivery, or a delivery Quorum could not verify in time, is
// (false, nil): not known to be posted.
func (c *Client) ChatPostConfirmed(ctx context.Context, nativeID, text string) (bool, error) {
	var r struct {
		Delivery string `json:"delivery"`
	}
	if err := c.Call(ctx, "chat.post", confirmArgs{nativeID, text, true}, &r); err != nil {
		return false, err
	}
	return r.Delivery == DeliveryConfirmed, nil
}

// Say calls say.
func (c *Client) Say(ctx context.Context, nativeID, text string) error {
	_, err := c.SayTimed(ctx, nativeID, text)
	return err
}

// SayTimed calls say and returns what Quorum reported about the line: its
// length and, from quorum 0.5.0, the wait before the first audio. An older
// Quorum answers the zero value for what it does not report.
func (c *Client) SayTimed(ctx context.Context, nativeID, text string) (SayResult, error) {
	var r SayResult
	err := c.Call(ctx, "say", textArgs{nativeID, text}, &r)
	return r, err
}

// prewarmArgs is say.prewarm's input.
type prewarmArgs struct {
	MeetingID string   `json:"meeting_id"`
	Texts     []string `json:"texts"`
}

// SayPrewarm calls say.prewarm (quorum 0.5.0+): Quorum synthesizes the
// lines in the background so a later say of the same text starts at once.
// An older Quorum answers invalid_argument for the unknown tool.
func (c *Client) SayPrewarm(ctx context.Context, nativeID string, texts []string) error {
	return c.Call(ctx, "say.prewarm", prewarmArgs{nativeID, texts}, nil)
}

// Delete calls meeting.delete, erasing the transcript upstream.
func (c *Client) Delete(ctx context.Context, nativeID string) error {
	return c.Call(ctx, "meeting.delete", idArgs{nativeID}, nil)
}

// Events streams GET /events and calls fn for each event until the stream
// ends, ctx ends, or fn returns false. nativeID "" follows every meeting
// (the stream that carries Discord `/hive join` requests); afterSeq > 0
// replays buffered segment and chat events after that seq first. It
// returns nil when fn stopped it, ErrStreamGone on 404, and an error
// otherwise (including a clean end of stream, so callers reconnect).
func (c *Client) Events(ctx context.Context, nativeID string, afterSeq int64, fn func(Event) bool) error {
	q := url.Values{}
	if nativeID != "" {
		q.Set("meeting_id", nativeID)
	}
	if afterSeq > 0 {
		q.Set("after_seq", strconv.FormatInt(afterSeq, 10))
	}
	target := c.base + "/events"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("quorum events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrStreamGone
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("quorum events: HTTP %d", resp.StatusCode)
	}
	stopped := false
	err = readSSE(resp.Body, func(data []byte) bool {
		ev, perr := parseEvent(data)
		if perr != nil {
			return true
		}
		if !fn(ev) {
			stopped = true
			return false
		}
		return true
	})
	if stopped {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quorum events: %w", err)
	}
	return errors.New("quorum events: stream ended")
}

// readSSE parses a text/event-stream body and calls fn with each event's
// data (multi-line data joined with \n) until fn returns false or the body
// ends.
func readSSE(r io.Reader, fn func([]byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	var data []byte
	has := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if has && !fn(data) {
				return nil
			}
			data, has = data[:0], false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if has {
			data = append(data, '\n')
		}
		data = append(data, value...)
		has = true
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if has {
		fn(data)
	}
	return nil
}
