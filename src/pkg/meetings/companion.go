package meetings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Desktop companion meetings (hivecommons/hive#18596, quorum
// docs/contract.md "Companion audio"). A desktop app on a person's own
// computer captures their meeting client's audio (channel "call") and their
// microphone ("mic") after they accepted the companion consent prompt, and
// hive relays the PCM to Quorum. Nothing joins the meeting itself. Betty's
// chat lines go to an outbox the companion window reads.

const (
	// CompanionConsentVersion names the consent notice the companion shows
	// before every capture; a start must acknowledge exactly this version.
	// Bump it whenever CompanionConsentText changes.
	CompanionConsentVersion = "companion-2026-11-16"
	// CompanionConsentText is the notice the companion shows (and offers to
	// copy into the meeting chat) before it captures anything.
	CompanionConsentText = "Betty, the hive's meeting assistant, will listen to this call through your own meeting app and microphone. " +
		"The audio goes only to your hive, which transcribes it, takes notes and answers questions. " +
		"The other participants are not told by the meeting app: tell them before you start, and only continue if everyone agrees. " +
		"Recording consent laws differ by place."
	// CompanionChannelCall is the meeting client's audio (everyone else).
	CompanionChannelCall = "call"
	// CompanionChannelMic is the person's own microphone.
	CompanionChannelMic = "mic"
	// CompanionSampleRate is the PCM rate the companion sends (16-bit mono).
	CompanionSampleRate = 16000
	// CompanionMaxChunkBytes is the largest audio chunk hive accepts; it
	// matches Quorum's QUORUM_COMPANION_MAX_CHUNK_BYTES default (4 s).
	CompanionMaxChunkBytes = 131072
	// companionTitleMax bounds a companion meeting title.
	companionTitleMax = 200
	// companionNameMax bounds the app and host names.
	companionNameMax = 100
	// maxCompanionReplyBytes bounds one outbox response from Quorum.
	maxCompanionReplyBytes = 1 << 20
)

// CompanionInfo is what hive records about a companion meeting's consent.
type CompanionInfo struct {
	// App is the meeting app the companion captured ("zoom", "teams", ...).
	App            string    `json:"app,omitempty"`
	ConsentVersion string    `json:"consent_version"`
	AttestedBy     string    `json:"attested_by"`
	AttestedAt     time.Time `json:"attested_at"`
}

// CompanionStart starts a companion meeting.
type CompanionStart struct {
	Title    string
	App      string
	HostName string
	// ConsentVersion must equal CompanionConsentVersion and Attested must
	// be true: the person confirmed the participants were told.
	ConsentVersion string
	Attested       bool
	// User is the hive user starting it (the meeting's InvitedBy).
	User string
}

// CompanionChunk is Quorum's answer to one audio chunk.
type CompanionChunk struct {
	Seq       int  `json:"seq"`
	Duplicate bool `json:"duplicate"`
}

// CompanionLine is one line for the companion window.
type CompanionLine struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
	At   string `json:"at"`
}

// CompanionReplies is what the companion window polls: Betty's lines after
// a cursor and the meeting's state (so the window notices a "betty leave").
// Tone asks the window to play the listening chime now (say.tone; Quorum
// sets it once per tone and only while the window is polling).
type CompanionReplies struct {
	Lines   []CompanionLine `json:"lines"`
	NextSeq int64           `json:"next_seq"`
	State   State           `json:"state"`
	Tone    bool            `json:"tone"`
}

// CompanionPage is Quorum's /companion/outbox answer: the lines after a
// cursor, the next cursor, and whether a listening tone is waiting.
type CompanionPage struct {
	Lines   []CompanionLine `json:"lines"`
	NextSeq int64           `json:"next_seq"`
	Tone    bool            `json:"tone"`
}

// ChunkGapError is an out-of-order chunk; Expected is the seq to send next.
type ChunkGapError struct{ Expected int }

func (e *ChunkGapError) Error() string {
	return fmt.Sprintf("meetings: chunk out of order; expected seq %d", e.Expected)
}

var (
	// ErrCompanionUnavailable: this hive's Quorum client cannot relay
	// companion audio.
	ErrCompanionUnavailable = errors.New("meetings: companion audio is not available")
	// ErrCompanionNotYours: the meeting is another person's companion
	// meeting (only its starter, or an owner, may use it).
	ErrCompanionNotYours = errors.New("meetings: this companion meeting belongs to someone else")
	// ErrNotCompanion: the meeting is not a companion meeting.
	ErrNotCompanion = errors.New("meetings: not a companion meeting")
)

// CompanionQuorum is the part of the Quorum client companion meetings use.
// *Client implements it; the manager checks for it at run time so the
// Quorum fakes of other tests stay as they are.
type CompanionQuorum interface {
	CompanionAudio(ctx context.Context, nativeID, channel string, seq int, pcm []byte) (CompanionChunk, error)
	CompanionOutbox(ctx context.Context, nativeID string, after int64) (CompanionPage, error)
}

// IsCompanionChannel reports whether ch is a companion audio channel.
func IsCompanionChannel(ch string) bool {
	return ch == CompanionChannelCall || ch == CompanionChannelMic
}

func (m *Manager) companionQuorum() (CompanionQuorum, error) {
	cq, ok := m.opts.Quorum.(CompanionQuorum)
	if !ok {
		return nil, ErrCompanionUnavailable
	}
	return cq, nil
}

// StartCompanion starts a companion meeting for req.User. It refuses a
// start without the current consent notice acknowledged and attested.
func (m *Manager) StartCompanion(ctx context.Context, req CompanionStart) (Meeting, error) {
	if _, err := m.companionQuorum(); err != nil {
		return Meeting{}, err
	}
	if !req.Attested || req.ConsentVersion != CompanionConsentVersion {
		return Meeting{}, fmt.Errorf("%w: acknowledge the current companion consent notice (%s) and confirm the participants were told",
			ErrInvalidRequest, CompanionConsentVersion)
	}
	user := strings.TrimSpace(req.User)
	if user == "" {
		return Meeting{}, fmt.Errorf("%w: a signed-in user is required", ErrInvalidRequest)
	}
	title := strings.TrimSpace(req.Title)
	app := strings.ToLower(strings.TrimSpace(req.App))
	host := strings.TrimSpace(req.HostName)
	if len(title) > companionTitleMax || len(app) > companionNameMax || len(host) > companionNameMax {
		return Meeting{}, fmt.Errorf("%w: title is at most %d characters, app and host_name at most %d",
			ErrInvalidRequest, companionTitleMax, companionNameMax)
	}
	if host == "" {
		host = user
	}
	now := m.opts.Now()
	mt := Meeting{
		ID: newMeetingID(), Platform: PlatformCompanion, Title: title, InvitedBy: user, State: StateRequested,
		Companion: &CompanionInfo{App: app, ConsentVersion: req.ConsentVersion, AttestedBy: user, AttestedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	return m.start(ctx, mt, JoinArgs{Platform: PlatformCompanion, DisplayName: m.opts.BotName, HostName: host})
}

// companionMeeting loads a companion meeting user may use.
func (m *Manager) companionMeeting(id, user string, asOwner bool) (Meeting, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return Meeting{}, err
	}
	if mt.Platform != PlatformCompanion {
		return Meeting{}, ErrNotCompanion
	}
	if !asOwner && mt.InvitedBy != strings.TrimSpace(user) {
		return Meeting{}, ErrCompanionNotYours
	}
	return mt, nil
}

// CompanionAudio relays chunk seq of one channel to Quorum.
func (m *Manager) CompanionAudio(ctx context.Context, id, user string, asOwner bool, channel string, seq int, pcm []byte) (CompanionChunk, error) {
	if !IsCompanionChannel(channel) || seq < 0 {
		return CompanionChunk{}, fmt.Errorf("%w: channel must be call or mic and seq non-negative", ErrInvalidRequest)
	}
	if len(pcm) > CompanionMaxChunkBytes || len(pcm)%2 != 0 {
		return CompanionChunk{}, fmt.Errorf("%w: a chunk is whole 16-bit samples, at most %d bytes", ErrInvalidRequest, CompanionMaxChunkBytes)
	}
	mt, err := m.companionMeeting(id, user, asOwner)
	if err != nil {
		return CompanionChunk{}, err
	}
	if mt.State != StateLive {
		return CompanionChunk{}, ErrNotLive
	}
	cq, err := m.companionQuorum()
	if err != nil {
		return CompanionChunk{}, err
	}
	return cq.CompanionAudio(ctx, mt.NativeID, channel, seq, pcm)
}

// CompanionReplies returns Betty's lines for the companion window after
// the cursor. A meeting that is no longer live answers its state, so the
// window can close, and the lines Quorum still held when hive left (the
// "Leaving now." line among them, companion_tail.go) until the window has
// fetched them or companionTailGrace has passed.
func (m *Manager) CompanionReplies(ctx context.Context, id, user string, asOwner bool, after int64) (CompanionReplies, error) {
	mt, err := m.companionMeeting(id, user, asOwner)
	if err != nil {
		return CompanionReplies{}, err
	}
	out := CompanionReplies{Lines: []CompanionLine{}, NextSeq: max(after, 0), State: mt.State}
	if mt.State != StateLive {
		out.Lines, out.NextSeq = m.companionTail(id, after)
		return out, nil
	}
	cq, err := m.companionQuorum()
	if err != nil {
		return CompanionReplies{}, err
	}
	page, err := cq.CompanionOutbox(ctx, mt.NativeID, after)
	if err != nil {
		return CompanionReplies{}, err
	}
	if page.Lines != nil {
		out.Lines = page.Lines
	}
	out.NextSeq, out.Tone = page.NextSeq, page.Tone
	return out, nil
}

// EndCompanion ends a companion meeting (the person stopped capturing).
func (m *Manager) EndCompanion(ctx context.Context, id, user string, asOwner bool) (Meeting, error) {
	if _, err := m.companionMeeting(id, user, asOwner); err != nil {
		return Meeting{}, err
	}
	return m.Leave(ctx, id, ReasonCompanionEnded)
}

// ReasonCompanionEnded is the state reason when the companion stopped.
const ReasonCompanionEnded = "companion_ended"

// quorumHTTPError is a non-2xx answer from a companion route.
type quorumHTTPError struct {
	Error       string `json:"error"`
	Code        string `json:"code"`
	ExpectedSeq *int   `json:"expected_seq"`
}

// companionStatusError maps a companion route's error answer to the
// manager's errors.
func companionStatusError(route string, status int, body []byte) error {
	var e quorumHTTPError
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusConflict && e.ExpectedSeq != nil:
		return &ChunkGapError{Expected: *e.ExpectedSeq}
	case status == http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrNotLive, e.Error)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, e.Error)
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return &ToolError{Tool: route, Code: CodeInvalidArgument, Message: e.Error}
	default:
		return fmt.Errorf("quorum %s: HTTP %d", route, status)
	}
}

// companionDo sends one companion request and reads a bounded body.
func (c *Client) companionDo(req *http.Request, route string) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quorum %s: %w", route, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCompanionReplyBytes))
	if err != nil {
		return nil, fmt.Errorf("quorum %s: %w", route, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, companionStatusError(route, resp.StatusCode, body)
	}
	return body, nil
}

// CompanionAudio posts one PCM chunk to Quorum's /companion/audio.
func (c *Client) CompanionAudio(ctx context.Context, nativeID, channel string, seq int, pcm []byte) (CompanionChunk, error) {
	q := url.Values{"meeting_id": {nativeID}, "channel": {channel}, "seq": {strconv.Itoa(seq)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/companion/audio?"+q.Encode(), bytes.NewReader(pcm))
	if err != nil {
		return CompanionChunk{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	body, err := c.companionDo(req, "companion/audio")
	if err != nil {
		return CompanionChunk{}, err
	}
	var out CompanionChunk
	if err := json.Unmarshal(body, &out); err != nil {
		return CompanionChunk{}, fmt.Errorf("quorum companion/audio: %w", err)
	}
	return out, nil
}

// CompanionOutbox reads Quorum's /companion/outbox after a cursor. An
// older Quorum without the tone answers no "tone", which reads as false.
func (c *Client) CompanionOutbox(ctx context.Context, nativeID string, after int64) (CompanionPage, error) {
	q := url.Values{"meeting_id": {nativeID}, "after": {strconv.FormatInt(max(after, 0), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/companion/outbox?"+q.Encode(), nil)
	if err != nil {
		return CompanionPage{}, err
	}
	body, err := c.companionDo(req, "companion/outbox")
	if err != nil {
		return CompanionPage{}, err
	}
	var out CompanionPage
	if err := json.Unmarshal(body, &out); err != nil {
		return CompanionPage{}, fmt.Errorf("quorum companion/outbox: %w", err)
	}
	return out, nil
}

var _ CompanionQuorum = (*Client)(nil)
