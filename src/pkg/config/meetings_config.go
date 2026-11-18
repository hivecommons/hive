package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/keystore"
)

// Meetings: hive as a live meeting participant through Quorum
// (hivecommons/hive#18284, hivecommons/quorum docs/contract.md,
// quorum.contract/v1). The whole feature is OFF until meetings.enabled is
// set; with it off nothing is constructed and no Quorum process starts.
//
// Every value below can be overridden by an environment variable, read at
// call time so a hosted hive's operator can turn meetings on from the
// deployment without editing hive.yaml. Precedence is env, then hive.yaml,
// then the default. Secrets (the Vexa tenant key, the optional Discord bot
// token, the transcription service token and the TTS token) are never
// stored in hive.yaml:
// they live in keystore-sealed secret files under /data/secrets, or arrive
// through an environment variable.

const (
	// DefaultMeetingsQuorumBinary is where the hive image installs the pinned
	// Quorum bundle. A path ending in .mjs or .js runs through node.
	DefaultMeetingsQuorumBinary = "/usr/local/lib/quorum/quorum.mjs"
	// DefaultMeetingsQuorumListen is the loopback address the Quorum child
	// listens on (the contract's QUORUM_LISTEN default).
	DefaultMeetingsQuorumListen = "127.0.0.1:8765"
	// DefaultMeetingsDelivery is where meeting output is delivered by
	// default: the meeting chat, so hive never talks over people.
	DefaultMeetingsDelivery = MeetingsDeliveryChat
	// DefaultMeetingsBotName is the display name the meeting bot joins with
	// when the persona is off. It says the bot takes notes. With the persona
	// on (the default), the persona's display_name is the default.
	DefaultMeetingsBotName = "hive (notes)"
	// DefaultMeetingsInvitePolicy lets only the hive owner send hive to a
	// meeting.
	DefaultMeetingsInvitePolicy = MeetingsInvitePolicyOwner
	// DefaultMeetingsResearchPoolSize is the number of research agents
	// (research-1..N) created when meetings are enabled.
	DefaultMeetingsResearchPoolSize = 2
	// DefaultMeetingsResearchPerMeeting caps research items one meeting may
	// start without dashboard approval.
	DefaultMeetingsResearchPerMeeting = 5
	// DefaultMeetingsChatPostsPerMinute caps hive's chat posts into one
	// meeting.
	DefaultMeetingsChatPostsPerMinute = 6
	// DefaultMeetingsChatAnswerBurst is the extra posts a minute addressed
	// answers may take.
	DefaultMeetingsChatAnswerBurst = 3
	// DefaultMeetingsLobbyTimeoutS is how long hive waits in a meeting
	// lobby before it gives up and leaves.
	DefaultMeetingsLobbyTimeoutS = 600
	// DefaultMeetingsRetentionDays is how long hive keeps an ended meeting's
	// transcript before the retention sweeper purges it.
	DefaultMeetingsRetentionDays = 30
	// DefaultMeetingsMaxTokensPerMeeting is the per-meeting LLM token budget
	// for Spektacular live mode, when Spek reports usage.
	DefaultMeetingsMaxTokensPerMeeting = 200000
	// DefaultMeetingsMaxTokensPerHour is the hive-wide hourly ceiling across
	// all meetings.
	DefaultMeetingsMaxTokensPerHour = 500000
	// DefaultMeetingsMaxTokensPerDay is the hive-wide daily ceiling across
	// all meetings.
	DefaultMeetingsMaxTokensPerDay = 2000000
	// DefaultMeetingsMaxIntentsPerMeeting is the per-meeting budget counted
	// in intents when Spek does not report token usage.
	DefaultMeetingsMaxIntentsPerMeeting = 500
	// DefaultMeetingsConsentText is posted in the meeting chat on join,
	// before anything else, with the default persona. Other personas get the
	// same text with their own names (ResolvedMeetingsPersona.ConsentText).
	DefaultMeetingsConsentText = "Betty Bee from the hive has joined to take notes and help with research. Betty records this meeting's transcript. Type 'betty leave' or 'hive leave' to remove Betty."
	// DefaultMeetingsDataDir is where meeting records and transcripts live.
	DefaultMeetingsDataDir = "/data/meetings"
	// DefaultMeetingsVexaAPIKeyFile is the keystore-sealed secret file that
	// holds the Vexa tenant key.
	DefaultMeetingsVexaAPIKeyFile = WritableSecretsDir + "/meetings_vexa_api_key"
	// DefaultMeetingsDiscordTokenFile is the keystore-sealed secret file that
	// holds the optional per-hive Discord bot token.
	DefaultMeetingsDiscordTokenFile = WritableSecretsDir + "/meetings_discord_token"
	// DefaultMeetingsTranscribeTokenFile is the keystore-sealed secret file
	// that holds the bearer token for the shared transcription service
	// Discord audio is posted to (QUORUM_TRANSCRIBE_TOKEN_FILE).
	DefaultMeetingsTranscribeTokenFile = WritableSecretsDir + "/meetings_transcribe_token"
	// DefaultMeetingsTTSTokenFile is the keystore-sealed secret file that
	// holds the bearer token for the text-to-speech service Quorum speaks
	// Discord replies with (QUORUM_TTS_TOKEN_FILE).
	DefaultMeetingsTTSTokenFile = WritableSecretsDir + "/meetings_tts_token"

	// MeetingsDeliveryChat posts meeting output in the meeting chat.
	MeetingsDeliveryChat = "chat"
	// MeetingsInvitePolicyOwner lets only the hive owner invite hive.
	MeetingsInvitePolicyOwner = "owner"

	// meetingsMaxConsentChars mirrors the contract's QUORUM_CHAT_MAX_CHARS
	// default: the consent notice is one chat post.
	meetingsMaxConsentChars = 2000
	// meetingsMaxBotNameChars bounds the display name.
	meetingsMaxBotNameChars = 64
)

// Environment overrides for the meetings block. Each one wins over hive.yaml.
const (
	MeetingsEnabledEnv             = "HIVE_MEETINGS_ENABLED"
	MeetingsQuorumBinaryEnv        = "HIVE_MEETINGS_QUORUM_BINARY"
	MeetingsQuorumListenEnv        = "HIVE_MEETINGS_QUORUM_LISTEN"
	MeetingsVexaURLEnv             = "HIVE_MEETINGS_VEXA_URL"
	MeetingsVexaAPIKeyEnv          = "HIVE_MEETINGS_VEXA_API_KEY"
	MeetingsVexaAPIKeyFileEnv      = "HIVE_MEETINGS_VEXA_API_KEY_FILE"
	MeetingsDiscordTokenEnv        = "HIVE_MEETINGS_DISCORD_TOKEN"
	MeetingsDiscordTokenFileEnv    = "HIVE_MEETINGS_DISCORD_TOKEN_FILE"
	MeetingsTranscribeURLEnv       = "HIVE_MEETINGS_TRANSCRIBE_URL"
	MeetingsTranscribeTokenEnv     = "HIVE_MEETINGS_TRANSCRIBE_TOKEN"
	MeetingsTranscribeTokenFileEnv = "HIVE_MEETINGS_TRANSCRIBE_TOKEN_FILE"
	MeetingsTTSURLEnv              = "HIVE_MEETINGS_TTS_URL"
	MeetingsTTSTokenEnv            = "HIVE_MEETINGS_TTS_TOKEN"
	MeetingsTTSTokenFileEnv        = "HIVE_MEETINGS_TTS_TOKEN_FILE"
	MeetingsDeliveryEnv            = "HIVE_MEETINGS_DELIVERY"
	MeetingsVoiceOnAddressEnv      = "HIVE_MEETINGS_VOICE_ON_ADDRESS"
	MeetingsListeningToneEnv       = "HIVE_MEETINGS_LISTENING_TONE"
	MeetingsBotNameEnv             = "HIVE_MEETINGS_BOT_NAME"
	MeetingsInvitePolicyEnv        = "HIVE_MEETINGS_INVITE_POLICY"
	MeetingsResearchPoolSizeEnv    = "HIVE_MEETINGS_RESEARCH_POOL_SIZE"
	MeetingsResearchPerMeetingEnv  = "HIVE_MEETINGS_RESEARCH_PER_MEETING"
	MeetingsChatPostsPerMinuteEnv  = "HIVE_MEETINGS_CHAT_POSTS_PER_MINUTE"
	MeetingsChatAnswerBurstEnv     = "HIVE_MEETINGS_CHAT_ANSWER_BURST"
	MeetingsLobbyTimeoutEnv        = "HIVE_MEETINGS_LOBBY_TIMEOUT_S"
	MeetingsRetentionDaysEnv       = "HIVE_MEETINGS_RETENTION_DAYS"
	MeetingsConsentTextEnv         = "HIVE_MEETINGS_CONSENT_TEXT"
	MeetingsDataDirEnv             = "HIVE_MEETINGS_DATA_DIR"
	MeetingsTokensPerMeetingEnv    = "HIVE_MEETINGS_MAX_TOKENS_PER_MEETING"
	MeetingsTokensPerHourEnv       = "HIVE_MEETINGS_MAX_TOKENS_PER_HOUR"
	MeetingsTokensPerDayEnv        = "HIVE_MEETINGS_MAX_TOKENS_PER_DAY"
	MeetingsIntentsPerMeetingEnv   = "HIVE_MEETINGS_MAX_INTENTS_PER_MEETING"
	MeetingsEmailInvitesEnv        = "HIVE_MEETINGS_EMAIL_INVITES_ENABLED"
	MeetingsEmailFilesEnv          = "HIVE_MEETINGS_EMAIL_FILES_ENABLED"
)

// MeetingsConfig is the `meetings:` block. The zero value is the feature off.
type MeetingsConfig struct {
	// Enabled turns meetings on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Quorum configures the Quorum child process.
	Quorum MeetingsQuorumConfig `yaml:"quorum,omitempty" json:"quorum,omitempty"`
	// Vexa configures the Vexa meeting-bot gateway Quorum talks to.
	Vexa MeetingsVexaConfig `yaml:"vexa,omitempty" json:"vexa,omitempty"`
	// Discord configures the optional Discord voice adapter.
	Discord MeetingsDiscordConfig `yaml:"discord,omitempty" json:"discord,omitempty"`
	// TranscribeURL is the shared transcription service Quorum sends
	// Discord audio to (QUORUM_TRANSCRIBE_URL). Empty leaves it unset.
	TranscribeURL string `yaml:"transcribe_url,omitempty" json:"transcribe_url,omitempty"`
	// TranscribeTokenFile is the keystore-sealed secret file holding the
	// transcription service token (QUORUM_TRANSCRIBE_TOKEN_FILE). A path,
	// never the token.
	TranscribeTokenFile string `yaml:"transcribe_token_file,omitempty" json:"transcribe_token_file,omitempty"`
	// TTSURL is the text-to-speech service Quorum speaks Discord replies
	// with (QUORUM_TTS_URL, a Kokoro /v1/audio/speech endpoint). Empty
	// leaves Discord without speech.
	TTSURL string `yaml:"tts_url,omitempty" json:"tts_url,omitempty"`
	// TTSTokenFile is the keystore-sealed secret file holding the TTS
	// service token (QUORUM_TTS_TOKEN_FILE). A path, never the token.
	TTSTokenFile string `yaml:"tts_token_file,omitempty" json:"tts_token_file,omitempty"`
	// Delivery is where meeting output goes by default. Only "chat".
	Delivery string `yaml:"delivery,omitempty" json:"delivery,omitempty"`
	// VoiceOnAddress lets hive speak when the meeting addresses it by name
	// and the meeting has voice enabled. Nil defaults true.
	VoiceOnAddress *bool `yaml:"voice_on_address,omitempty" json:"voice_on_address,omitempty"`
	// ListeningTone plays a soft two-note chime when Betty was addressed
	// by voice and is about to answer. Nil defaults true; the only setting,
	// there are no volume or sound options.
	ListeningTone *bool `yaml:"listening_tone,omitempty" json:"listening_tone,omitempty"`
	// BotName is the display name the bot joins with.
	BotName string `yaml:"bot_name,omitempty" json:"bot_name,omitempty"`
	// InvitePolicy is who may send hive to a meeting. Only "owner".
	InvitePolicy string `yaml:"invite_policy,omitempty" json:"invite_policy,omitempty"`
	// ResearchPoolSize is the number of research agents. Zero or negative
	// means DefaultMeetingsResearchPoolSize.
	ResearchPoolSize int `yaml:"research_pool_size,omitempty" json:"research_pool_size,omitempty"`
	// ResearchPerMeeting caps research items per meeting before dashboard
	// approval is required. Zero or negative means the default.
	ResearchPerMeeting int `yaml:"research_per_meeting,omitempty" json:"research_per_meeting,omitempty"`
	// ChatPostsPerMinute caps hive's chat posts into one meeting. Zero or
	// negative means the default.
	ChatPostsPerMinute int `yaml:"chat_posts_per_minute,omitempty" json:"chat_posts_per_minute,omitempty"`
	// ChatAnswerBurst is how many extra chat posts a minute the persona's
	// answers to requests addressed to her may take beyond
	// ChatPostsPerMinute, so parallel answers are not held for a minute
	// (#18590). Zero or negative means the default.
	ChatAnswerBurst int `yaml:"chat_answer_burst,omitempty" json:"chat_answer_burst,omitempty"`
	// LobbyTimeoutS is how long hive waits in a lobby. Zero or negative
	// means the default.
	LobbyTimeoutS int `yaml:"lobby_timeout_s,omitempty" json:"lobby_timeout_s,omitempty"`
	// Budget bounds Spektacular live-mode LLM use.
	Budget MeetingsBudgetConfig `yaml:"budget,omitempty" json:"budget,omitempty"`
	// LLM is the model Spektacular live mode reasons with. Hive owns the
	// credential and the budget; Spek never sees an OpenAI-compatible key.
	LLM MeetingsLLMConfig `yaml:"llm,omitempty" json:"llm,omitempty"`
	// RetentionDays is how long an ended meeting's transcript is kept. Zero
	// or negative means the default.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
	// ConsentText is the notice posted in the meeting chat on join.
	ConsentText string `yaml:"consent_text,omitempty" json:"consent_text,omitempty"`
	// DataDir is where meeting records and transcripts are stored.
	DataDir string `yaml:"data_dir,omitempty" json:"data_dir,omitempty"`
	// Persona is the attendee persona (Betty by default).
	Persona MeetingsPersonaConfig `yaml:"persona,omitempty" json:"persona,omitzero"`
	// Email is mail that reaches this hive through hive-mx
	// (hive@<dashboard host>, #18395): calendar invites and mail-in files.
	Email MeetingsEmailConfig `yaml:"email,omitempty" json:"email,omitzero"`
}

// MeetingsEmailConfig is the `meetings.email:` block. Both switches default
// on: nothing arrives unless hive-mx and the hub route mail to this hive,
// and mail from senders outside the allowlist always waits for approval.
type MeetingsEmailConfig struct {
	// InvitesEnabled accepts calendar invites sent to hive@<host>
	// (HIVE_MEETINGS_EMAIL_INVITES_ENABLED).
	InvitesEnabled *bool `yaml:"invites_enabled,omitempty" json:"invites_enabled,omitempty"`
	// FilesEnabled accepts Markdown, audio, video, image and office
	// document attachments as Inbox items (HIVE_MEETINGS_EMAIL_FILES_ENABLED).
	FilesEnabled *bool `yaml:"files_enabled,omitempty" json:"files_enabled,omitempty"`
}

// InvitesOn reports whether email invites are accepted (env first, default on).
func (e MeetingsEmailConfig) InvitesOn() bool {
	if v, ok := parseBoolEnv(MeetingsEmailInvitesEnv); ok {
		return v
	}
	return e.InvitesEnabled == nil || *e.InvitesEnabled
}

// FilesOn reports whether mail-in files are accepted (env first, default on).
func (e MeetingsEmailConfig) FilesOn() bool {
	if v, ok := parseBoolEnv(MeetingsEmailFilesEnv); ok {
		return v
	}
	return e.FilesEnabled == nil || *e.FilesEnabled
}

// MeetingsQuorumConfig is meetings.quorum.
type MeetingsQuorumConfig struct {
	// Binary is the Quorum bundle (run through node when it ends in .mjs or
	// .js) or an executable.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
	// Listen is the loopback host:port Quorum listens on.
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
}

// MeetingsVexaConfig is meetings.vexa.
type MeetingsVexaConfig struct {
	// URL is the Vexa gateway base URL (QUORUM_VEXA_URL).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// APIKeyFile is the keystore-sealed secret file holding the tenant key.
	// A path, never the key.
	APIKeyFile string `yaml:"api_key_file,omitempty" json:"api_key_file,omitempty"`
}

// MeetingsDiscordConfig is meetings.discord.
type MeetingsDiscordConfig struct {
	// TokenFile is the keystore-sealed secret file holding the bot token.
	// A path, never the token.
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
}

// MeetingsLLMConfig is meetings.llm. With nothing set it resolves like the
// knowledge advisor: governor.litellm's endpoint, key and default model.
type MeetingsLLMConfig struct {
	// Mode is auto (default), openai, anthropic, agent-backend, claude-cli
	// (agent-backend with backend claude) or none.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Backend is the agent backend the agent-backend mode answers through
	// (hivecommons/hive#18517): one the hive's agents are configured with.
	// Empty uses the backend most of the hive's agents run on.
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// Endpoint is an OpenAI-compatible base URL; empty uses governor.litellm.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Model is the model name; empty uses governor.litellm.default_model.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// APIKeyFile is the keystore-sealed key for Endpoint; empty, or a
	// missing file, uses governor.litellm's key.
	APIKeyFile string `yaml:"api_key_file,omitempty" json:"api_key_file,omitempty"`
	// AnthropicKeyFile is a keystore-sealed Anthropic key; when set, Spek
	// calls Anthropic directly (mode auto or anthropic).
	AnthropicKeyFile string `yaml:"anthropic_key_file,omitempty" json:"anthropic_key_file,omitempty"`
	// AnthropicModel overrides Spek's default Anthropic model.
	AnthropicModel string `yaml:"anthropic_model,omitempty" json:"anthropic_model,omitempty"`
	// TimeoutMS bounds one model call. Zero means DefaultMeetingsLLMTimeoutMS.
	TimeoutMS int `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	// MaxInFlight caps concurrent model calls across meetings. Zero means
	// DefaultMeetingsLLMMaxInFlight.
	MaxInFlight int `yaml:"max_in_flight,omitempty" json:"max_in_flight,omitempty"`
	// PerMeetingInFlight caps one meeting's concurrent model calls, so the
	// persona answers several requests at once (#18590); it never exceeds
	// MaxInFlight. Zero means DefaultMeetingsLLMPerMeetingInFlight.
	PerMeetingInFlight int `yaml:"per_meeting_in_flight,omitempty" json:"per_meeting_in_flight,omitempty"`
}

// Meetings LLM modes and defaults.
const (
	MeetingsLLMModeAuto      = "auto"
	MeetingsLLMModeOpenAI    = "openai"
	MeetingsLLMModeAnthropic = "anthropic"
	MeetingsLLMModeNone      = "none"
	// MeetingsLLMModeAgentBackend answers through one of the hive's agent
	// backends, in its no-tools completion profile (#18517).
	MeetingsLLMModeAgentBackend = "agent-backend"
	// MeetingsLLMModeClaudeCLI is agent-backend with backend claude.
	MeetingsLLMModeClaudeCLI = "claude-cli"
	// DefaultMeetingsLLMTimeoutMS matches Spek's own per-call cap.
	DefaultMeetingsLLMTimeoutMS = 2500
	// DefaultMeetingsLLMMaxInFlight lets a few meetings reason at once.
	DefaultMeetingsLLMMaxInFlight = 4
	// DefaultMeetingsLLMPerMeetingInFlight lets one meeting's persona work
	// on a few requests at once.
	DefaultMeetingsLLMPerMeetingInFlight = 3
	// DefaultMeetingsLLMKeyFile and DefaultMeetingsAnthropicKeyFile are the
	// keystore-sealed secret files.
	DefaultMeetingsLLMKeyFile       = WritableSecretsDir + "/meetings_llm_api_key"
	DefaultMeetingsAnthropicKeyFile = WritableSecretsDir + "/meetings_anthropic_api_key"
	// DefaultMeetingsPersonaAnthropicModel is the model the meeting persona
	// answers with on the Anthropic path when meetings.llm.model is unset:
	// a fast model, since answers must arrive within seconds.
	DefaultMeetingsPersonaAnthropicModel = "claude-haiku-5-5"
	// MeetingsLLMNoModelReason is what the panel shows without a model.
	MeetingsLLMNoModelReason = "rules only, no model configured"
)

// Meetings LLM env overrides.
const (
	MeetingsLLMModeEnv          = "HIVE_MEETINGS_LLM_MODE"
	MeetingsLLMEndpointEnv      = "HIVE_MEETINGS_LLM_ENDPOINT"
	MeetingsLLMModelEnv         = "HIVE_MEETINGS_LLM_MODEL"
	MeetingsLLMAPIKeyEnv        = "HIVE_MEETINGS_LLM_API_KEY"
	MeetingsLLMAPIKeyFileEnv    = "HIVE_MEETINGS_LLM_API_KEY_FILE"
	MeetingsAnthropicKeyEnv     = "HIVE_MEETINGS_ANTHROPIC_API_KEY"
	MeetingsAnthropicKeyFileEnv = "HIVE_MEETINGS_ANTHROPIC_API_KEY_FILE"
	MeetingsAnthropicModelEnv   = "HIVE_MEETINGS_ANTHROPIC_MODEL"
	MeetingsLLMTimeoutEnv       = "HIVE_MEETINGS_LLM_TIMEOUT_MS"
	MeetingsLLMMaxInFlightEnv   = "HIVE_MEETINGS_LLM_MAX_IN_FLIGHT"
	// MeetingsLLMPerMeetingInFlightEnv overrides per_meeting_in_flight.
	MeetingsLLMPerMeetingInFlightEnv = "HIVE_MEETINGS_LLM_PER_MEETING_IN_FLIGHT"
	MeetingsLLMBackendEnv            = "HIVE_MEETINGS_LLM_BACKEND"
)

// ResolvedMeetingsLLM is the effective meetings model. Keys are secrets;
// never log them.
type ResolvedMeetingsLLM struct {
	// Backend is openai (hive's meeting-llm command), anthropic or none.
	Backend  string
	Endpoint string
	// Model is the OpenAI-compatible model; on the anthropic backend it is
	// the model the meeting persona answers with (meetings.llm.model, else
	// DefaultMeetingsPersonaAnthropicModel). Spek uses AnthropicModel.
	Model          string
	APIKey         string
	AnthropicKey   string
	AnthropicModel string
	TimeoutMS      int
	MaxInFlight    int
	// PerMeetingInFlight is one meeting's concurrent model calls, at most
	// MaxInFlight.
	PerMeetingInFlight int
	// Reason explains Backend none.
	Reason string
	// AgentBackend is the agent backend (claude, ...) on the agent-backend
	// backend; Model is then that backend's model. On the openai backend it
	// names the model gateway (vllm, llm-d or a named gateway) the
	// agent-backend mode resolved, and is empty otherwise.
	AgentBackend string
	// CABundle is an optional PEM file of private CAs the openai backend's
	// endpoint is verified against (a named gateway's ca_bundle).
	CABundle string
	// CLI is the completion-profile launch settings on the agent-backend
	// backend.
	CLI ResolvedMeetingsCLI
}

// Label is the panel's one-line description of the model in use.
func (r ResolvedMeetingsLLM) Label() string {
	switch r.Backend {
	case MeetingsLLMModeOpenAI:
		if r.AgentBackend != "" {
			return r.AgentBackend + " model " + r.Model
		}
		return "model " + r.Model
	case MeetingsLLMModeAnthropic:
		if r.AnthropicModel != "" {
			return "Anthropic " + r.AnthropicModel
		}
		return "Anthropic (Spektacular default model)"
	case MeetingsLLMModeAgentBackend:
		return r.AgentBackend + " CLI " + r.Model
	default:
		return r.Reason
	}
}

// ResolveMeetingsLLM resolves meetings.llm with env overrides and the
// governor.litellm fallbacks.
func (c *Config) ResolveMeetingsLLM(ctx context.Context) ResolvedMeetingsLLM {
	none := ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone, Reason: MeetingsLLMNoModelReason}
	if c == nil {
		return none
	}
	l := c.Meetings.LLM
	mode := strings.ToLower(meetingsEnvString(MeetingsLLMModeEnv, l.Mode, MeetingsLLMModeAuto))
	if mode == MeetingsLLMModeNone {
		none.Reason = "rules only, model turned off (meetings.llm.mode: none)"
		return none
	}
	r := ResolvedMeetingsLLM{
		TimeoutMS:      meetingsEnvInt(MeetingsLLMTimeoutEnv, l.TimeoutMS, DefaultMeetingsLLMTimeoutMS),
		MaxInFlight:    meetingsEnvInt(MeetingsLLMMaxInFlightEnv, l.MaxInFlight, DefaultMeetingsLLMMaxInFlight),
		AnthropicModel: meetingsEnvString(MeetingsAnthropicModelEnv, l.AnthropicModel, ""),
	}
	r.PerMeetingInFlight = min(r.MaxInFlight,
		meetingsEnvInt(MeetingsLLMPerMeetingInFlightEnv, l.PerMeetingInFlight, DefaultMeetingsLLMPerMeetingInFlight))
	if mode == MeetingsLLMModeAgentBackend || mode == MeetingsLLMModeClaudeCLI {
		return c.resolveMeetingsAgentBackend(ctx, mode, r)
	}
	if mode == MeetingsLLMModeAuto || mode == MeetingsLLMModeAnthropic {
		file := meetingsEnvString(MeetingsAnthropicKeyFileEnv, l.AnthropicKeyFile, DefaultMeetingsAnthropicKeyFile)
		if key, err := meetingsSecret(ctx, MeetingsAnthropicKeyEnv, file); err == nil && key != "" {
			r.Backend, r.AnthropicKey = MeetingsLLMModeAnthropic, key
			r.Model = meetingsEnvString(MeetingsLLMModelEnv, l.Model, DefaultMeetingsPersonaAnthropicModel)
			return r
		}
		if mode == MeetingsLLMModeAnthropic {
			none.Reason = "rules only, no Anthropic key configured"
			return none
		}
	}
	return c.resolveMeetingsOpenAI(ctx, r, strings.TrimSpace(c.Governor.LiteLLM.DefaultModel))
}

// resolveMeetingsOpenAI resolves the OpenAI-compatible path: meetings.llm's
// endpoint and key, else governor.litellm's, and the model, else
// defaultModel. It returns Backend none when either is missing.
func (c *Config) resolveMeetingsOpenAI(ctx context.Context, r ResolvedMeetingsLLM, defaultModel string) ResolvedMeetingsLLM {
	l := c.Meetings.LLM
	r.Endpoint = strings.TrimRight(meetingsEnvString(MeetingsLLMEndpointEnv, l.Endpoint, ""), "/")
	if r.Endpoint == "" {
		r.Endpoint = strings.TrimRight(c.Governor.LiteLLM.ResolveEndpoint(), "/")
	}
	file := meetingsEnvString(MeetingsLLMAPIKeyFileEnv, l.APIKeyFile, DefaultMeetingsLLMKeyFile)
	if key, err := meetingsSecret(ctx, MeetingsLLMAPIKeyEnv, file); err == nil {
		r.APIKey = key
	}
	if r.APIKey == "" {
		r.APIKey = c.Governor.LiteLLM.ResolveAPIKey()
	}
	r.Model = meetingsEnvString(MeetingsLLMModelEnv, l.Model, defaultModel)
	if r.Endpoint == "" || r.Model == "" {
		return ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone, Reason: MeetingsLLMNoModelReason}
	}
	r.Backend = MeetingsLLMModeOpenAI
	return r
}

// MeetingsBudgetConfig is meetings.budget. Zero or negative means the
// default for each field.
type MeetingsBudgetConfig struct {
	MaxTokensPerMeeting  int `yaml:"max_tokens_per_meeting,omitempty" json:"max_tokens_per_meeting,omitempty"`
	MaxTokensPerHour     int `yaml:"max_tokens_per_hour,omitempty" json:"max_tokens_per_hour,omitempty"`
	MaxTokensPerDay      int `yaml:"max_tokens_per_day,omitempty" json:"max_tokens_per_day,omitempty"`
	MaxIntentsPerMeeting int `yaml:"max_intents_per_meeting,omitempty" json:"max_intents_per_meeting,omitempty"`
}

// meetingsEnvString returns the trimmed env value when set, else the
// trimmed config value, else def.
func meetingsEnvString(env, value, def string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return def
}

// meetingsEnvInt returns a positive env value when set and valid, else a
// positive config value, else def.
func meetingsEnvInt(env string, value, def int) int {
	if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	if value > 0 {
		return value
	}
	return def
}

// IsEnabled reports whether meetings are on: HIVE_MEETINGS_ENABLED, then
// meetings.enabled.
func (m MeetingsConfig) IsEnabled() bool {
	if v, ok := parseBoolEnv(MeetingsEnabledEnv); ok {
		return v
	}
	return m.Enabled
}

// MeetingsEnabledEnvSet reports whether HIVE_MEETINGS_ENABLED overrides
// meetings.enabled, so the dashboard can say a toggle will not take effect.
func MeetingsEnabledEnvSet() bool {
	_, ok := parseBoolEnv(MeetingsEnabledEnv)
	return ok
}

// QuorumBinary returns the Quorum bundle or executable path.
func (m MeetingsConfig) QuorumBinary() string {
	return meetingsEnvString(MeetingsQuorumBinaryEnv, m.Quorum.Binary, DefaultMeetingsQuorumBinary)
}

// QuorumListen returns the Quorum listen address.
func (m MeetingsConfig) QuorumListen() string {
	return meetingsEnvString(MeetingsQuorumListenEnv, m.Quorum.Listen, DefaultMeetingsQuorumListen)
}

// VexaURL returns the Vexa gateway base URL without a trailing slash.
func (m MeetingsConfig) VexaURL() string {
	return strings.TrimRight(meetingsEnvString(MeetingsVexaURLEnv, m.Vexa.URL, ""), "/")
}

// VexaAPIKeyFile returns the secret file path for the Vexa tenant key.
func (m MeetingsConfig) VexaAPIKeyFile() string {
	return meetingsEnvString(MeetingsVexaAPIKeyFileEnv, m.Vexa.APIKeyFile, DefaultMeetingsVexaAPIKeyFile)
}

// DiscordTokenFile returns the secret file path for the Discord bot token.
func (m MeetingsConfig) DiscordTokenFile() string {
	return meetingsEnvString(MeetingsDiscordTokenFileEnv, m.Discord.TokenFile, DefaultMeetingsDiscordTokenFile)
}

// TranscribeTokenFileOrDefault returns the secret file path for the
// transcription service token.
func (m MeetingsConfig) TranscribeTokenFileOrDefault() string {
	return meetingsEnvString(MeetingsTranscribeTokenFileEnv, m.TranscribeTokenFile, DefaultMeetingsTranscribeTokenFile)
}

// TTSTokenFileOrDefault returns the secret file path for the TTS service
// token.
func (m MeetingsConfig) TTSTokenFileOrDefault() string {
	return meetingsEnvString(MeetingsTTSTokenFileEnv, m.TTSTokenFile, DefaultMeetingsTTSTokenFile)
}

// TTSURLOrEmpty returns the text-to-speech service URL.
func (m MeetingsConfig) TTSURLOrEmpty() string {
	return strings.TrimRight(meetingsEnvString(MeetingsTTSURLEnv, m.TTSURL, ""), "/")
}

// TranscribeURLOrEmpty returns the transcription service URL.
func (m MeetingsConfig) TranscribeURLOrEmpty() string {
	return strings.TrimRight(meetingsEnvString(MeetingsTranscribeURLEnv, m.TranscribeURL, ""), "/")
}

// DeliveryOrDefault returns the default delivery channel.
func (m MeetingsConfig) DeliveryOrDefault() string {
	return strings.ToLower(meetingsEnvString(MeetingsDeliveryEnv, m.Delivery, DefaultMeetingsDelivery))
}

// VoiceOnAddressEnabled reports whether hive may speak when addressed by
// name in a meeting that has voice enabled. Default true.
func (m MeetingsConfig) VoiceOnAddressEnabled() bool {
	if v, ok := parseBoolEnv(MeetingsVoiceOnAddressEnv); ok {
		return v
	}
	return m.VoiceOnAddress == nil || *m.VoiceOnAddress
}

// ListeningToneEnabled reports whether Betty plays the listening tone when
// she was addressed and will answer aloud. Default true.
func (m MeetingsConfig) ListeningToneEnabled() bool {
	if v, ok := parseBoolEnv(MeetingsListeningToneEnv); ok {
		return v
	}
	return m.ListeningTone == nil || *m.ListeningTone
}

// BotNameOrDefault returns the bot display name: meetings.bot_name (env
// first), else the persona's display name, else DefaultMeetingsBotName when
// the persona is off.
func (m MeetingsConfig) BotNameOrDefault() string {
	def := DefaultMeetingsBotName
	if p := m.Persona.Resolve(); p.Enabled {
		def = p.DisplayName
	}
	return meetingsEnvString(MeetingsBotNameEnv, m.BotName, def)
}

// InvitePolicyOrDefault returns who may invite hive to a meeting.
func (m MeetingsConfig) InvitePolicyOrDefault() string {
	return strings.ToLower(meetingsEnvString(MeetingsInvitePolicyEnv, m.InvitePolicy, DefaultMeetingsInvitePolicy))
}

// ResearchPoolSizeOrDefault returns the research agent pool size.
func (m MeetingsConfig) ResearchPoolSizeOrDefault() int {
	return meetingsEnvInt(MeetingsResearchPoolSizeEnv, m.ResearchPoolSize, DefaultMeetingsResearchPoolSize)
}

// ResearchPerMeetingOrDefault returns the per-meeting research cap.
func (m MeetingsConfig) ResearchPerMeetingOrDefault() int {
	return meetingsEnvInt(MeetingsResearchPerMeetingEnv, m.ResearchPerMeeting, DefaultMeetingsResearchPerMeeting)
}

// ChatPostsPerMinuteOrDefault returns the per-meeting chat post rate.
func (m MeetingsConfig) ChatPostsPerMinuteOrDefault() int {
	return meetingsEnvInt(MeetingsChatPostsPerMinuteEnv, m.ChatPostsPerMinute, DefaultMeetingsChatPostsPerMinute)
}

// ChatAnswerBurstOrDefault returns the extra chat posts a minute addressed
// persona answers may take.
func (m MeetingsConfig) ChatAnswerBurstOrDefault() int {
	return meetingsEnvInt(MeetingsChatAnswerBurstEnv, m.ChatAnswerBurst, DefaultMeetingsChatAnswerBurst)
}

// LobbyTimeoutSOrDefault returns the lobby timeout in seconds.
func (m MeetingsConfig) LobbyTimeoutSOrDefault() int {
	return meetingsEnvInt(MeetingsLobbyTimeoutEnv, m.LobbyTimeoutS, DefaultMeetingsLobbyTimeoutS)
}

// RetentionDaysOrDefault returns the transcript retention in days.
func (m MeetingsConfig) RetentionDaysOrDefault() int {
	return meetingsEnvInt(MeetingsRetentionDaysEnv, m.RetentionDays, DefaultMeetingsRetentionDays)
}

// ConsentTextOrDefault returns the consent notice: meetings.consent_text
// (env first), else the persona's notice.
func (m MeetingsConfig) ConsentTextOrDefault() string {
	return meetingsEnvString(MeetingsConsentTextEnv, m.ConsentText, m.Persona.Resolve().ConsentText())
}

// DataDirOrDefault returns the meetings data directory.
func (m MeetingsConfig) DataDirOrDefault() string {
	return meetingsEnvString(MeetingsDataDirEnv, m.DataDir, DefaultMeetingsDataDir)
}

// MaxTokensPerMeetingOrDefault returns the per-meeting token budget.
func (b MeetingsBudgetConfig) MaxTokensPerMeetingOrDefault() int {
	return meetingsEnvInt(MeetingsTokensPerMeetingEnv, b.MaxTokensPerMeeting, DefaultMeetingsMaxTokensPerMeeting)
}

// MaxTokensPerHourOrDefault returns the hive-wide hourly token ceiling.
func (b MeetingsBudgetConfig) MaxTokensPerHourOrDefault() int {
	return meetingsEnvInt(MeetingsTokensPerHourEnv, b.MaxTokensPerHour, DefaultMeetingsMaxTokensPerHour)
}

// MaxTokensPerDayOrDefault returns the hive-wide daily token ceiling.
func (b MeetingsBudgetConfig) MaxTokensPerDayOrDefault() int {
	return meetingsEnvInt(MeetingsTokensPerDayEnv, b.MaxTokensPerDay, DefaultMeetingsMaxTokensPerDay)
}

// MaxIntentsPerMeetingOrDefault returns the per-meeting intent budget used
// when Spek reports no token usage.
func (b MeetingsBudgetConfig) MaxIntentsPerMeetingOrDefault() int {
	return meetingsEnvInt(MeetingsIntentsPerMeetingEnv, b.MaxIntentsPerMeeting, DefaultMeetingsMaxIntentsPerMeeting)
}

// meetingsSecret resolves a secret: the env value when set, else the
// keystore-sealed file at path. A missing file is not an error; it returns
// "".
func meetingsSecret(ctx context.Context, env, path string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	v, err := keystore.ReadString(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("meetings: reading secret file %s: %w", path, err)
	}
	return strings.TrimSpace(v), nil
}

// VexaAPIKey returns the Vexa tenant key: HIVE_MEETINGS_VEXA_API_KEY, then
// the secret file. A secret; never log it.
func (m MeetingsConfig) VexaAPIKey(ctx context.Context) (string, error) {
	return meetingsSecret(ctx, MeetingsVexaAPIKeyEnv, m.VexaAPIKeyFile())
}

// DiscordToken returns the optional Discord bot token:
// HIVE_MEETINGS_DISCORD_TOKEN, then the secret file. A secret; never log it.
func (m MeetingsConfig) DiscordToken(ctx context.Context) (string, error) {
	return meetingsSecret(ctx, MeetingsDiscordTokenEnv, m.DiscordTokenFile())
}

// TranscribeToken returns the optional transcription service token:
// HIVE_MEETINGS_TRANSCRIBE_TOKEN, then the secret file. A secret; never log
// it.
func (m MeetingsConfig) TranscribeToken(ctx context.Context) (string, error) {
	return meetingsSecret(ctx, MeetingsTranscribeTokenEnv, m.TranscribeTokenFileOrDefault())
}

// TTSToken returns the optional TTS service token: HIVE_MEETINGS_TTS_TOKEN,
// then the secret file. A secret; never log it.
func (m MeetingsConfig) TTSToken(ctx context.Context) (string, error) {
	return meetingsSecret(ctx, MeetingsTTSTokenEnv, m.TTSTokenFileOrDefault())
}

// WriteMeetingsSecret seals value into the secret file at path through the
// keystore (owner-only mode). An empty value removes the file.
func WriteMeetingsSecret(ctx context.Context, path, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("meetings: removing secret file %s: %w", path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("meetings: creating secret dir: %w", err)
	}
	return keystore.WriteFile(ctx, path, []byte(value))
}

// Validate checks the effective meetings settings (env included). A
// disabled block is never rejected, so a stray env var cannot stop a hive
// that does not use meetings from booting.
func (m MeetingsConfig) Validate() error {
	if !m.IsEnabled() {
		return nil
	}
	if err := validateMeetingsListen(m.QuorumListen()); err != nil {
		return err
	}
	if u := m.VexaURL(); u != "" {
		if err := validateMeetingsURL("meetings.vexa.url", u); err != nil {
			return err
		}
	}
	if u := m.TranscribeURLOrEmpty(); u != "" {
		if err := validateMeetingsURL("meetings.transcribe_url", u); err != nil {
			return err
		}
	}
	if u := m.TTSURLOrEmpty(); u != "" {
		if err := validateMeetingsURL("meetings.tts_url", u); err != nil {
			return err
		}
	}
	switch mode := strings.ToLower(meetingsEnvString(MeetingsLLMModeEnv, m.LLM.Mode, MeetingsLLMModeAuto)); mode {
	case MeetingsLLMModeAuto, MeetingsLLMModeOpenAI, MeetingsLLMModeAnthropic, MeetingsLLMModeNone:
	case MeetingsLLMModeAgentBackend, MeetingsLLMModeClaudeCLI:
		if err := validateMeetingsAgentBackend(mode, m.LLM.Backend); err != nil {
			return err
		}
	default:
		return fmt.Errorf("meetings.llm.mode %q is not auto, openai, anthropic, agent-backend, claude-cli or none", m.LLM.Mode)
	}
	if u := meetingsEnvString(MeetingsLLMEndpointEnv, m.LLM.Endpoint, ""); u != "" {
		if err := validateMeetingsURL("meetings.llm.endpoint", u); err != nil {
			return err
		}
	}
	if d := m.DeliveryOrDefault(); d != MeetingsDeliveryChat {
		return fmt.Errorf("meetings.delivery %q is not supported (only %q)", d, MeetingsDeliveryChat)
	}
	if p := m.InvitePolicyOrDefault(); p != MeetingsInvitePolicyOwner {
		return fmt.Errorf("meetings.invite_policy %q is not supported (only %q)", p, MeetingsInvitePolicyOwner)
	}
	if n := len([]rune(m.BotNameOrDefault())); n > meetingsMaxBotNameChars {
		return fmt.Errorf("meetings.bot_name is %d characters, the limit is %d", n, meetingsMaxBotNameChars)
	}
	if n := len([]rune(m.ConsentTextOrDefault())); n > meetingsMaxConsentChars {
		return fmt.Errorf("meetings.consent_text is %d characters, the limit is %d (one chat post)", n, meetingsMaxConsentChars)
	}
	if err := m.Persona.Validate(); err != nil {
		return err
	}
	for _, f := range []struct {
		key string
		v   int
	}{
		{"meetings.research_pool_size", m.ResearchPoolSize},
		{"meetings.research_per_meeting", m.ResearchPerMeeting},
		{"meetings.chat_posts_per_minute", m.ChatPostsPerMinute},
		{"meetings.chat_answer_burst", m.ChatAnswerBurst},
		{"meetings.lobby_timeout_s", m.LobbyTimeoutS},
		{"meetings.retention_days", m.RetentionDays},
		{"meetings.budget.max_tokens_per_meeting", m.Budget.MaxTokensPerMeeting},
		{"meetings.budget.max_tokens_per_hour", m.Budget.MaxTokensPerHour},
		{"meetings.budget.max_tokens_per_day", m.Budget.MaxTokensPerDay},
		{"meetings.budget.max_intents_per_meeting", m.Budget.MaxIntentsPerMeeting},
		{"meetings.llm.timeout_ms", m.LLM.TimeoutMS},
		{"meetings.llm.max_in_flight", m.LLM.MaxInFlight},
		{"meetings.llm.per_meeting_in_flight", m.LLM.PerMeetingInFlight},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s must not be negative (got %d)", f.key, f.v)
		}
	}
	b := m.Budget
	if b.MaxTokensPerMeetingOrDefault() > b.MaxTokensPerDayOrDefault() {
		return fmt.Errorf("meetings.budget.max_tokens_per_meeting (%d) exceeds max_tokens_per_day (%d)",
			b.MaxTokensPerMeetingOrDefault(), b.MaxTokensPerDayOrDefault())
	}
	if b.MaxTokensPerHourOrDefault() > b.MaxTokensPerDayOrDefault() {
		return fmt.Errorf("meetings.budget.max_tokens_per_hour (%d) exceeds max_tokens_per_day (%d)",
			b.MaxTokensPerHourOrDefault(), b.MaxTokensPerDayOrDefault())
	}
	return nil
}

// validateMeetingsListen requires a loopback host:port: Quorum is never
// reachable from outside the container.
func validateMeetingsListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("meetings.quorum.listen %q is not host:port: %w", addr, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("meetings.quorum.listen %q has an invalid port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("meetings.quorum.listen %q must be a loopback address", addr)
	}
	return nil
}

// validateMeetingsURL requires an absolute http(s) URL with no credentials.
func validateMeetingsURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a URL: %w", key, raw, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s %q must be an absolute http(s) URL", key, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not carry credentials", key)
	}
	return nil
}
