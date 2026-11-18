package meetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ConfirmingChat is a Quorum that can confirm a chat line reached the
// meeting (chat.post confirm:true, quorum 0.2.2+). The real Client
// implements it; the manager then marks the consent notice posted only on a
// confirmed delivery. A Quorum without it (in-process fakes) is trusted on a
// nil ChatPost error.
type ConfirmingChat interface {
	ChatPostConfirmed(ctx context.Context, nativeID, text string) (bool, error)
}

// TranscriptCursor is a Quorum that can say how far its transcript has got
// (transcript.read next_seq). The real Client implements it; the manager
// reads it once the consent notice is delivered, so a final segment Quorum
// already held when the notice landed is discarded even if the event stream
// delivers it later. A Quorum without it (in-process fakes) skips that fence.
type TranscriptCursor interface {
	TranscriptRead(ctx context.Context, nativeID string, afterSeq int64) (TranscriptResult, error)
}

// Quorum is the slice of the Quorum client the manager uses; tests fake it.
type Quorum interface {
	Join(ctx context.Context, a JoinArgs) (JoinResult, error)
	Leave(ctx context.Context, nativeID string) (State, error)
	ChatPost(ctx context.Context, nativeID, text string) error
	Say(ctx context.Context, nativeID, text string) error
	Delete(ctx context.Context, nativeID string) error
	Events(ctx context.Context, nativeID string, afterSeq int64, fn func(Event) bool) error
}

const (
	// DefaultReconnectMin is the first SSE reconnect delay.
	DefaultReconnectMin = time.Second
	// DefaultReconnectMax caps the SSE reconnect delay.
	DefaultReconnectMax = 30 * time.Second
	// DefaultCallTimeout bounds one Quorum tool call made by the manager.
	DefaultCallTimeout = 30 * time.Second
	// DefaultConsentRetry spaces consent-notice retries while chat is
	// unavailable, so a platform without chat is not called on every line.
	DefaultConsentRetry = time.Minute
	// DefaultConsentAttempts bounds how often a transient consent failure
	// (not_live, rate_limited, a dropped call) is retried before hive gives
	// up and leaves: it must not stay in a meeting that was never told.
	DefaultConsentAttempts = 3
	// ReasonConsentUndelivered is the state reason when hive left because
	// the consent notice could be delivered neither in chat nor by voice.
	ReasonConsentUndelivered = "consent_undelivered"
	// ConsentViaChat and ConsentViaVoice record how the notice was delivered.
	ConsentViaChat  = "chat"
	ConsentViaVoice = "voice"
	// ConsentSpeechWindow is how long after the notice a segment can still
	// be one that began before it (a long utterance finalized late). The
	// ConsentSpeechEnd fence applies only this long, so a platform that
	// restarts its session clock cannot silence the rest of the meeting.
	ConsentSpeechWindow = 2 * time.Minute
	// ConsentSTTLag is how long speech-to-text can take to finalize an
	// utterance after it was spoken. A segment that arrives sooner than its
	// own duration plus this lag after the notice began before the notice
	// and is discarded, even when no line was heard while consent was
	// pending (the usual case: the notice goes out on the first live state).
	ConsentSTTLag = 3 * time.Second
	// ConsentCursorAttempts is how many times markConsent asks Quorum for
	// its transcript cursor before it gives up and falls back to the
	// degraded fence.
	ConsentCursorAttempts = 2
	// ConsentDegradedWindow is how long after the notice every segment is
	// discarded when Quorum's transcript cursor could not be read: without
	// the cursor nothing says which buffered lines came before the notice,
	// so the fence fails closed on time instead.
	ConsentDegradedWindow = 30 * time.Second
	// ReasonDeclined is the state reason for a platform join request the
	// owner declined.
	ReasonDeclined = "declined"
	// ReasonHiveLeave is the state reason when someone typed or said a
	// leave phrase ("betty leave", "hive leave").
	ReasonHiveLeave = "hive_leave"
	// ReasonOwnerLeave is the state reason when the owner removed hive.
	ReasonOwnerLeave = "owner_leave"
	// ReasonQuorumLost is the state reason when Quorum no longer knows the
	// meeting (it restarted).
	ReasonQuorumLost = "quorum_lost_meeting"
)

// DefaultLeavePhrases is the leave phrase without a persona.
var DefaultLeavePhrases = []string{"hive leave"}

// leaveSep is what may stand between the words of a leave phrase:
// punctuation and an optional "please" ("hive, leave", "Betty: please
// leave"). The words must otherwise be adjacent, which keeps "we should
// leave hive out of this" from matching.
const leaveSep = `[\s,.:;!-]*(?:please[\s,]+)?`

// LeavingText is posted when a leave command is heard, just before the
// bot leaves.
const LeavingText = "Leaving now."

// LeaveMatcher matches the configured leave phrases ("betty leave", "hive
// leave") and close variants anywhere in a line.
type LeaveMatcher struct {
	re *regexp.Regexp
	// names are the words before "leave" in the phrases ("betty",
	// "hive"): a line that is only one of them and a sound-alike of
	// "leave" ("Betty Lee.", "hive leaf") asks her to leave too (scored
	// live test: "Betty leave" was transcribed "Betty Lee." and she stayed).
	names map[string]bool
}

// leaveSounds are speech-to-text spellings of "leave" accepted when the
// line is only the name and that word.
var leaveSounds = map[string]bool{
	"leave": true, "lee": true, "leaf": true, "lead": true, "leaves": true, "leaving": true,
	"lief": true, "leeve": true, "leve": true, "leigh": true,
}

// leaveWords splits a line into lowercase words for the misheard check.
var leaveWords = regexp.MustCompile(`[a-z']+`)

// matchMisheard reports a whole line that is a name plus a sound-alike of
// "leave", optionally with "please" between them.
func (l *LeaveMatcher) matchMisheard(text string) bool {
	w := leaveWords.FindAllString(strings.ToLower(text), -1)
	switch {
	case len(w) == 2:
		return l.names[w[0]] && leaveSounds[w[1]]
	case len(w) == 3:
		return l.names[w[0]] && w[1] == "please" && leaveSounds[w[2]]
	}
	return false
}

// NewLeaveMatcher builds a matcher; no usable phrase means
// DefaultLeavePhrases.
func NewLeaveMatcher(phrases []string) *LeaveMatcher {
	var alts []string
	names := map[string]bool{}
	for _, p := range phrases {
		words := strings.Fields(strings.ToLower(p))
		if len(words) == 0 {
			continue
		}
		if len(words) == 2 && words[1] == "leave" {
			names[words[0]] = true
		}
		q := make([]string, len(words))
		for i, w := range words {
			q[i] = regexp.QuoteMeta(w)
		}
		alts = append(alts, strings.Join(q, leaveSep))
	}
	if len(alts) == 0 {
		return NewLeaveMatcher(DefaultLeavePhrases)
	}
	return &LeaveMatcher{re: regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`), names: names}
}

// Match reports whether text asks the bot to leave.
func (l *LeaveMatcher) Match(text string) bool {
	return l.re.MatchString(text) || l.matchMisheard(text)
}

var defaultLeave = NewLeaveMatcher(DefaultLeavePhrases)

// IsLeaveCommand reports whether text asks hive to leave with the default
// phrase ("hive leave").
func IsLeaveCommand(text string) bool { return defaultLeave.Match(text) }

// Options configures a Manager.
type Options struct {
	Store       *Store
	Quorum      Quorum
	BotName     string
	ConsentText string
	// LeavePhrases remove the bot when typed or said (the persona's
	// leave_phrases); empty means DefaultLeavePhrases.
	LeavePhrases []string
	LobbyTimeout time.Duration
	Logger       *slog.Logger
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	CallTimeout  time.Duration
	// ConsentRetry spaces consent retries; zero means DefaultConsentRetry.
	ConsentRetry time.Duration
	// ConsentAttempts bounds consent tries after transient failures; zero
	// means DefaultConsentAttempts.
	ConsentAttempts int
	// VoiceOnAddress starts every bot able to speak (meetings.voice_on_address),
	// so voice can be turned on later in the meeting; whether hive actually
	// speaks is still the meeting's voice toggle.
	VoiceOnAddress bool
	// VoiceID is meeting.join's voice_id (the persona's TTS voice); empty
	// leaves Quorum's default.
	VoiceID string
	// AutoApprove decides whether a platform join request (Discord
	// `/hive join`) is approved without the owner. Nil approves none: every
	// request waits in the dashboard.
	AutoApprove func(requester Person) bool
	// Trust lists the platform users the owner chose to always approve,
	// matched by user id only. Nil trusts nobody.
	Trust *TrustStore
	// OnAutoApproved is called after a join request was approved without
	// the owner, so the approval can be audited.
	OnAutoApproved func(m Meeting, requester Person)
	// FixedSpokenLines returns the lines the persona says in every meeting
	// (its spoken introduction, acknowledgements, the goodbye), which hive
	// asks Quorum to pre-render when a meeting that can speak goes live
	// (say.go). Nil pre-renders nothing.
	FixedSpokenLines func(mt Meeting) []string
	// Now is the clock (tests).
	Now func() time.Time
}

// JoinRequest is a request to send hive to a meeting.
type JoinRequest struct {
	URL       string `json:"url,omitempty"`
	Platform  string `json:"platform,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Voice     bool   `json:"voice,omitempty"`
	InvitedBy string `json:"-"`
}

// Update is what listeners receive: the meeting after the change, and the
// Quorum event that caused it (nil for hive-side changes).
type Update struct {
	Meeting Meeting
	Event   *Event
}

// Manager runs meeting lifecycles. Safe for concurrent use.
type Manager struct {
	opts  Options
	log   *slog.Logger
	leave *LeaveMatcher

	mu        sync.Mutex
	consentAt map[string]time.Time
	// consentTries counts consent attempts per meeting.
	consentTries map[string]int
	watchers     map[string]context.CancelFunc
	lobby        map[string]*time.Timer
	listeners    map[int]func(Update)
	nextL        int
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	// tails keeps ended companion meetings' last lines (companion_tail.go).
	tails companionTails
}

// ErrInvalidRequest wraps a rejected JoinRequest.
var ErrInvalidRequest = errors.New("meetings: invalid request")

// ErrConsentPending is returned by PostChat before the consent notice is
// posted: nothing reaches a meeting before it.
var ErrConsentPending = errors.New("meetings: consent notice not posted yet")

// ErrNotLive is returned for an action that needs a live meeting.
var ErrNotLive = errors.New("meetings: meeting is not live")

// NewManager builds a manager. Call Resume to re-attach to meetings that
// were live before a restart, and Close on shutdown.
func NewManager(opts Options) (*Manager, error) {
	if opts.Store == nil || opts.Quorum == nil {
		return nil, errors.New("meetings: store and quorum are required")
	}
	if opts.ReconnectMin <= 0 {
		opts.ReconnectMin = DefaultReconnectMin
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = DefaultReconnectMax
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ConsentRetry <= 0 {
		opts.ConsentRetry = DefaultConsentRetry
	}
	if opts.ConsentAttempts <= 0 {
		opts.ConsentAttempts = DefaultConsentAttempts
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts: opts, log: log, leave: NewLeaveMatcher(opts.LeavePhrases),
		watchers:     map[string]context.CancelFunc{},
		consentAt:    map[string]time.Time{},
		consentTries: map[string]int{},
		lobby:        map[string]*time.Timer{},
		listeners:    map[int]func(Update){},
		ctx:          ctx, cancel: cancel,
	}, nil
}

// Subscribe registers fn for every update; the returned func unregisters
// it. fn runs on the meeting's event goroutine and must not block.
func (m *Manager) Subscribe(fn func(Update)) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextL
	m.nextL++
	m.listeners[id] = fn
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.listeners, id)
	}
}

func (m *Manager) emit(u Update) {
	m.mu.Lock()
	fns := make([]func(Update), 0, len(m.listeners))
	for _, fn := range m.listeners {
		fns = append(fns, fn)
	}
	m.mu.Unlock()
	for _, fn := range fns {
		fn(u)
	}
}

// validate normalises and checks a join request.
func (r *JoinRequest) validate() error {
	r.URL = strings.TrimSpace(r.URL)
	r.Platform = strings.ToLower(strings.TrimSpace(r.Platform))
	if r.Platform == PlatformDiscord || (r.URL == "" && (r.GuildID != "" || r.ChannelID != "")) {
		r.Platform = PlatformDiscord
		if strings.TrimSpace(r.GuildID) == "" || strings.TrimSpace(r.ChannelID) == "" {
			return fmt.Errorf("%w: discord needs guild_id and channel_id", ErrInvalidRequest)
		}
		r.URL = ""
		return nil
	}
	if r.URL == "" {
		return fmt.Errorf("%w: a meeting url (or discord guild_id and channel_id) is required", ErrInvalidRequest)
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: url must be an https meeting link", ErrInvalidRequest)
	}
	switch r.Platform {
	case "", PlatformGoogleMeet, PlatformTeams, PlatformZoom:
	default:
		return fmt.Errorf("%w: unknown platform %q", ErrInvalidRequest, r.Platform)
	}
	return nil
}

func (m *Manager) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(m.ctx, m.opts.CallTimeout)
}

// Join sends hive to a meeting: it records the meeting, calls meeting.join
// and starts following its events.
func (m *Manager) Join(ctx context.Context, req JoinRequest) (Meeting, error) {
	if err := req.validate(); err != nil {
		return Meeting{}, err
	}
	now := m.opts.Now()
	voice := req.Voice || m.opts.VoiceOnAddress
	mt := Meeting{
		ID: newMeetingID(), Platform: req.Platform, URL: req.URL, GuildID: req.GuildID, ChannelID: req.ChannelID,
		Title: strings.TrimSpace(req.Title), InvitedBy: req.InvitedBy, State: StateRequested,
		VoiceEnabled: req.Voice, VoiceCapable: voice, CreatedAt: now, UpdatedAt: now,
	}
	return m.start(ctx, mt, JoinArgs{
		URL: req.URL, DisplayName: m.opts.BotName, Platform: req.Platform, GuildID: req.GuildID, ChannelID: req.ChannelID,
		Voice: voice, VoiceID: strings.TrimSpace(m.opts.VoiceID),
	})
}

// start stores a new meeting, calls meeting.join with args and starts
// following its events. A refused join leaves the meeting failed (or
// blocked) and returns the error.
func (m *Manager) start(ctx context.Context, mt Meeting, args JoinArgs) (Meeting, error) {
	now := mt.CreatedAt
	if err := m.opts.Store.Save(mt); err != nil {
		return Meeting{}, err
	}
	res, err := m.opts.Quorum.Join(ctx, args)
	if err != nil {
		mt.State, mt.StateReason = StateFailed, err.Error()
		if IsCode(err, CodeBlocked) {
			mt.State = StateBlocked
		}
		mt.EndedAt, mt.UpdatedAt = timePtr(now), now
		_ = m.opts.Store.Save(mt)
		m.emit(Update{Meeting: mt})
		return mt, err
	}
	mt.NativeID = res.MeetingID
	if res.Platform != "" {
		mt.Platform = res.Platform
	}
	if knownStates[res.State] {
		mt.State = res.State
	} else {
		mt.State = StateJoining
	}
	mt.UpdatedAt = m.opts.Now()
	if err := m.opts.Store.Save(mt); err != nil {
		return mt, err
	}
	m.emit(Update{Meeting: mt})
	m.watch(mt)
	return mt, nil
}

// Get returns one meeting.
func (m *Manager) Get(id string) (Meeting, error) { return m.opts.Store.Get(id) }

// List returns all meetings, newest first.
func (m *Manager) List() ([]Meeting, error) { return m.opts.Store.List() }

// Transcript returns a meeting's stored transcript.
func (m *Manager) Transcript(id string) ([]Event, error) { return m.opts.Store.Transcript(id) }

// SetVoice turns voice on or off for one meeting.
func (m *Manager) SetVoice(id string, enabled bool) (Meeting, error) {
	return m.update(id, nil, func(mt *Meeting) { mt.VoiceEnabled = enabled })
}

// update applies fn to the stored meeting, saves it and emits the update.
func (m *Manager) update(id string, ev *Event, fn func(*Meeting)) (Meeting, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return Meeting{}, err
	}
	fn(&mt)
	mt.UpdatedAt = m.opts.Now()
	if err := m.opts.Store.Save(mt); err != nil {
		return mt, err
	}
	m.emit(Update{Meeting: mt, Event: ev})
	return mt, nil
}

// Leave removes hive from a meeting. A meeting Quorum no longer knows is
// still marked ended.
func (m *Manager) Leave(ctx context.Context, id, reason string) (Meeting, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return Meeting{}, err
	}
	if mt.State.Terminal() {
		return mt, nil
	}
	if mt.NativeID != "" {
		m.keepCompanionTail(ctx, mt)
		if _, err := m.opts.Quorum.Leave(ctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
			return mt, err
		}
	}
	m.stopWatch(id)
	return m.finish(id, StateEnded, reason, nil)
}

// finish moves a meeting to a terminal state.
func (m *Manager) finish(id string, st State, reason string, ev *Event) (Meeting, error) {
	m.clearLobby(id)
	return m.update(id, ev, func(mt *Meeting) {
		if mt.State.Terminal() {
			return
		}
		mt.State, mt.StateReason = st, reason
		mt.LobbySince = nil
		mt.EndedAt = timePtr(m.opts.Now())
	})
}

// PostChat posts text into a live meeting's chat. It refuses until the
// consent notice is posted, so nothing reaches a meeting before it.
func (m *Manager) PostChat(ctx context.Context, id, text string) error {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return err
	}
	if mt.State != StateLive {
		return ErrNotLive
	}
	if mt.ConsentPostedAt == nil {
		return ErrConsentPending
	}
	return m.opts.Quorum.ChatPost(ctx, mt.NativeID, text)
}

// Resume re-attaches to every meeting that was not terminal when hive
// stopped.
func (m *Manager) Resume() int {
	list, err := m.opts.Store.List()
	if err != nil {
		m.log.Warn("[meetings] listing meetings to resume", "error", err)
		return 0
	}
	n := 0
	for _, mt := range list {
		if mt.State.Terminal() || mt.NativeID == "" || (mt.State == StateRequested && mt.RequestedBy != nil) {
			continue
		}
		if mt.State == StateLobby && mt.LobbySince != nil {
			m.armLobby(mt.ID, mt.LobbySince.Add(m.opts.LobbyTimeout).Sub(m.opts.Now()))
		}
		m.watch(mt)
		n++
	}
	return n
}

// Close stops every watcher. Meetings stay as they are in Quorum.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	for id, t := range m.lobby {
		t.Stop()
		delete(m.lobby, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) watch(mt Meeting) {
	m.mu.Lock()
	if _, ok := m.watchers[mt.ID]; ok || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.watchers[mt.ID] = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer m.stopWatch(mt.ID)
		m.follow(ctx, mt.ID, mt.NativeID, mt.LastSeq)
	}()
}

func (m *Manager) stopWatch(id string) {
	m.mu.Lock()
	cancel, ok := m.watchers[id]
	delete(m.watchers, id)
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

// follow consumes the meeting's event stream, reconnecting with backoff,
// until the meeting is terminal or ctx ends.
// Reconnects replay from the last seen seq (after_seq), and replayed lines
// already seen are skipped.
func (m *Manager) follow(ctx context.Context, id, nativeID string, lastSeq int64) {
	backoff := m.opts.ReconnectMin
	for ctx.Err() == nil {
		done := false
		err := m.opts.Quorum.Events(ctx, nativeID, m.replayFrom(id, lastSeq), func(ev Event) bool {
			backoff = m.opts.ReconnectMin
			if ev.Type == EventSegment || ev.Type == EventChat {
				if ev.Seq > 0 && ev.Seq <= lastSeq {
					return true
				}
				if ev.Seq > 0 && (ev.Type == EventChat || ev.Final) {
					lastSeq = ev.Seq
				}
			}
			done = m.handle(ctx, id, ev)
			return !done
		})
		if done || ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrStreamGone) {
			_, _ = m.finish(id, StateFailed, ReasonQuorumLost, nil)
			return
		}
		m.log.Warn("[meetings] event stream dropped; reconnecting", "meeting", id, "error", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, m.opts.ReconnectMax)
	}
}

// replayFrom is the after_seq for a (re)connect. While the consent notice
// is still pending it is 0, no replay: whatever Quorum buffered while hive
// was away came before the notice and is not wanted. The stream opens with
// the meeting's state, which is where the notice is posted, so a replay
// would otherwise hand pre-consent lines over as if they came after it.
func (m *Manager) replayFrom(id string, lastSeq int64) int64 {
	if mt, err := m.opts.Store.Get(id); err == nil && mt.ConsentPostedAt == nil {
		return 0
	}
	return lastSeq
}

// handle applies one event. It reports whether the meeting is over.
func (m *Manager) handle(ctx context.Context, id string, ev Event) bool {
	switch ev.Type {
	case EventState:
		return m.handleState(ctx, id, ev)
	case EventSegment, EventChat:
		return m.handleLine(ctx, id, ev)
	default:
		return false
	}
}

func (m *Manager) handleState(ctx context.Context, id string, ev Event) bool {
	if !knownStates[ev.State] {
		m.log.Warn("[meetings] unknown state from quorum", "meeting", id, "state", string(ev.State))
		return false
	}
	if ev.State.Terminal() {
		_, _ = m.finish(id, ev.State, ev.Reason, &ev)
		return true
	}
	now := m.opts.Now()
	wentLive := false
	mt, err := m.update(id, &ev, func(mt *Meeting) {
		if mt.State.Terminal() {
			return
		}
		mt.State, mt.StateReason = ev.State, ev.Reason
		if ev.State == StateLobby {
			if mt.LobbySince == nil {
				mt.LobbySince = timePtr(now)
			}
		} else {
			mt.LobbySince = nil
		}
		if ev.State == StateLive && mt.LiveAt == nil {
			mt.LiveAt = timePtr(now)
			wentLive = true
		}
	})
	if err != nil {
		m.log.Warn("[meetings] saving state", "meeting", id, "error", err)
		return false
	}
	if mt.State == StateLobby {
		m.armLobby(id, mt.LobbySince.Add(m.opts.LobbyTimeout).Sub(now))
	} else {
		m.clearLobby(id)
	}
	if wentLive {
		m.prewarmSpeech(mt)
	}
	if mt.State == StateLive && m.ensureConsent(ctx, mt) {
		return true
	}
	return mt.State.Terminal()
}

// ensureConsent delivers the consent notice once, the first thing hive
// says in the meeting, and reports whether hive left the meeting because it
// could not. The notice counts as posted only when Quorum confirms the chat
// line reached the meeting. If chat cannot deliver it and the bot joined
// able to speak (voice_capable, whatever the voice reply setting), hive
// speaks it instead; if neither works, hive leaves
// (ReasonConsentUndelivered) rather than stay in a meeting that was never
// told. Transient failures (the bot not admitted yet, a rate limit, a
// dropped call) are retried on later events, at most ConsentAttempts times.
func (m *Manager) ensureConsent(ctx context.Context, mt Meeting) bool {
	if mt.ConsentPostedAt != nil || mt.State != StateLive {
		return false
	}
	now := m.opts.Now()
	m.mu.Lock()
	last, tried := m.consentAt[mt.ID]
	if tried && now.Sub(last) < m.opts.ConsentRetry {
		m.mu.Unlock()
		return false
	}
	m.consentAt[mt.ID] = now
	m.consentTries[mt.ID]++
	attempt := m.consentTries[mt.ID]
	m.mu.Unlock()

	chatCh := make(chan error, 1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		chatCh <- m.postConsentChat(ctx, mt)
	}()
	var chatErr error
	spoken := false
	var sayErr error
	if mt.VoiceCapable {
		select {
		case chatErr = <-chatCh:
		case <-time.After(ConsentChatGrace):
			// A chat post that is still waiting (Teams holds it up to 10 s
			// for a composer) never holds the spoken notice back.
			sayErr = m.speakConsent(ctx, mt)
			spoken = sayErr == nil
			chatErr = <-chatCh
		}
	} else {
		chatErr = <-chatCh
	}
	if chatErr == nil {
		if !spoken {
			m.markConsent(ctx, mt, ConsentViaChat)
		}
		return false
	}
	more := attempt < m.opts.ConsentAttempts
	if !spoken && mt.VoiceCapable && (!consentTransient(chatErr) || isNoComposer(chatErr)) {
		sayErr = m.speakConsent(ctx, mt)
		spoken = sayErr == nil
	}
	if spoken {
		if consentTransient(chatErr) && more {
			m.retryConsentChat(mt, m.opts.ConsentAttempts-attempt)
		}
		return false
	}
	if consentTransient(chatErr) && more && sayErr == nil {
		m.log.Warn("[meetings] consent notice not posted; will retry", "meeting", mt.ID, "attempt", attempt, "error", chatErr)
		if mt.ConsentError != chatErr.Error() {
			_, _ = m.update(mt.ID, nil, func(x *Meeting) { x.ConsentError = chatErr.Error() })
		}
		return false
	}
	why := "chat unavailable (" + chatErr.Error() + ")"
	if !mt.VoiceCapable {
		why += ", the bot joined without voice"
	} else if sayErr != nil {
		why += ", voice failed (" + sayErr.Error() + ")"
	}
	m.log.Warn("[meetings] consent notice could not be delivered; leaving the meeting", "meeting", mt.ID, "reason", why)
	_, _ = m.update(mt.ID, nil, func(x *Meeting) { x.ConsentError = why })
	lctx, cancel := context.WithTimeout(m.ctx, m.opts.CallTimeout)
	defer cancel()
	if _, err := m.Leave(lctx, mt.ID, ReasonConsentUndelivered); err != nil {
		// The bot may still be in the meeting: record the end anyway so
		// nothing else is said, and keep the error visible.
		m.log.Warn("[meetings] leave after undelivered consent failed", "meeting", mt.ID, "error", err)
		m.stopWatch(mt.ID)
		_, _ = m.finish(mt.ID, StateEnded, ReasonConsentUndelivered, nil)
	}
	return true
}

// ConsentChatGrace is how long the consent chat post is waited for before a
// bot that can speak says the notice too.
const ConsentChatGrace = 2 * time.Second

// isNoComposer reports a Teams chat that had no composer on screen: the
// platform could not take the line yet, and may later (the control bar and
// chat panel mount some seconds after admission).
func isNoComposer(err error) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Code == CodePlatformUnavailable && strings.Contains(te.Message, "no_composer")
}

// speakConsent says the consent notice and records it delivered by voice.
// The notice is not a voice reply: the voice setting governs whether she
// answers out loud, but telling the room it is recorded comes first.
func (m *Manager) speakConsent(ctx context.Context, mt Meeting) error {
	cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	if err := m.sayConsent(cctx, mt.NativeID); err != nil {
		return err
	}
	m.log.Info("[meetings] consent notice spoken; chat could not deliver it", "meeting", mt.ID)
	m.markConsent(ctx, mt, ConsentViaVoice)
	return nil
}

// retryConsentChat keeps posting the notice in chat after it was spoken,
// at most tries more times, ConsentRetry apart, until a post is confirmed
// or the meeting is over.
func (m *Manager) retryConsentChat(mt Meeting, tries int) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for i := 0; i < tries; i++ {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(m.opts.ConsentRetry):
			}
			if cur, err := m.Get(mt.ID); err != nil || cur.State != StateLive {
				return
			}
			err := m.postConsentChat(m.ctx, mt)
			if err == nil {
				m.log.Info("[meetings] consent notice also posted in chat", "meeting", mt.ID, "attempt", i+2)
				return
			}
			if !consentTransient(err) && !isNoComposer(err) {
				return
			}
		}
	}()
}

// errConsentNotSpoken is a voice notice Quorum accepted but reported with no
// audio: nothing (or too little to count) reached the room.
var errConsentNotSpoken = errors.New("the voice notice reported no audio")

// sayConsent speaks the consent notice. A Quorum that reports timing must
// report a positive duration: a bot that could not synthesize the line
// answers "spoken" with a zero length, and that is not a delivered notice.
// A Quorum that cannot report timing is judged by its error alone.
func (m *Manager) sayConsent(ctx context.Context, nativeID string) error {
	res, err := m.say(ctx, nativeID, m.opts.ConsentText)
	if err != nil {
		return err
	}
	if _, timed := m.opts.Quorum.(TimedSayer); timed && res.DurationMS <= 0 {
		return errConsentNotSpoken
	}
	return nil
}

// postConsentChat posts the consent notice in chat. It returns nil only
// when the line is known to be in the meeting.
func (m *Manager) postConsentChat(ctx context.Context, mt Meeting) error {
	cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	cq, ok := m.opts.Quorum.(ConfirmingChat)
	if !ok {
		return m.opts.Quorum.ChatPost(cctx, mt.NativeID, m.opts.ConsentText)
	}
	confirmed, err := cq.ChatPostConfirmed(cctx, mt.NativeID, m.opts.ConsentText)
	if err != nil {
		return err
	}
	if !confirmed {
		return errConsentUnconfirmed
	}
	return nil
}

// errConsentUnconfirmed is a chat post Quorum accepted but could not
// confirm reached the meeting (or a Quorum too old to say).
var errConsentUnconfirmed = errors.New("chat delivery not confirmed")

// consentTransient reports whether a failed consent post is worth retrying
// rather than a definite "chat cannot deliver this": the bot not admitted
// yet, a rate limit, or a call that never got an answer.
func consentTransient(err error) bool {
	if isNoComposer(err) {
		return true
	}
	if errors.Is(err, errConsentUnconfirmed) || IsCode(err, CodePlatformUnavailable) ||
		IsCode(err, CodeInvalidArgument) || IsCode(err, CodeNotFound) {
		return false
	}
	return true
}

// markConsent records the notice as delivered, and raises ConsentSeq to
// Quorum's transcript cursor at that moment when Quorum can report it.
func (m *Manager) markConsent(ctx context.Context, mt Meeting, via string) {
	at := m.opts.Now()
	var fence int64
	var degraded bool
	if tc, ok := m.opts.Quorum.(TranscriptCursor); ok {
		for attempt := 1; attempt <= ConsentCursorAttempts; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
			res, err := tc.TranscriptRead(cctx, mt.NativeID, mt.LastSeq)
			cancel()
			if err == nil {
				fence, degraded = res.NextSeq, false
				break
			}
			degraded = true
			m.log.Warn("[meetings] reading quorum's transcript cursor for the consent fence", "meeting", mt.ID, "attempt", attempt, "error", err)
		}
	}
	_, _ = m.update(mt.ID, nil, func(x *Meeting) {
		x.ConsentPostedAt, x.ConsentVia, x.ConsentError = timePtr(at), via, ""
		x.ConsentSeq = max(x.ConsentSeq, fence)
		x.ConsentFenceDegraded = degraded
	})
	// The notice is the first of hive's own lines in the meeting record.
	kind := ReplyKindChat
	if via == ConsentViaVoice {
		kind = ReplyKindVoice
	}
	m.RecordReply(mt.ID, m.opts.ConsentText, kind, ReplyTriggerConsent, at, 0, nil)
}

// fromHive reports whether a line is hive's own (its chat posts echo back).
// Platforms may decorate the bot's display name ("Betty Bee from the hive
// (Guest)"), so a trailing parenthetical is ignored.
func (m *Manager) fromHive(id string, ev Event) bool {
	if m.opts.BotName != "" && (ev.Author != nil && IsBotName(ev.Author.Name, m.opts.BotName) ||
		ev.Speaker != nil && IsBotName(ev.Speaker.Name, m.opts.BotName)) {
		return true
	}
	if ev.Type == EventChat && strings.TrimSpace(ev.Text) == strings.TrimSpace(m.opts.ConsentText) {
		return true
	}
	// Her own recent line under another name: a segment is judged only when
	// final, and chat always.
	if ev.Type == EventChat || ev.Type == EventSegment && ev.Final {
		return m.opts.Store.IsOwnEcho(id, ev.Text, m.opts.Now())
	}
	return false
}

func (m *Manager) handleLine(ctx context.Context, id string, ev Event) bool {
	if m.fromHive(id, ev) {
		return false
	}
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return false
	}
	pending := mt.ConsentPostedAt == nil
	if m.ensureConsent(ctx, mt) {
		return true
	}
	if pending {
		// The line came before the notice, even if this line is what got
		// the notice delivered just now.
		return m.dropPreConsent(ctx, id, ev, true)
	}
	if beforeConsent(mt, ev, m.opts.Now()) {
		return m.dropPreConsent(ctx, id, ev, false)
	}
	if ev.Type == EventSegment && !ev.Final {
		m.emit(Update{Meeting: mt, Event: &ev})
		return false
	}
	if err := m.opts.Store.AppendTranscript(id, ev); err != nil {
		m.log.Warn("[meetings] appending transcript", "meeting", id, "error", err)
	} else if cur, err := m.opts.Store.Get(id); err == nil && ev.Seq > cur.LastSeq {
		// Quietly: the replay cursor is bookkeeping, not an update.
		cur.LastSeq = ev.Seq
		_ = m.opts.Store.Save(cur)
	}
	if m.leave.Match(ev.Text) {
		m.log.Info("[meetings] leave requested in the meeting", "meeting", id, "by", ev.Who())
		cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
		defer cancel()
		if mt.ConsentPostedAt != nil {
			// Say so before going, so the room knows the command was heard,
			// and only go once the line is known to be out (say.go).
			m.goodbye(cctx, mt, ev, m.opts.Now())
		}
		m.keepCompanionTail(cctx, mt)
		if _, err := m.opts.Quorum.Leave(cctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
			m.log.Warn("[meetings] leave failed", "meeting", id, "error", err)
		}
		_, _ = m.finish(id, StateEnded, ReasonHiveLeave, &ev)
		return true
	}
	if cur, err := m.opts.Store.Get(id); err == nil {
		mt = cur
	}
	m.emit(Update{Meeting: mt, Event: &ev})
	return false
}

// beforeConsent reports whether a line that arrived after the notice was
// delivered still belongs to the time before it: a seq Quorum had already
// reached when the notice landed, a chat line stamped earlier, or a
// segment that began before the last speech heard while consent was pending.
func beforeConsent(mt Meeting, ev Event, now time.Time) bool {
	if mt.ConsentPostedAt == nil {
		return true
	}
	if ev.Seq > 0 && ev.Seq <= mt.ConsentSeq {
		return true
	}
	if ev.Type == EventChat {
		var at time.Time
		if len(ev.At) > 0 && json.Unmarshal(ev.At, &at) == nil && !at.IsZero() && at.Before(*mt.ConsentPostedAt) {
			return true
		}
		return false
	}
	since := now.Sub(*mt.ConsentPostedAt)
	if mt.ConsentFenceDegraded && since < ConsentDegradedWindow {
		return true
	}
	if mt.ConsentSpeechEnd > 0 && since < ConsentSpeechWindow {
		if start, ok := sessionSecs(ev.Start); ok && start < mt.ConsentSpeechEnd {
			return true
		}
	}
	return speechBeganBeforeNotice(ev, since)
}

// speechBeganBeforeNotice reports whether a segment that arrived since
// after the notice must have begun before it: an utterance of duration d
// finalized by STT with a lag of at most ConsentSTTLag began no earlier
// than d+lag before it arrived. A segment without a usable start and end is
// not judged here.
func speechBeganBeforeNotice(ev Event, since time.Duration) bool {
	if ev.Type != EventSegment || since >= ConsentSpeechWindow {
		return false
	}
	start, okStart := sessionSecs(ev.Start)
	end, okEnd := sessionSecs(ev.End)
	if !okStart || !okEnd || end < start {
		return false
	}
	spoken := time.Duration((end - start) * float64(time.Second))
	return since < spoken+ConsentSTTLag
}

// sessionSecs decodes a segment's start or end (seconds from the session
// start).
func sessionSecs(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil {
		return 0, false
	}
	return v, true
}

// dropPreConsent discards a line from before the consent notice: it is not
// stored and not handed to listeners (Betty, Spektacular, research, facts,
// the Inbox hand-off), only counted. The replay cursor moves past it so a
// reconnect or restart never brings it back. A leave phrase is still
// honoured: a participant can always remove the bot. pending says the
// notice was not delivered when the line arrived, so the line also raises
// the fences that catch its late-finalized neighbours.
func (m *Manager) dropPreConsent(ctx context.Context, id string, ev Event, pending bool) bool {
	if cur, err := m.opts.Store.Get(id); err == nil {
		cur.PreConsentDropped++
		if ev.Seq > cur.LastSeq && (ev.Type == EventChat || ev.Final) {
			cur.LastSeq = ev.Seq
		}
		if pending {
			cur.ConsentSeq = max(cur.ConsentSeq, ev.Seq)
			if end, ok := sessionSecs(ev.End); ok {
				cur.ConsentSpeechEnd = max(cur.ConsentSpeechEnd, end)
			}
		}
		// Quietly, like the replay cursor: bookkeeping, not an update.
		if err := m.opts.Store.Save(cur); err != nil {
			m.log.Warn("[meetings] counting a pre-consent line", "meeting", id, "error", err)
		}
	}
	if ev.Type == EventSegment && !ev.Final || !m.leave.Match(ev.Text) {
		return false
	}
	m.log.Info("[meetings] leave requested before the consent notice was delivered", "meeting", id)
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	if _, err := m.opts.Quorum.Leave(cctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
		m.log.Warn("[meetings] leave failed", "meeting", id, "error", err)
	}
	// No event on the update: the line itself is not kept or forwarded.
	_, _ = m.finish(id, StateEnded, ReasonHiveLeave, nil)
	return true
}

// armLobby (re)starts the lobby timer: when it fires and the meeting is
// still in the lobby, hive leaves and marks it failed with lobby_timeout.
func (m *Manager) armLobby(id string, after time.Duration) {
	if m.opts.LobbyTimeout <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.lobby[id]; ok {
		return
	}
	m.lobby[id] = time.AfterFunc(max(after, 0), func() { m.lobbyExpired(id) })
}

func (m *Manager) clearLobby(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.lobby[id]; ok {
		t.Stop()
		delete(m.lobby, id)
	}
}

func (m *Manager) lobbyExpired(id string) {
	m.mu.Lock()
	delete(m.lobby, id)
	m.mu.Unlock()
	mt, err := m.opts.Store.Get(id)
	if err != nil || mt.State != StateLobby {
		return
	}
	ctx, cancel := m.callCtx()
	defer cancel()
	if _, err := m.opts.Quorum.Leave(ctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
		m.log.Warn("[meetings] leaving after lobby timeout", "meeting", id, "error", err)
	}
	m.stopWatch(id)
	_, _ = m.finish(id, StateFailed, CodeLobbyTimeout, nil)
}

// IsBotName reports whether a participant name is the bot's display name,
// ignoring case, surrounding space and one trailing parenthetical the
// platform adds ("(Guest)", "(bot)").
func IsBotName(name, botName string) bool {
	name, botName = strings.TrimSpace(name), strings.TrimSpace(botName)
	if botName == "" {
		return false
	}
	if strings.EqualFold(name, botName) {
		return true
	}
	if strings.HasSuffix(name, ")") {
		if i := strings.LastIndex(name, "("); i > 0 {
			return strings.EqualFold(strings.TrimSpace(name[:i]), botName)
		}
	}
	return false
}
