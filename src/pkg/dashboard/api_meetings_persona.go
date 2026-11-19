package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Meeting attendee settings (hivecommons/hive#18396): Settings, Meeting
// attendee reads and saves meetings.persona (the preset and any field
// overrides), plays a sample line through the meetings TTS service, and
// shows the attendee's email address and inviters. Owner or read-write,
// like the Calendar. The meetings feature resolves the persona at boot, so
// a saved change reports restart_required until the hive restarts.
//
// The TTS service (meetings.tts_url and its token) and the email-in data
// (registered calendar emails, proof status, the approval queue) arrive
// through SetMeetingsTTSResolver and SetMeetingsAttendeeEmail, which
// cmd/hive wires once those features exist; until then the sample says TTS
// is not configured and the email section reports available: false.

const (
	// meetingsSampleMaxBytes caps the audio a sample returns.
	meetingsSampleMaxBytes = 2 << 20
	// meetingsSampleMaxChars caps a caller-supplied sample line.
	meetingsSampleMaxChars = 200
	// meetingsSampleTimeout bounds one TTS call.
	meetingsSampleTimeout = 20 * time.Second
	// meetingsTTSSpeechPath is the OpenAI-compatible speech route Kokoro
	// serves.
	meetingsTTSSpeechPath = "/v1/audio/speech"
	// meetingsTTSModel is the model name Kokoro-FastAPI accepts.
	meetingsTTSModel = "kokoro"
	// meetingsTTSFormat is the audio format asked for; every browser plays it.
	meetingsTTSFormat = "mp3"
	// meetingsTTSDefaultContentType is used when the TTS service sends no
	// audio content type.
	meetingsTTSDefaultContentType = "audio/mpeg"
	// meetingsTTSErrorBodyMax bounds the TTS error text echoed back.
	meetingsTTSErrorBodyMax = 300
	// meetingsAttendeeMailbox is the local part of the attendee address.
	meetingsAttendeeMailbox = "hive"
)

// ErrMeetingsTTSNotConfigured is returned by a MeetingsTTSResolver when
// no TTS service is set.
var ErrMeetingsTTSNotConfigured = errors.New("TTS not configured: set meetings.tts_url (and its token) to play samples")

// MeetingsTTSConfig is the TTS service a sample is spoken through.
type MeetingsTTSConfig struct {
	URL   string
	Token string
}

// MeetingsTTSResolver returns the TTS service, or an error wrapping
// ErrMeetingsTTSNotConfigured when none is set.
type MeetingsTTSResolver func(ctx context.Context) (MeetingsTTSConfig, error)

// MeetingsCalendarEmail is one calendar address a user registered for
// invites, with its proof status ("verified" or "pending").
type MeetingsCalendarEmail struct {
	Address    string `json:"address"`
	Proof      string `json:"proof"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

// MeetingsEmailApproval is one mail waiting for the owner: an invite or
// mail-in file from someone not on the allowlist.
type MeetingsEmailApproval struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	From       string `json:"from"`
	Subject    string `json:"subject,omitempty"`
	ReceivedAt string `json:"received_at,omitempty"`
}

// MeetingsAttendeeEmailSnapshot is the email-in state.
type MeetingsAttendeeEmailSnapshot struct {
	// Emails are the registered calendar emails by login.
	Emails          map[string][]MeetingsCalendarEmail
	Approvals       []MeetingsEmailApproval
	CalendarInvites bool
	MailInFiles     bool
}

// MeetingsAttendeeEmail is the email-in feature as the settings tab sees it.
type MeetingsAttendeeEmail interface {
	Snapshot(ctx context.Context) (MeetingsAttendeeEmailSnapshot, error)
}

// meetingsAttendeeState is the Server's attendee settings state.
type meetingsAttendeeState struct {
	mu       sync.Mutex
	bootOnce sync.Once
	// boot is what the running hive resolved at start.
	boot  meetingsAttendeeBoot
	tts   MeetingsTTSResolver
	email MeetingsAttendeeEmail
	// research reports the meeting research pool (api_meetings_research.go).
	research MeetingsResearchStatusFunc
	// client calls the TTS service; nil uses a client with
	// meetingsSampleTimeout.
	client *http.Client
	// preview caches and rate-limits the preset voice previews
	// (api_meetings_persona_preview.go).
	preview meetingsPreviewState
}

// meetingsAttendeeBoot is the attendee configuration a hive resolves at
// start; a saved change to it needs a restart.
type meetingsAttendeeBoot struct {
	Persona        config.ResolvedMeetingsPersona
	VoiceOnAddress bool
}

func meetingsAttendeeBootOf(m config.MeetingsConfig) meetingsAttendeeBoot {
	return meetingsAttendeeBoot{Persona: m.Persona.Resolve(), VoiceOnAddress: m.VoiceOnAddressEnabled()}
}

// SetMeetingsTTSResolver installs the TTS service lookup (nil removes it).
func (s *Server) SetMeetingsTTSResolver(fn MeetingsTTSResolver) {
	s.meetingsAttendee.mu.Lock()
	defer s.meetingsAttendee.mu.Unlock()
	s.meetingsAttendee.tts = fn
}

// SetMeetingsAttendeeEmail installs the email-in data (nil removes it).
func (s *Server) SetMeetingsAttendeeEmail(e MeetingsAttendeeEmail) {
	s.meetingsAttendee.mu.Lock()
	defer s.meetingsAttendee.mu.Unlock()
	s.meetingsAttendee.email = e
}

func (s *Server) meetingsAttendeeDeps() (MeetingsTTSResolver, MeetingsAttendeeEmail, *http.Client) {
	s.meetingsAttendee.mu.Lock()
	defer s.meetingsAttendee.mu.Unlock()
	c := s.meetingsAttendee.client
	if c == nil {
		c = &http.Client{Timeout: meetingsSampleTimeout}
	}
	return s.meetingsAttendee.tts, s.meetingsAttendee.email, c
}

// captureMeetingsPersonaBoot records the persona the hive booted with. It
// runs when the routes register, before any save.
func (s *Server) captureMeetingsPersonaBoot() {
	s.meetingsAttendee.bootOnce.Do(func() {
		if s.deps != nil && s.deps.Config != nil {
			s.meetingsAttendee.boot = meetingsAttendeeBootOf(s.deps.Config.Meetings)
		}
	})
}

func (s *Server) registerMeetingsPersonaRoutes() {
	s.captureMeetingsPersonaBoot()
	s.mux.HandleFunc("GET /api/meetings/persona", s.handleMeetingsPersonaGet)
	s.mux.HandleFunc("PUT /api/meetings/persona", s.handleMeetingsPersonaPut)
	s.mux.HandleFunc("POST /api/meetings/persona/sample", s.handleMeetingsPersonaSample)
	// The browser's <audio> element plays the GET form: the dashboard CSP
	// has no media-src, so audio must come from the dashboard origin.
	s.mux.HandleFunc("GET /api/meetings/persona/sample", s.handleMeetingsPersonaSample)
	s.mux.HandleFunc("GET /api/meetings/persona/preview", s.handleMeetingsPersonaPreview)
}

// MeetingsPersonaUnpromptedView is the effective unprompted caps.
type MeetingsPersonaUnpromptedView struct {
	Per10m        int     `json:"per_10m"`
	MinGapS       int     `json:"min_gap_s"`
	PerMeeting    int     `json:"per_meeting"`
	AnswerWaitS   int     `json:"answer_wait_s"`
	MinConfidence float64 `json:"min_confidence"`
	QueueDepth    int     `json:"queue_depth"`
	StaleS        int     `json:"stale_s"`
}

// MeetingsPersonaView is the effective persona.
type MeetingsPersonaView struct {
	Enabled       bool                          `json:"enabled"`
	Preset        string                        `json:"preset"`
	Name          string                        `json:"name"`
	DisplayName   string                        `json:"display_name"`
	Aliases       []string                      `json:"aliases"`
	Pronouns      string                        `json:"pronouns"`
	Language      string                        `json:"language"`
	Voice         string                        `json:"voice"`
	VoiceType     string                        `json:"voice_type"`
	Accent        string                        `json:"accent"`
	IntroTemplate string                        `json:"intro_template"`
	Intro         string                        `json:"intro"`
	LeavePhrases  []string                      `json:"leave_phrases"`
	Humor         string                        `json:"humor"`
	Unprompted    MeetingsPersonaUnpromptedView `json:"unprompted"`
	// VoiceOnAddress is meetings.voice_on_address: hive may speak when
	// addressed (otherwise replies go to chat).
	VoiceOnAddress bool `json:"voice_on_address"`
}

// MeetingsAttendeeInviter is one person on the live access list.
type MeetingsAttendeeInviter struct {
	Login       string                  `json:"login"`
	DisplayName string                  `json:"display_name,omitempty"`
	Role        string                  `json:"role"`
	Emails      []MeetingsCalendarEmail `json:"emails"`
}

// MeetingsAttendeeEmailView is the Email section.
type MeetingsAttendeeEmailView struct {
	// Available is false until email-in is enabled (hivecommons/hive#18395);
	// the tab then shows the section with its controls disabled.
	Available       bool                      `json:"available"`
	Address         string                    `json:"address"`
	Inviters        []MeetingsAttendeeInviter `json:"inviters"`
	Approvals       []MeetingsEmailApproval   `json:"approvals"`
	CalendarInvites bool                      `json:"calendar_invites"`
	MailInFiles     bool                      `json:"mail_in_files"`
	Error           string                    `json:"error,omitempty"`
}

// MeetingsPersonaSettings is GET and PUT /api/meetings/persona.
type MeetingsPersonaSettings struct {
	Effective MeetingsPersonaView            `json:"effective"`
	Presets   []config.MeetingsPersonaPreset `json:"presets"`
	// Overridden maps each meetings.persona key not taken from the preset
	// (or the defaults) to its source, "config" or "env".
	Overridden map[string]string                `json:"overridden"`
	Voices     []config.MeetingsTTSVoice        `json:"voices"`
	VoiceTypes []config.MeetingsVoiceTypeOption `json:"voice_types"`
	// RestartRequired is true when the saved persona differs from the one
	// the running hive booted with.
	RestartRequired bool                      `json:"restart_required"`
	Running         bool                      `json:"running"`
	TTSConfigured   bool                      `json:"tts_configured"`
	Email           MeetingsAttendeeEmailView `json:"email"`
	// Actions are meetings.persona.actions, the drafts people may ask the
	// persona for (#18406); they are saved through
	// PUT /api/meetings/actions/settings.
	Actions MeetingsActionsSettingsView `json:"actions"`
	// Research is the research agent pool Betty's deep dives go to, with a
	// warning when no agent can take one (hivecommons/hive#18590).
	Research MeetingsResearchView `json:"research"`
}

// resolvePersonaFields resolves the persona's fields as if it were on: a
// disabled persona resolves its name to "hive", but the tab edits (and
// samples) the persona that turning it back on would bring.
func resolvePersonaFields(p config.MeetingsPersonaConfig) config.ResolvedMeetingsPersona {
	on := true
	p.Enabled = &on
	return p.Resolve()
}

func meetingsPersonaView(m config.MeetingsConfig) MeetingsPersonaView {
	r := resolvePersonaFields(m.Persona)
	u := r.Unprompted
	return MeetingsPersonaView{
		Enabled: m.Persona.IsEnabled(), Preset: r.Preset, Name: r.Name, DisplayName: r.DisplayName,
		Aliases: nonNilStrings(r.Aliases), Pronouns: r.Pronouns, Language: r.Language, Voice: r.Voice,
		VoiceType: r.VoiceType, Accent: r.Accent, IntroTemplate: r.IntroTemplate, Intro: r.Intro(),
		LeavePhrases: nonNilStrings(r.LeavePhrases), Humor: r.Humor,
		Unprompted: MeetingsPersonaUnpromptedView{
			Per10m: u.Per10m, MinGapS: u.MinGapS, PerMeeting: u.PerMeeting, AnswerWaitS: u.AnswerWaitS,
			MinConfidence: u.MinConfidence, QueueDepth: u.QueueDepth, StaleS: u.StaleS,
		},
		VoiceOnAddress: m.VoiceOnAddressEnabled(),
	}
}

// meetingsAttendeeHost is the dashboard host the attendee address uses:
// the forwarded host behind a proxy, else the request host, without a port.
func meetingsAttendeeHost(r *http.Request) string {
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

func (s *Server) meetingsAttendeeEmail(r *http.Request, email MeetingsAttendeeEmail) MeetingsAttendeeEmailView {
	out := MeetingsAttendeeEmailView{Inviters: []MeetingsAttendeeInviter{}, Approvals: []MeetingsEmailApproval{}}
	if host := meetingsAttendeeHost(r); host != "" {
		out.Address = meetingsAttendeeMailbox + "@" + host
	}
	var snap MeetingsAttendeeEmailSnapshot
	if email != nil {
		var err error
		if snap, err = email.Snapshot(r.Context()); err != nil {
			out.Error = err.Error()
		} else {
			out.Available = true
			out.CalendarInvites, out.MailInFiles = snap.CalendarInvites, snap.MailInFiles
			if snap.Approvals != nil {
				out.Approvals = snap.Approvals
			}
		}
	}
	d := s.deps.Config.Dashboard
	for i, entry := range d.AuthorizedUsers {
		login, role := dashboardAuthorizedUserEntry(entry)
		if login == "" {
			continue
		}
		if role == "" {
			role = config.RoleRead
			if i == 0 {
				role = config.RoleOwner
			}
		}
		// Everyone with access (read and up) may invite the attendee.
		if !config.RoleAtLeast(role, config.RoleRead) {
			continue
		}
		emails := snap.Emails[login]
		if emails == nil {
			emails = []MeetingsCalendarEmail{}
		}
		out.Inviters = append(out.Inviters, MeetingsAttendeeInviter{
			Login: login, DisplayName: strings.TrimSpace(d.AuthorizedUserNames[login]), Role: role, Emails: emails,
		})
	}
	sort.Slice(out.Inviters, func(i, j int) bool { return out.Inviters[i].Login < out.Inviters[j].Login })
	return out
}

func (s *Server) meetingsPersonaSettings(r *http.Request) MeetingsPersonaSettings {
	tts, email, _ := s.meetingsAttendeeDeps()
	m := s.deps.Config.Meetings
	h := (&s.meetings).Load()
	ttsOK := false
	if tts != nil {
		cfg, err := tts(r.Context())
		ttsOK = err == nil && cfg.URL != ""
	}
	return MeetingsPersonaSettings{
		Effective:       meetingsPersonaView(m),
		Presets:         config.OrderedMeetingsPersonaPresets(),
		Overridden:      m.Persona.Overrides(),
		Voices:          config.KokoroVoices(),
		VoiceTypes:      config.MeetingsVoiceTypeOptions(),
		RestartRequired: !reflect.DeepEqual(meetingsAttendeeBootOf(m), s.meetingsAttendee.boot),
		Running:         h != nil && h.svc != nil,
		TTSConfigured:   ttsOK,
		Email:           s.meetingsAttendeeEmail(r, email),
		Actions:         meetingsActionsSettingsView(s.deps.Config, os.Getenv),
		Research:        s.meetingsResearchView(m),
	}
}

func (s *Server) meetingsPersonaConfigReady(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := requireMeetingsScheduleRole(w, r); !ok {
		return false
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) handleMeetingsPersonaGet(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsPersonaConfigReady(w, r) {
		return
	}
	jsonResponse(w, s.meetingsPersonaSettings(r))
}

// meetingsPersonaUnpromptedRequest sets the unprompted caps; 0 restores a
// default.
type meetingsPersonaUnpromptedRequest struct {
	Per10m        *int     `json:"per_10m,omitempty"`
	MinGapS       *int     `json:"min_gap_s,omitempty"`
	PerMeeting    *int     `json:"per_meeting,omitempty"`
	AnswerWaitS   *int     `json:"answer_wait_s,omitempty"`
	MinConfidence *float64 `json:"min_confidence,omitempty"`
	QueueDepth    *int     `json:"queue_depth,omitempty"`
	StaleS        *int     `json:"stale_s,omitempty"`
}

// meetingsPersonaRequest is PUT /api/meetings/persona. An absent field is
// unchanged; an empty string or list clears the override, so the field
// follows the preset again. A value equal to the preset's is stored as no
// override. clear_overrides drops every field override first (picking a
// preset card).
type meetingsPersonaRequest struct {
	Enabled        *bool                             `json:"enabled,omitempty"`
	Preset         *string                           `json:"preset,omitempty"`
	ClearOverrides bool                              `json:"clear_overrides,omitempty"`
	Name           *string                           `json:"name,omitempty"`
	DisplayName    *string                           `json:"display_name,omitempty"`
	Aliases        *[]string                         `json:"aliases,omitempty"`
	Pronouns       *string                           `json:"pronouns,omitempty"`
	Language       *string                           `json:"language,omitempty"`
	Voice          *string                           `json:"voice,omitempty"`
	VoiceType      *string                           `json:"voice_type,omitempty"`
	Accent         *string                           `json:"accent,omitempty"`
	IntroTemplate  *string                           `json:"intro_template,omitempty"`
	LeavePhrases   *[]string                         `json:"leave_phrases,omitempty"`
	Humor          *string                           `json:"humor,omitempty"`
	Unprompted     *meetingsPersonaUnpromptedRequest `json:"unprompted,omitempty"`
	// VoiceOnAddress sets meetings.voice_on_address.
	VoiceOnAddress *bool `json:"voice_on_address,omitempty"`
	// Email sets meetings.email.invites_enabled and files_enabled (owner
	// only, #18395).
	Email *meetingsEmailRequest `json:"email,omitempty"`
}

// applyMeetingsPersonaRequest returns p with req applied.
func applyMeetingsPersonaRequest(p config.MeetingsPersonaConfig, req meetingsPersonaRequest) config.MeetingsPersonaConfig {
	if req.Enabled != nil {
		v := *req.Enabled
		p.Enabled = &v
	}
	if req.Preset != nil {
		p.Preset = strings.ToLower(strings.TrimSpace(*req.Preset))
	}
	if req.ClearOverrides {
		p = config.MeetingsPersonaConfig{Enabled: p.Enabled, Preset: p.Preset, Unprompted: p.Unprompted, SearxngURL: p.SearxngURL}
	}
	pre, ok := config.MeetingsPersonaPresets[p.Preset]
	if !ok {
		pre = config.MeetingsPersonaPresets[config.DefaultMeetingsPersonaPreset]
	}
	str := func(dst *string, v *string, preset string) {
		if v == nil {
			return
		}
		x := strings.TrimSpace(*v)
		if x == preset {
			x = ""
		}
		*dst = x
	}
	str(&p.Name, req.Name, pre.Name)
	str(&p.DisplayName, req.DisplayName, pre.DisplayName)
	str(&p.Pronouns, req.Pronouns, pre.Pronouns)
	str(&p.Language, req.Language, pre.Language)
	str(&p.Voice, req.Voice, pre.Voice)
	str(&p.VoiceType, req.VoiceType, pre.VoiceType)
	str(&p.Accent, req.Accent, pre.Accent)
	str(&p.Humor, req.Humor, pre.Humor)
	if req.IntroTemplate != nil {
		// The preset intro depends on humor, so compare with both.
		x := strings.TrimSpace(*req.IntroTemplate)
		if x == pre.IntroTemplate || x == pre.PlainIntroTemplate {
			x = ""
		}
		p.IntroTemplate = x
	}
	list := func(dst *[]string, v *[]string, preset []string) {
		if v == nil {
			return
		}
		var out []string
		for _, s := range *v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if strings.EqualFold(strings.Join(out, "\x00"), strings.Join(preset, "\x00")) {
			out = nil
		}
		*dst = out
	}
	list(&p.Aliases, req.Aliases, pre.Aliases)
	list(&p.LeavePhrases, req.LeavePhrases, pre.LeavePhrases)
	if u := req.Unprompted; u != nil {
		setInt := func(dst *int, v *int) {
			if v != nil {
				*dst = *v
			}
		}
		setInt(&p.Unprompted.Per10m, u.Per10m)
		setInt(&p.Unprompted.MinGapS, u.MinGapS)
		setInt(&p.Unprompted.PerMeeting, u.PerMeeting)
		setInt(&p.Unprompted.AnswerWaitS, u.AnswerWaitS)
		setInt(&p.Unprompted.QueueDepth, u.QueueDepth)
		setInt(&p.Unprompted.StaleS, u.StaleS)
		if u.MinConfidence != nil {
			p.Unprompted.MinConfidence = *u.MinConfidence
		}
	}
	return p
}

// handleMeetingsPersonaPut saves meetings.persona to hive.yaml. The
// persona is validated as if enabled, so a disabled persona is still a
// valid one to turn back on.
func (s *Server) handleMeetingsPersonaPut(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsPersonaConfigReady(w, r) {
		return
	}
	var req meetingsPersonaRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	next := applyMeetingsPersonaRequest(s.deps.Config.Meetings.Persona, req)
	check := next
	on := true
	check.Enabled = &on
	if err := check.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Email != nil {
		if !isOwnerRole(r.Header.Get("X-Hive-Role")) {
			jsonError(w, "only the owner changes which email this hive accepts", http.StatusForbidden)
			return
		}
		if v := req.Email.CalendarInvites; v != nil {
			b := *v
			s.deps.Config.Meetings.Email.InvitesEnabled = &b
		}
		if v := req.Email.MailInFiles; v != nil {
			b := *v
			s.deps.Config.Meetings.Email.FilesEnabled = &b
		}
	}
	s.deps.Config.Meetings.Persona = next
	if req.VoiceOnAddress != nil {
		v := *req.VoiceOnAddress
		s.deps.Config.Meetings.VoiceOnAddress = &v
	}
	if err := s.saveConfig(); err != nil {
		jsonError(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res := next.Resolve()
	s.auditFromRequest(r, "meetings_persona", auditDetail("preset", res.Preset, "enabled", boolString(res.Enabled),
		"voice", res.Voice, "language", res.Language), "")
	jsonResponse(w, s.meetingsPersonaSettings(r))
}

// meetingsPersonaSampleRequest is POST /api/meetings/persona/sample (or
// the same fields as GET query parameters): an optional voice (default the persona's), language (picks the sample
// line's language) and text (default the persona's sample line).
type meetingsPersonaSampleRequest struct {
	Voice       string `json:"voice,omitempty"`
	Language    string `json:"language,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Text        string `json:"text,omitempty"`
}

// meetingsTTSSpeechRequest is Kokoro's OpenAI-compatible speech body.
type meetingsTTSSpeechRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
	Stream         bool   `json:"stream"`
}

// handleMeetingsPersonaSample speaks a short line through the TTS service
// and returns the audio for the browser to play.
func (s *Server) handleMeetingsPersonaSample(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsPersonaConfigReady(w, r) {
		return
	}
	var req meetingsPersonaSampleRequest
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req = meetingsPersonaSampleRequest{Voice: q.Get("voice"), Language: q.Get("language"), DisplayName: q.Get("display_name"), Text: q.Get("text")}
	} else if r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	tts, _, client := s.meetingsAttendeeDeps()
	if tts == nil {
		jsonError(w, ErrMeetingsTTSNotConfigured.Error(), http.StatusServiceUnavailable)
		return
	}
	cfg, err := tts(r.Context())
	if err == nil && strings.TrimSpace(cfg.URL) == "" {
		err = ErrMeetingsTTSNotConfigured
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	persona := resolvePersonaFields(s.deps.Config.Meetings.Persona)
	if v := strings.TrimSpace(req.Voice); v != "" {
		persona.Voice = v
	}
	if l := strings.TrimSpace(req.Language); l != "" {
		persona.Language = l
	}
	if d := strings.TrimSpace(req.DisplayName); d != "" {
		persona.DisplayName = d
	}
	text := strings.Join(strings.Fields(req.Text), " ")
	if text == "" {
		text = persona.SampleLine()
	}
	if n := len([]rune(text)); n > meetingsSampleMaxChars {
		jsonError(w, fmt.Sprintf("sample text is %d characters, at most %d", n, meetingsSampleMaxChars), http.StatusBadRequest)
		return
	}
	audio, ctype, status, err := meetingsSpeak(r.Context(), client, cfg, persona.Voice, text)
	if err != nil {
		jsonError(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(audio)
}

// meetingsSpeak calls the TTS service. On failure it returns the HTTP
// status to answer with.
func meetingsSpeak(ctx context.Context, client *http.Client, cfg MeetingsTTSConfig, voice, text string) ([]byte, string, int, error) {
	body, _ := json.Marshal(meetingsTTSSpeechRequest{Model: meetingsTTSModel, Input: text, Voice: voice, ResponseFormat: meetingsTTSFormat})
	ctx, cancel := context.WithTimeout(ctx, meetingsSampleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.URL, "/")+meetingsTTSSpeechPath, bytes.NewReader(body))
	if err != nil {
		return nil, "", http.StatusServiceUnavailable, fmt.Errorf("TTS URL: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		req.Header.Set(meetingsTTSAPIKeyHeader, cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("TTS service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, meetingsTTSErrorBodyMax))
		return nil, "", http.StatusBadGateway, fmt.Errorf("TTS service returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, meetingsSampleMaxBytes+1))
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("reading TTS audio: %w", err)
	}
	if len(audio) > meetingsSampleMaxBytes {
		return nil, "", http.StatusBadGateway, fmt.Errorf("TTS audio is over %d bytes", meetingsSampleMaxBytes)
	}
	if len(audio) == 0 {
		return nil, "", http.StatusBadGateway, errors.New("TTS service returned no audio")
	}
	ctype := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(strings.ToLower(ctype), "audio/") {
		ctype = meetingsTTSDefaultContentType
	}
	return audio, ctype, http.StatusOK, nil
}
