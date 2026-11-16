package meetings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

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

// LeaveMatcher matches the configured leave phrases ("betty leave", "hive
// leave") and close variants anywhere in a line.
type LeaveMatcher struct{ re *regexp.Regexp }

// NewLeaveMatcher builds a matcher; no usable phrase means
// DefaultLeavePhrases.
func NewLeaveMatcher(phrases []string) *LeaveMatcher {
	var alts []string
	for _, p := range phrases {
		words := strings.Fields(strings.ToLower(p))
		if len(words) == 0 {
			continue
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
	return &LeaveMatcher{re: regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)}
}

// Match reports whether text asks the bot to leave.
func (l *LeaveMatcher) Match(text string) bool { return l.re.MatchString(text) }

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
	watchers  map[string]context.CancelFunc
	lobby     map[string]*time.Timer
	listeners map[int]func(Update)
	nextL     int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
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
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts: opts, log: log, leave: NewLeaveMatcher(opts.LeavePhrases),
		watchers:  map[string]context.CancelFunc{},
		consentAt: map[string]time.Time{},
		lobby:     map[string]*time.Timer{},
		listeners: map[int]func(Update){},
		ctx:       ctx, cancel: cancel,
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
	if err := m.opts.Store.Save(mt); err != nil {
		return Meeting{}, err
	}
	res, err := m.opts.Quorum.Join(ctx, JoinArgs{
		URL: req.URL, DisplayName: m.opts.BotName, Platform: req.Platform, GuildID: req.GuildID, ChannelID: req.ChannelID,
		Voice: voice, VoiceID: strings.TrimSpace(m.opts.VoiceID),
	})
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

// Say speaks text in a live meeting. The caller applies the voice policy.
func (m *Manager) Say(ctx context.Context, id, text string) error {
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
	return m.opts.Quorum.Say(ctx, mt.NativeID, text)
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
		err := m.opts.Quorum.Events(ctx, nativeID, lastSeq, func(ev Event) bool {
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
	if mt.State == StateLive {
		m.ensureConsent(ctx, mt)
	}
	return mt.State.Terminal()
}

// ensureConsent posts the consent notice once, the first thing hive says
// in the meeting. A failed post is retried on the next event.
func (m *Manager) ensureConsent(ctx context.Context, mt Meeting) {
	if mt.ConsentPostedAt != nil || mt.State != StateLive {
		return
	}
	now := m.opts.Now()
	m.mu.Lock()
	last, tried := m.consentAt[mt.ID]
	if tried && now.Sub(last) < m.opts.ConsentRetry {
		m.mu.Unlock()
		return
	}
	m.consentAt[mt.ID] = now
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	defer cancel()
	if err := m.opts.Quorum.ChatPost(cctx, mt.NativeID, m.opts.ConsentText); err != nil {
		// Soft failure (for example platform_unavailable while the
		// platform's chat is down): nothing else reaches the meeting until
		// the notice is posted, and the panel keeps showing everything.
		m.log.Warn("[meetings] consent notice not posted; will retry", "meeting", mt.ID, "error", err)
		if mt.ConsentError != err.Error() {
			_, _ = m.update(mt.ID, nil, func(x *Meeting) { x.ConsentError = err.Error() })
		}
		return
	}
	_, _ = m.update(mt.ID, nil, func(x *Meeting) { x.ConsentPostedAt, x.ConsentError = timePtr(m.opts.Now()), "" })
}

// fromHive reports whether a line is hive's own (its chat posts echo back).
// Platforms may decorate the bot's display name ("Betty Bee from the hive
// (Guest)"), so a trailing parenthetical is ignored.
func (m *Manager) fromHive(ev Event) bool {
	if m.opts.BotName != "" && (ev.Author != nil && IsBotName(ev.Author.Name, m.opts.BotName) ||
		ev.Speaker != nil && IsBotName(ev.Speaker.Name, m.opts.BotName)) {
		return true
	}
	return ev.Type == EventChat && strings.TrimSpace(ev.Text) == strings.TrimSpace(m.opts.ConsentText)
}

func (m *Manager) handleLine(ctx context.Context, id string, ev Event) bool {
	if m.fromHive(ev) {
		return false
	}
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return false
	}
	m.ensureConsent(ctx, mt)
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
