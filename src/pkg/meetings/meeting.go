// Package meetings is hive's side of live meetings through Quorum
// (hivecommons/hive#18284, hivecommons/quorum docs/contract.md,
// quorum.contract/v1): the meeting entity and its store, the MCP client for
// Quorum's tools, the SSE consumer for its live events, and the manager
// that runs each meeting's lifecycle (consent notice on live, `hive leave`,
// lobby timeout).
package meetings

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// State is a meeting's lifecycle state: the contract's states plus the
// hive-side handed_off and purged.
type State string

// Contract states (Quorum `state` events) and hive-side states.
const (
	StateRequested State = "requested"
	StateJoining   State = "joining"
	StateLobby     State = "lobby"
	StateLive      State = "live"
	StateEnded     State = "ended"
	StateFailed    State = "failed"
	StateBlocked   State = "blocked"
	// StateHandedOff: the ended meeting's transcript and notes were handed
	// to the inbox and deleted upstream.
	StateHandedOff State = "handed_off"
	// StatePurged: the retention sweeper removed the transcript.
	StatePurged State = "purged"
)

// knownStates are the states a Quorum state event may carry.
var knownStates = map[State]bool{
	StateRequested: true, StateJoining: true, StateLobby: true, StateLive: true,
	StateEnded: true, StateFailed: true, StateBlocked: true,
}

// Terminal reports whether no further Quorum events are expected.
func (s State) Terminal() bool {
	switch s {
	case StateEnded, StateFailed, StateBlocked, StateHandedOff, StatePurged:
		return true
	}
	return false
}

// Platform names (contract section 1).
const (
	PlatformGoogleMeet = "google_meet"
	PlatformTeams      = "teams"
	PlatformZoom       = "zoom"
	PlatformDiscord    = "discord"
	// PlatformCompanion is the desktop companion: a person's own meeting
	// client audio, captured on their computer and relayed by hive.
	PlatformCompanion = "companion"
)

// Meeting is one meeting hive was sent to.
type Meeting struct {
	ID           string `json:"id"`
	Platform     string `json:"platform,omitempty"`
	URL          string `json:"url,omitempty"`
	GuildID      string `json:"guild_id,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	NativeID     string `json:"native_id,omitempty"`
	Title        string `json:"title,omitempty"`
	InvitedBy    string `json:"invited_by,omitempty"`
	State        State  `json:"state"`
	StateReason  string `json:"state_reason,omitempty"`
	VoiceEnabled bool   `json:"voice_enabled"`
	// VoiceCapable records that the bot was started able to speak. Vexa
	// fixes this at join time: turning voice on later for a bot joined
	// without it downgrades every spoken reply to chat.
	VoiceCapable bool `json:"voice_capable"`
	// RequestedBy is the platform user who asked hive in (Discord
	// `/hive join`); such a meeting waits in requested for approval.
	RequestedBy *Person `json:"requested_by,omitempty"`
	// LastSeq is the highest segment/chat seq stored, the replay cursor.
	LastSeq int64 `json:"last_seq,omitempty"`
	// ConsentError is why the consent notice could not be posted yet
	// (for example platform_unavailable while chat is down).
	ConsentError    string     `json:"consent_error,omitempty"`
	ConsentPostedAt *time.Time `json:"consent_posted_at,omitempty"`
	// ConsentVia is how the notice was delivered: "chat" (Quorum confirmed
	// the line in the meeting chat) or "voice" (spoken because chat could
	// not deliver it).
	ConsentVia string `json:"consent_via,omitempty"`
	// Companion is set on a desktop companion meeting: who attested that
	// the participants were told, when, and which notice they saw.
	Companion *CompanionInfo `json:"companion,omitempty"`
	// PreConsentDropped counts the lines (segments, drafts and chat) hive
	// discarded because they came before the consent notice was delivered.
	// Only the count is kept, never the content.
	PreConsentDropped int `json:"pre_consent_dropped,omitempty"`
	// ConsentSeq is the highest Quorum seq known to come before the notice:
	// a line at or below it is discarded even if it arrives after.
	ConsentSeq int64 `json:"consent_seq,omitempty"`
	// ConsentSpeechEnd is the latest session time (seconds, a segment's
	// `end`) heard before the notice; a segment that starts before it, and
	// is finalized just after the notice, is discarded too.
	ConsentSpeechEnd float64 `json:"consent_speech_end,omitempty"`
	// ConsentFenceDegraded is set when Quorum's transcript cursor could not
	// be read at the notice: every segment in the next ConsentDegradedWindow
	// is discarded, since nothing says which came before the notice.
	ConsentFenceDegraded bool       `json:"consent_fence_degraded,omitempty"`
	LobbySince           *time.Time `json:"lobby_since,omitempty"`
	LiveAt               *time.Time `json:"live_at,omitempty"`
	EndedAt              *time.Time `json:"ended_at,omitempty"`
	HandedOffAt          *time.Time `json:"handed_off_at,omitempty"`
	PurgedAt             *time.Time `json:"purged_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// Person is a meeting participant as Quorum reports it.
type Person struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Event types on Quorum's /events stream.
const (
	EventSegment = "segment"
	EventChat    = "chat"
	EventState   = "state"
)

// Event is one Quorum live event. Raw keeps the exact JSON object so a
// segment can be forwarded to Spektacular unchanged.
type Event struct {
	Type      string          `json:"type"`
	MeetingID string          `json:"meeting_id"`
	Seq       int64           `json:"seq,omitempty"`
	Speaker   *Person         `json:"speaker,omitempty"`
	Author    *Person         `json:"author,omitempty"`
	Text      string          `json:"text,omitempty"`
	Start     json.RawMessage `json:"start,omitempty"`
	End       json.RawMessage `json:"end,omitempty"`
	Final     bool            `json:"final,omitempty"`
	At        json.RawMessage `json:"at,omitempty"`
	State     State           `json:"state,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	// Platform, GuildID, ChannelID and RequestedBy are set on a `requested`
	// state event raised from the platform (Discord `/hive join`).
	Platform    string  `json:"platform,omitempty"`
	GuildID     string  `json:"guild_id,omitempty"`
	ChannelID   string  `json:"channel_id,omitempty"`
	RequestedBy *Person `json:"requested_by,omitempty"`
	// Reply is set on a `reply` event: one of the bot's own lines, written
	// by hive into the transcript (replies.go). Quorum never sends it.
	Reply *ReplyInfo      `json:"reply,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

// Who returns the speaker or author display name.
func (e Event) Who() string {
	if e.Speaker != nil {
		return e.Speaker.Name
	}
	if e.Author != nil {
		return e.Author.Name
	}
	return ""
}

// parseEvent decodes one event object, keeping its raw bytes.
func parseEvent(b []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return Event{}, err
	}
	ev.Raw = append(json.RawMessage(nil), b...)
	return ev, nil
}

// idBytes is the random part of a meeting ID.
const idBytes = 6

func newMeetingID() string {
	b := make([]byte, idBytes)
	_, _ = rand.Read(b)
	return "mtg-" + hex.EncodeToString(b)
}

func timePtr(t time.Time) *time.Time { return &t }
