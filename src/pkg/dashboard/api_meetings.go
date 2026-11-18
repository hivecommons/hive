package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

// Meetings API (hivecommons/hive#18292, quorum.contract/v1 section 3). Every
// route is owner-only: a meeting transcript is private, and sending hive
// into a meeting acts in the owner's name. pkg/dashboard never imports
// pkg/meetings (its import ratchet); the meeting manager and live loop
// arrive through MeetingsService, which cmd/hive implements.

// MeetingJoinRequest is POST /api/meetings: a meeting URL, or a Discord
// guild and voice channel.
type MeetingJoinRequest struct {
	URL       string `json:"url,omitempty"`
	Platform  string `json:"platform,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Voice     bool   `json:"voice,omitempty"`
}

// MeetingFrame is one live update for the panel: a meeting record change
// ("meeting"), a transcript line ("line"), a Spek intent ("intent"), a
// research item ("research"), notes ("notes") or a dropped intent
// ("dropped").
type MeetingFrame struct {
	MeetingID string `json:"meeting_id"`
	Kind      string `json:"kind"`
	Data      any    `json:"data,omitempty"`
}

// MeetingFrameMeeting is the kind carried on /api/events as well.
const MeetingFrameMeeting = "meeting"

// Errors a MeetingsService returns (wrapped), mapped to HTTP statuses.
var (
	ErrMeetingNotFound = errors.New("meeting not found")
	ErrMeetingInvalid  = errors.New("invalid meeting request")
	ErrMeetingConflict = errors.New("meeting is not in a state that allows this")
)

// MeetingsService is the meetings feature as the dashboard sees it. Values
// are JSON-encoded as returned.
type MeetingsService interface {
	Join(ctx context.Context, req MeetingJoinRequest, invitedBy string) (any, error)
	List() (any, error)
	Get(id string) (any, error)
	Leave(ctx context.Context, id string) (any, error)
	SetVoice(id string, enabled bool) (any, error)
	ApproveResearch(id, rid string) error
	// ApproveRequest / DeclineRequest answer a platform join request
	// (Discord `/hive join`) waiting for the owner.
	ApproveRequest(ctx context.Context, id string, voice bool) (any, error)
	DeclineRequest(ctx context.Context, id string) (any, error)
	// Delete removes a finished meeting or a pending join request for good
	// (ErrMeetingConflict while hive is still in it); ClearFinished deletes
	// every finished meeting that ended before the cutoff.
	Delete(ctx context.Context, id string) (MeetingDeleted, error)
	ClearFinished(ctx context.Context, before time.Time) ([]MeetingDeleted, error)
	// Live is the panel snapshot: meeting, transcript, live state.
	Live(id string) (any, error)
	Subscribe(fn func(MeetingFrame)) (cancel func())
}

const (
	// meetingsEventName is the /api/events SSE event for meeting changes.
	meetingsEventName = "meeting"
	// meetingsWSBuffer is a panel client's frame queue; a slow client
	// drops frames rather than stalling the meeting.
	meetingsWSBuffer = 64
	// meetingsWSWriteTimeout bounds one frame write.
	meetingsWSWriteTimeout = 5 * time.Second
	// meetingsCallTimeout bounds a join or leave call.
	meetingsCallTimeout = 60 * time.Second
)

type meetingsHolder struct {
	svc   MeetingsService
	unsub func()
}

// SetMeetingsService installs the meetings feature (nil removes it) and
// forwards meeting record changes to /api/events.
func (s *Server) SetMeetingsService(svc MeetingsService) {
	h := &meetingsHolder{svc: svc}
	if svc != nil {
		h.unsub = svc.Subscribe(func(f MeetingFrame) {
			if f.Kind != MeetingFrameMeeting {
				return
			}
			s.broadcastFrame(meetingsSSEFrame(f))
		})
	}
	if old := (&s.meetings).Swap(h); old != nil && old.unsub != nil {
		old.unsub()
	}
}

// meetingsSSEFrame carries only the meeting id and record (state,
// platform, timestamps); transcript and notes never go on the shared
// event stream.
func meetingsSSEFrame(f MeetingFrame) string {
	data, _ := json.Marshal(map[string]any{"meeting_id": f.MeetingID, "meeting": f.Data})
	return "event: " + meetingsEventName + "\ndata: " + string(data) + "\n\n"
}

func (s *Server) meetingsService(w http.ResponseWriter) (MeetingsService, bool) {
	h := (&s.meetings).Load()
	if h == nil || h.svc == nil {
		jsonError(w, "meetings are not enabled on this hive (Settings, Meetings)", http.StatusServiceUnavailable)
		return nil, false
	}
	return h.svc, true
}

func (s *Server) registerMeetingsRoutes() {
	s.mux.HandleFunc("GET /api/meetings/settings", s.handleMeetingsSettingsGet)
	s.mux.HandleFunc("POST /api/meetings/settings", s.handleMeetingsSettingsPost)
	s.mux.HandleFunc("POST /api/meetings", s.handleMeetingsJoin)
	s.mux.HandleFunc("GET /api/meetings", s.handleMeetingsList)
	s.mux.HandleFunc("GET /api/meetings/{id}", s.handleMeetingsGet)
	s.mux.HandleFunc("POST /api/meetings/{id}/leave", s.handleMeetingsLeave)
	s.mux.HandleFunc("POST /api/meetings/{id}/voice", s.handleMeetingsVoice)
	s.mux.HandleFunc("POST /api/meetings/{id}/research/{rid}/approve", s.handleMeetingsApprove)
	s.mux.HandleFunc("POST /api/meetings/{id}/approve", s.handleMeetingsRequestApprove)
	s.mux.HandleFunc("POST /api/meetings/{id}/decline", s.handleMeetingsRequestDecline)
	s.mux.HandleFunc("GET /api/meetings/{id}/ws", s.handleMeetingsWS)
	s.mux.HandleFunc("GET /api/meetings/budget", s.handleMeetingsBudget)
	s.registerMeetingsDeleteRoutes()
	s.mux.HandleFunc("DELETE /api/meetings/trusted/{platform}/{uid}", s.handleMeetingsUntrust)
}

// meetingsErrorStatus maps a MeetingsService error to its HTTP status: the
// dashboard sentinels to 404/400/409, anything else (Quorum refusing or
// failing a call, a store error) to 502.
func meetingsErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrMeetingNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrMeetingInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrMeetingConflict):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

// meetingsError answers a failed meetings call with its mapped status.
func (s *Server) meetingsError(w http.ResponseWriter, r *http.Request, err error) {
	s.writeMeetingsError(w, r, err, meetingsErrorStatus(err))
}

// writeMeetingsError logs a failed meetings call, then answers it. Every
// failure is logged with the error because the answer alone is not enough:
// the hosted ingress replaces a 502 body with its generic "hive unavailable"
// page, so a Discord join Quorum refused ("blocked: the bot has no
// permission...") reached the owner as "hive is restarting" and left no
// trace in the spoke log. A caller mistake (4xx) logs at Info, a dependency
// or store failure at Warn.
func (s *Server) writeMeetingsError(w http.ResponseWriter, r *http.Request, err error, status int) {
	if s.logger != nil {
		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "[meetings] call failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "error", err.Error())
	}
	jsonError(w, err.Error(), status)
}

func (s *Server) handleMeetingsJoin(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	var req MeetingJoinRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	m, err := svc.Join(ctx, req, strings.TrimSpace(r.Header.Get("X-Hive-User")))
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_join", auditDetail("platform", req.Platform, "voice", boolString(req.Voice)), "")
	jsonResponse(w, m)
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (s *Server) handleMeetingsList(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	list, err := svc.List()
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	out := map[string]any{"meetings": list}
	if ts, ok := svc.(MeetingsTrustService); ok {
		if trusted, err := ts.TrustedRequesters(); err == nil {
			out["trusted_requesters"] = trusted
		}
	}
	jsonResponse(w, s.withEmailIn(r, out))
}

func (s *Server) handleMeetingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	m, err := svc.Live(r.PathValue("id"))
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	jsonResponse(w, m)
}

func (s *Server) handleMeetingsLeave(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	m, err := svc.Leave(ctx, r.PathValue("id"))
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_leave", auditDetail("meeting", r.PathValue("id")), "")
	jsonResponse(w, m)
}

type meetingVoiceRequest struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) handleMeetingsVoice(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	var req meetingVoiceRequest
	if err := decodeBody(r, &req); err != nil || req.Enabled == nil {
		jsonError(w, `body must be {"enabled": true|false}`, http.StatusBadRequest)
		return
	}
	m, err := svc.SetVoice(r.PathValue("id"), *req.Enabled)
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_voice", auditDetail("meeting", r.PathValue("id"), "enabled", boolString(*req.Enabled)), "")
	jsonResponse(w, m)
}

func (s *Server) handleMeetingsApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	id, rid := r.PathValue("id"), r.PathValue("rid")
	if err := svc.ApproveResearch(id, rid); err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_research_approve", auditDetail("meeting", id, "rid", rid), "")
	jsonResponse(w, map[string]any{"approved": true, "rid": rid})
}

type meetingRequestApproveBody struct {
	Voice bool `json:"voice,omitempty"`
	// Always also trusts the requester (by the platform user id on the
	// request) so their later requests are approved automatically.
	Always bool `json:"always,omitempty"`
}

// MeetingsTrustService is the optional part of MeetingsService that keeps
// the trusted requesters: platform users whose join requests are approved
// without the owner, matched by user id only.
type MeetingsTrustService interface {
	// ApproveAndTrust approves the pending request and trusts its requester;
	// the user id comes from the request and is returned for the audit log.
	ApproveAndTrust(ctx context.Context, id string, voice bool, by string) (meeting any, trustedID string, err error)
	TrustedRequesters() (any, error)
	Untrust(platform, id string) (bool, error)
}

// handleMeetingsRequestApprove lets hive into a meeting someone asked for
// from the platform (Discord `/hive join`). Joining for a requester is the
// owner's call, the same invite policy as POST /api/meetings.
func (s *Server) handleMeetingsRequestApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	var body meetingRequestApproveBody
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	var m any
	var err error
	if body.Always {
		ts, ok := svc.(MeetingsTrustService)
		if !ok {
			jsonError(w, "trusted requesters are not available", http.StatusServiceUnavailable)
			return
		}
		var trustedID string
		m, trustedID, err = ts.ApproveAndTrust(ctx, r.PathValue("id"), body.Voice, requestUser(r))
		if err == nil {
			s.auditFromRequest(r, "meeting_requester_trust", auditDetail("meeting", r.PathValue("id"), "trusted_id", trustedID), "")
		}
	} else {
		m, err = svc.ApproveRequest(ctx, r.PathValue("id"), body.Voice)
	}
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_request_approve", auditDetail("meeting", r.PathValue("id"), "voice", boolString(body.Voice)), "")
	jsonResponse(w, m)
}

func (s *Server) handleMeetingsRequestDecline(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	m, err := svc.DeclineRequest(ctx, r.PathValue("id"))
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_request_decline", auditDetail("meeting", r.PathValue("id")), "")
	jsonResponse(w, m)
}

var meetingsUpgrader = websocket.Upgrader{CheckOrigin: wsSameOrigin}

// handleMeetingsWS is the live panel: a snapshot, then every frame for the
// meeting. Same-origin and owner-only, like the Jam socket it is modelled
// on (api_campaigns_jam_live.go): per-client buffered queue, drop on a slow
// client, one writer goroutine.
func (s *Server) handleMeetingsWS(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	snap, err := svc.Live(id)
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	conn, err := meetingsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	send := make(chan MeetingFrame, meetingsWSBuffer)
	send <- MeetingFrame{MeetingID: id, Kind: "snapshot", Data: snap}
	unsub := svc.Subscribe(func(f MeetingFrame) {
		if f.MeetingID != id {
			return
		}
		select {
		case send <- f:
		default:
		}
	})
	defer unsub()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case f := <-send:
			_ = conn.SetWriteDeadline(time.Now().Add(meetingsWSWriteTimeout))
			if err := conn.WriteJSON(f); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

// MeetingsSettings is GET/POST /api/meetings/settings. Secrets are never
// returned, only whether they are set. Settings, Meetings controls
// meetings.enabled, meetings.vexa.url, the Vexa tenant key
// (meetings.vexa.api_key_file), the Discord bot token
// (meetings.discord.token_file) and the transcription service token
// (meetings.transcribe_token_file) and the TTS service token
// (meetings.tts_token_file); the other meetings keys are shown
// read-only.
type MeetingsSettings struct {
	Enabled            bool   `json:"enabled"`
	EnabledEnvOverride bool   `json:"enabled_env_override,omitempty"`
	Running            bool   `json:"running"`
	VexaURL            string `json:"vexa_url"`
	VexaKeySet         bool   `json:"vexa_key_set"`
	DiscordTokenSet    bool   `json:"discord_token_set"`
	TranscribeTokenSet bool   `json:"transcribe_token_set"`
	TTSTokenSet        bool   `json:"tts_token_set"`
	RestartRequired    bool   `json:"restart_required,omitempty"`
	// SpekModel is the model Spektacular live mode would use (never a key).
	SpekModel string            `json:"spek_model"`
	Effective map[string]string `json:"effective"`
}

type meetingsSettingsRequest struct {
	Enabled      *bool   `json:"enabled,omitempty"`
	VexaURL      *string `json:"vexa_url,omitempty"`
	VexaAPIKey   *string `json:"vexa_api_key,omitempty"`
	DiscordToken *string `json:"discord_token,omitempty"`
	// TranscribeToken is the transcription service token Quorum posts
	// Discord audio with (QUORUM_TRANSCRIBE_TOKEN_FILE).
	TranscribeToken *string `json:"transcribe_token,omitempty"`
	// TTSToken is the TTS service token Quorum speaks Discord replies
	// with (QUORUM_TTS_TOKEN_FILE).
	TTSToken *string `json:"tts_token,omitempty"`
}

// meetingsSecretSet reports whether a secret resolves to a value, without
// returning it.
func meetingsSecretSet(ctx context.Context, read func(context.Context) (string, error)) bool {
	v, err := read(ctx)
	return err == nil && v != ""
}

func (s *Server) meetingsSettings(ctx context.Context, m config.MeetingsConfig) MeetingsSettings {
	h := (&s.meetings).Load()
	// The list keys meetings.persona.aliases[] and
	// meetings.persona.leave_phrases[] are shown joined with ", ".
	persona := m.Persona.Resolve()
	actions := m.Persona.Actions.Resolve()
	return MeetingsSettings{
		Enabled:            m.IsEnabled(),
		EnabledEnvOverride: config.MeetingsEnabledEnvSet(),
		Running:            h != nil && h.svc != nil,
		VexaURL:            m.VexaURL(),
		VexaKeySet:         meetingsSecretSet(ctx, m.VexaAPIKey),
		DiscordTokenSet:    meetingsSecretSet(ctx, m.DiscordToken),
		TranscribeTokenSet: meetingsSecretSet(ctx, m.TranscribeToken),
		TTSTokenSet:        meetingsSecretSet(ctx, m.TTSToken),
		SpekModel:          s.meetingsSpekModel(ctx),
		Effective: map[string]string{
			"meetings.quorum.binary":                     m.QuorumBinary(),
			"meetings.quorum.listen":                     m.QuorumListen(),
			"meetings.transcribe_url":                    m.TranscribeURLOrEmpty(),
			"meetings.tts_url":                           m.TTSURLOrEmpty(),
			"meetings.delivery":                          m.DeliveryOrDefault(),
			"meetings.voice_on_address":                  boolString(m.VoiceOnAddressEnabled()),
			"meetings.listening_tone":                    boolString(m.ListeningToneEnabled()),
			"meetings.bot_name":                          m.BotNameOrDefault(),
			"meetings.invite_policy":                     m.InvitePolicyOrDefault(),
			"meetings.research_pool_size":                strconv.Itoa(m.ResearchPoolSizeOrDefault()),
			"meetings.research_per_meeting":              strconv.Itoa(m.ResearchPerMeetingOrDefault()),
			"meetings.chat_posts_per_minute":             strconv.Itoa(m.ChatPostsPerMinuteOrDefault()),
			"meetings.chat_answer_burst":                 strconv.Itoa(m.ChatAnswerBurstOrDefault()),
			"meetings.lobby_timeout_s":                   strconv.Itoa(m.LobbyTimeoutSOrDefault()),
			"meetings.retention_days":                    strconv.Itoa(m.RetentionDaysOrDefault()),
			"meetings.consent_text":                      m.ConsentTextOrDefault(),
			"meetings.data_dir":                          m.DataDirOrDefault(),
			"meetings.budget.max_tokens_per_meeting":     strconv.Itoa(m.Budget.MaxTokensPerMeetingOrDefault()),
			"meetings.budget.max_tokens_per_hour":        strconv.Itoa(m.Budget.MaxTokensPerHourOrDefault()),
			"meetings.budget.max_tokens_per_day":         strconv.Itoa(m.Budget.MaxTokensPerDayOrDefault()),
			"meetings.budget.max_intents_per_meeting":    strconv.Itoa(m.Budget.MaxIntentsPerMeetingOrDefault()),
			"meetings.llm.mode":                          m.LLM.Mode,
			"meetings.llm.endpoint":                      m.LLM.Endpoint,
			"meetings.llm.model":                         m.LLM.Model,
			"meetings.llm.backend":                       m.LLM.Backend,
			"meetings.llm.api_key_file":                  m.LLM.APIKeyFile,
			"meetings.llm.anthropic_key_file":            m.LLM.AnthropicKeyFile,
			"meetings.llm.anthropic_model":               m.LLM.AnthropicModel,
			"meetings.llm.timeout_ms":                    strconv.Itoa(m.LLM.TimeoutMS),
			"meetings.llm.max_in_flight":                 strconv.Itoa(m.LLM.MaxInFlight),
			"meetings.llm.per_meeting_in_flight":         strconv.Itoa(meetingsPerMeetingInFlight(m.LLM)),
			"meetings.persona.enabled":                   boolString(persona.Enabled),
			"meetings.persona.preset":                    persona.Preset,
			"meetings.persona.name":                      persona.Name,
			"meetings.persona.display_name":              persona.DisplayName,
			"meetings.persona.language":                  persona.Language,
			"meetings.persona.voice":                     persona.Voice,
			"meetings.persona.humor":                     persona.Humor,
			"meetings.persona.leave_phrases":             strings.Join(persona.LeavePhrases, ", "),
			"meetings.persona.aliases":                   strings.Join(persona.Aliases, ", "),
			"meetings.persona.pronouns":                  persona.Pronouns,
			"meetings.persona.voice_type":                persona.VoiceType,
			"meetings.persona.accent":                    persona.Accent,
			"meetings.persona.intro_template":            persona.IntroTemplate,
			"meetings.persona.searxng_url":               persona.SearxngURL,
			"meetings.persona.unprompted.per_10m":        strconv.Itoa(persona.Unprompted.Per10m),
			"meetings.persona.unprompted.min_gap_s":      strconv.Itoa(persona.Unprompted.MinGapS),
			"meetings.persona.unprompted.per_meeting":    strconv.Itoa(persona.Unprompted.PerMeeting),
			"meetings.persona.unprompted.answer_wait_s":  strconv.Itoa(persona.Unprompted.AnswerWaitS),
			"meetings.persona.unprompted.min_confidence": strconv.FormatFloat(persona.Unprompted.MinConfidence, 'f', -1, 64),
			"meetings.persona.unprompted.queue_depth":    strconv.Itoa(persona.Unprompted.QueueDepth),
			"meetings.persona.unprompted.stale_s":        strconv.Itoa(persona.Unprompted.StaleS),
			// meetings.persona.actions.types[] is shown joined with ", " and
			// meetings.persona.actions.auto_approve.<key> as the auto-approved
			// types (#18406; edited at /api/meetings/actions/settings).
			"meetings.persona.actions.enabled":      boolString(actions.Enabled),
			"meetings.persona.actions.types":        strings.Join(actions.Types, ", "),
			"meetings.persona.actions.default_repo": actions.DefaultRepo,
			"meetings.persona.actions.auto_approve": strings.Join(meetingsAutoApproved(actions.AutoApprove), ", "),
		},
	}
}

// meetingsSpekModel describes the resolved meetings model.
func (s *Server) meetingsSpekModel(ctx context.Context) string {
	if s.deps == nil || s.deps.Config == nil {
		return config.MeetingsLLMNoModelReason
	}
	return s.deps.Config.ResolveMeetingsLLM(ctx).Label()
}

func (s *Server) handleMeetingsSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.meetingsSettings(r.Context(), s.deps.Config.Meetings))
}

// handleMeetingsSettingsPost (owner) updates meetings.enabled and
// meetings.vexa.url in hive.yaml and writes the Vexa tenant key, the
// Discord bot token and the transcription service token to their
// keystore-sealed secret files (an empty string
// clears one). The Quorum child reads them at boot, so a change reports
// restart_required.
func (s *Server) handleMeetingsSettingsPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return
	}
	var req meetingsSettingsRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	next := s.deps.Config.Meetings
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.VexaURL != nil {
		next.Vexa.URL = strings.TrimRight(strings.TrimSpace(*req.VexaURL), "/")
	}
	check := next
	check.Enabled = true
	if err := check.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	changed := req.Enabled != nil || req.VexaURL != nil
	if req.VexaAPIKey != nil {
		if err := config.WriteMeetingsSecret(r.Context(), next.VexaAPIKeyFile(), *req.VexaAPIKey); err != nil {
			jsonError(w, "saving the Vexa key: "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed = true
	}
	if req.DiscordToken != nil {
		if err := config.WriteMeetingsSecret(r.Context(), next.DiscordTokenFile(), *req.DiscordToken); err != nil {
			jsonError(w, "saving the Discord token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed = true
	}
	if req.TranscribeToken != nil {
		if err := config.WriteMeetingsSecret(r.Context(), next.TranscribeTokenFileOrDefault(), *req.TranscribeToken); err != nil {
			jsonError(w, "saving the transcription token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed = true
	}
	if req.TTSToken != nil {
		if err := config.WriteMeetingsSecret(r.Context(), next.TTSTokenFileOrDefault(), *req.TTSToken); err != nil {
			jsonError(w, "saving the TTS token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed = true
	}
	s.deps.Config.Meetings = next
	if err := s.saveConfig(); err != nil {
		jsonError(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "meetings_settings", auditDetail("enabled", boolString(next.Enabled), "vexa_url", next.Vexa.URL,
		"vexa_key_changed", boolString(req.VexaAPIKey != nil), "discord_token_changed", boolString(req.DiscordToken != nil),
		"transcribe_token_changed", boolString(req.TranscribeToken != nil), "tts_token_changed", boolString(req.TTSToken != nil)), "")
	out := s.meetingsSettings(r.Context(), next)
	out.RestartRequired = changed
	jsonResponse(w, out)
}

// meetingsPerMeetingInFlight is meetings.llm.per_meeting_in_flight as it
// applies: the default when unset, never above max_in_flight.
func meetingsPerMeetingInFlight(l config.MeetingsLLMConfig) int {
	n, ceiling := l.PerMeetingInFlight, l.MaxInFlight
	if n <= 0 {
		n = config.DefaultMeetingsLLMPerMeetingInFlight
	}
	if ceiling <= 0 {
		ceiling = config.DefaultMeetingsLLMMaxInFlight
	}
	return min(n, ceiling)
}

// handleMeetingsUntrust stops auto-approving a trusted requester. Owner only.
func (s *Server) handleMeetingsUntrust(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	ts, ok := svc.(MeetingsTrustService)
	if !ok {
		jsonError(w, "trusted requesters are not available", http.StatusServiceUnavailable)
		return
	}
	platform, uid := r.PathValue("platform"), r.PathValue("uid")
	found, err := ts.Untrust(platform, uid)
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	if !found {
		jsonError(w, "not a trusted requester", http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "meeting_requester_untrust", auditDetail("platform", platform, "id", uid), "")
	jsonResponse(w, map[string]any{"removed": true})
}
