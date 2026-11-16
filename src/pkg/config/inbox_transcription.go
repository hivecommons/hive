package config

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Inbox transcription modes.
const (
	// InboxTranscriptionModeManual transcribes only when the owner clicks
	// Transcribe on an Inbox item.
	InboxTranscriptionModeManual = "manual"
	// InboxTranscriptionModeAuto also transcribes new pending items from the
	// sources listed in inbox.transcription.sources after each sync.
	InboxTranscriptionModeAuto = "auto"
)

// Inbox transcription providers (inbox.transcription.provider).
const (
	// InboxTranscriptionProviderAuto picks the first available provider in
	// the order whisper, gemini, deepgram (see inbox-transcription.md).
	InboxTranscriptionProviderAuto = "auto"
	// InboxTranscriptionProviderWhisper is whisper.cpp built into the hive
	// image: no account, the audio never leaves the spoke.
	InboxTranscriptionProviderWhisper = "whisper"
	// InboxTranscriptionProviderGemini uses the hive's own Gemini credentials.
	InboxTranscriptionProviderGemini = "gemini"
	// InboxTranscriptionProviderDeepgram uses the owner's Deepgram key.
	InboxTranscriptionProviderDeepgram = "deepgram"
)

// InboxTranscriptionProviders lists the accepted provider values.
var InboxTranscriptionProviders = []string{InboxTranscriptionProviderAuto, InboxTranscriptionProviderWhisper, InboxTranscriptionProviderGemini, InboxTranscriptionProviderDeepgram}

// Inbox transcription defaults. Each spoke transcribes on its own hive or
// with its owner's own provider account; these bound what one hive spends.
const (
	// DefaultInboxTranscriptionProvider picks a provider by availability.
	DefaultInboxTranscriptionProvider = InboxTranscriptionProviderAuto
	// DefaultInboxTranscriptionModel is Deepgram's pre-recorded model.
	DefaultInboxTranscriptionModel = "nova-3"
	// DefaultInboxTranscriptionKeyEnv names the variable holding the key.
	DefaultInboxTranscriptionKeyEnv = "DEEPGRAM_API_KEY"
	// DefaultInboxTranscriptionLanguage asks the provider to detect it.
	DefaultInboxTranscriptionLanguage = "auto"
	// DefaultInboxTranscriptionMaxDurationMinutes skips longer recordings.
	DefaultInboxTranscriptionMaxDurationMinutes = 120
	// DefaultInboxTranscriptionMaxBytes (500 MiB) cuts larger uploads.
	DefaultInboxTranscriptionMaxBytes int64 = 500 << 20
	// DefaultInboxTranscriptionMonthlyBudgetMinutes caps audio minutes sent
	// per calendar month (UTC).
	DefaultInboxTranscriptionMonthlyBudgetMinutes = 600
	// DefaultInboxTranscriptionDailyMaxItems caps transcription runs per UTC day.
	DefaultInboxTranscriptionDailyMaxItems = 20
)

// Environment overrides for the transcription limits (env > hive.yaml >
// default; a non-positive or malformed value falls through).
const (
	EnvInboxTranscriptionMaxDurationMinutes   = "HIVE_INBOX_TRANSCRIPTION_MAX_DURATION_MINUTES"
	EnvInboxTranscriptionMaxBytes             = "HIVE_INBOX_TRANSCRIPTION_MAX_BYTES"
	EnvInboxTranscriptionMonthlyBudgetMinutes = "HIVE_INBOX_TRANSCRIPTION_MONTHLY_BUDGET_MINUTES"
	EnvInboxTranscriptionDailyMaxItems        = "HIVE_INBOX_TRANSCRIPTION_DAILY_MAX_ITEMS"
)

// InboxTranscription is `inbox.transcription`: spoke-local machine
// transcription for Inbox recordings that arrive without text. Depending on
// the provider the audio stays on the spoke (whisper) or is sent to the
// provider under the hive's own account (gemini, deepgram); the transcript
// is stored only on this spoke. Off by default, and enabling it requires
// consent_acknowledged: true.
type InboxTranscription struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// Mode is manual (default) or auto.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Provider is auto (default), whisper, gemini or deepgram.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Model is the Deepgram model; it defaults to nova-3.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the provider key
	// (default DEEPGRAM_API_KEY). The key itself never lives in hive.yaml.
	APIKeyEnv string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	// Sources lists the Inbox source names auto mode transcribes.
	Sources []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	// Diarize labels speakers; nil means true.
	Diarize *bool `yaml:"diarize,omitempty" json:"diarize,omitempty"`
	// Language is a BCP-47 code or auto (default).
	Language string `yaml:"language,omitempty" json:"language,omitempty"`
	// MaxDurationMinutes skips longer recordings before uploading.
	MaxDurationMinutes int `yaml:"max_duration_minutes,omitempty" json:"max_duration_minutes,omitempty"`
	// MaxBytes cuts an upload that grows past this many bytes.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// MonthlyBudgetMinutes caps audio minutes per UTC calendar month.
	MonthlyBudgetMinutes int `yaml:"monthly_budget_minutes,omitempty" json:"monthly_budget_minutes,omitempty"`
	// DailyMaxItems caps transcription runs per UTC day.
	DailyMaxItems int `yaml:"daily_max_items,omitempty" json:"daily_max_items,omitempty"`
	// WhisperModel is the ggml model the built-in whisper provider uses:
	// tiny, tiny.en, base, base.en, small or small.en. Empty means base.en,
	// or base when Language names another language.
	WhisperModel string `yaml:"whisper_model,omitempty" json:"whisper_model,omitempty"`
	// WhisperThreads is the whisper.cpp thread count; 0 means half the
	// hive's CPUs (at least one), so agents keep the rest.
	WhisperThreads int `yaml:"whisper_threads,omitempty" json:"whisper_threads,omitempty"`
	// GeminiModel is the Gemini model the gemini provider uses; empty means
	// gemini-3.5-flash.
	GeminiModel string `yaml:"gemini_model,omitempty" json:"gemini_model,omitempty"`
	// ConsentAcknowledged records that the owner read the notice: recordings
	// contain other people's voices, and where the chosen provider sends the
	// audio (nowhere for whisper; Google or Deepgram otherwise).
	ConsentAcknowledged bool `yaml:"consent_acknowledged,omitempty" json:"consent_acknowledged"`
}

var (
	inboxEnvNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	inboxLanguageRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	// inboxGeminiModelRe keeps gemini_model a plain id (it lands in a URL path).
	inboxGeminiModelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,63}$`)
)

// EffectiveMode returns the mode, defaulting to manual.
func (t InboxTranscription) EffectiveMode() string {
	if m := strings.TrimSpace(t.Mode); m != "" {
		return m
	}
	return InboxTranscriptionModeManual
}

// EffectiveProvider returns the configured provider, defaulting to auto.
// Which provider auto resolves to depends on what is available on the hive
// (inbox.Service resolves it).
func (t InboxTranscription) EffectiveProvider() string {
	if p := strings.TrimSpace(t.Provider); p != "" {
		return p
	}
	return DefaultInboxTranscriptionProvider
}

// EffectiveModel returns the Deepgram model, defaulting to nova-3.
func (t InboxTranscription) EffectiveModel() string {
	if m := strings.TrimSpace(t.Model); m != "" {
		return m
	}
	return DefaultInboxTranscriptionModel
}

// EffectiveAPIKeyEnv returns the key variable name, defaulting to
// DEEPGRAM_API_KEY.
func (t InboxTranscription) EffectiveAPIKeyEnv() string {
	if e := strings.TrimSpace(t.APIKeyEnv); e != "" {
		return e
	}
	return DefaultInboxTranscriptionKeyEnv
}

// InboxWhisperModels lists the accepted whisper_model values.
var InboxWhisperModels = []string{"tiny", "tiny.en", "base", "base.en", "small", "small.en"}

// EffectiveWhisperModel returns the whisper model: whisper_model when set,
// else base.en for English or auto-detected speech and base otherwise.
func (t InboxTranscription) EffectiveWhisperModel() string {
	if m := strings.TrimSpace(t.WhisperModel); m != "" {
		return m
	}
	if l := strings.ToLower(t.EffectiveLanguage()); l == DefaultInboxTranscriptionLanguage || l == "en" || strings.HasPrefix(l, "en-") {
		return "base.en"
	}
	return "base"
}

// EffectiveWhisperThreads returns whisper_threads, or half of cpus (at least 1).
func (t InboxTranscription) EffectiveWhisperThreads(cpus int) int {
	if t.WhisperThreads > 0 {
		return t.WhisperThreads
	}
	return max(cpus/2, 1)
}

// DefaultInboxTranscriptionGeminiModel is the gemini provider's model.
const DefaultInboxTranscriptionGeminiModel = "gemini-3.5-flash"

// EffectiveGeminiModel returns gemini_model, defaulting to gemini-3.5-flash.
func (t InboxTranscription) EffectiveGeminiModel() string {
	if m := strings.TrimSpace(t.GeminiModel); m != "" {
		return m
	}
	return DefaultInboxTranscriptionGeminiModel
}

// EffectiveDiarize returns Diarize, defaulting to true.
func (t InboxTranscription) EffectiveDiarize() bool { return t.Diarize == nil || *t.Diarize }

// EffectiveLanguage returns the language, defaulting to auto.
func (t InboxTranscription) EffectiveLanguage() string {
	if l := strings.TrimSpace(t.Language); l != "" {
		return l
	}
	return DefaultInboxTranscriptionLanguage
}

// EffectiveMaxDuration returns the longest recording that is transcribed.
func (t InboxTranscription) EffectiveMaxDuration() time.Duration {
	return time.Duration(inboxLimit(EnvInboxTranscriptionMaxDurationMinutes, int64(t.MaxDurationMinutes), DefaultInboxTranscriptionMaxDurationMinutes)) * time.Minute
}

// EffectiveMaxBytes returns the upload byte cap.
func (t InboxTranscription) EffectiveMaxBytes() int64 {
	return inboxLimit(EnvInboxTranscriptionMaxBytes, t.MaxBytes, DefaultInboxTranscriptionMaxBytes)
}

// EffectiveMonthlyBudget returns the audio allowed per UTC month.
func (t InboxTranscription) EffectiveMonthlyBudget() time.Duration {
	return time.Duration(inboxLimit(EnvInboxTranscriptionMonthlyBudgetMinutes, int64(t.MonthlyBudgetMinutes), DefaultInboxTranscriptionMonthlyBudgetMinutes)) * time.Minute
}

// EffectiveDailyMaxItems returns the runs allowed per UTC day.
func (t InboxTranscription) EffectiveDailyMaxItems() int {
	return int(inboxLimit(EnvInboxTranscriptionDailyMaxItems, int64(t.DailyMaxItems), DefaultInboxTranscriptionDailyMaxItems))
}

// Active reports that transcription is enabled and consented to.
func (t InboxTranscription) Active() bool { return t.Enabled && t.ConsentAcknowledged }

// AutoSource reports whether auto mode transcribes items from source.
func (t InboxTranscription) AutoSource(source string) bool {
	if !t.Active() || t.EffectiveMode() != InboxTranscriptionModeAuto {
		return false
	}
	for _, s := range t.Sources {
		if strings.TrimSpace(s) == source {
			return true
		}
	}
	return false
}

// inboxLimit resolves env > configured > def; non-positive values fall
// through to the next source.
func inboxLimit(env string, configured, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if configured > 0 {
		return configured
	}
	return def
}

// ValidateInboxTranscription rejects unknown modes and providers, bad env
// names and languages, negative limits, and enabling without consent.
func ValidateInboxTranscription(t InboxTranscription) error {
	switch t.EffectiveMode() {
	case InboxTranscriptionModeManual, InboxTranscriptionModeAuto:
	default:
		return fmt.Errorf("inbox.transcription.mode %q: must be %s or %s", t.Mode, InboxTranscriptionModeManual, InboxTranscriptionModeAuto)
	}
	if p := t.EffectiveProvider(); !slices.Contains(InboxTranscriptionProviders, p) {
		return fmt.Errorf("inbox.transcription.provider %q: must be one of %s", p, strings.Join(InboxTranscriptionProviders, ", "))
	}
	if !inboxEnvNameRe.MatchString(t.EffectiveAPIKeyEnv()) {
		return fmt.Errorf("inbox.transcription.api_key_env %q: must be an environment variable name", t.APIKeyEnv)
	}
	if l := t.EffectiveLanguage(); l != DefaultInboxTranscriptionLanguage && !inboxLanguageRe.MatchString(l) {
		return fmt.Errorf("inbox.transcription.language %q: must be auto or a language code such as en or pt-BR", l)
	}
	if m := strings.TrimSpace(t.WhisperModel); m != "" && !slices.Contains(InboxWhisperModels, m) {
		return fmt.Errorf("inbox.transcription.whisper_model %q: must be one of %s", m, strings.Join(InboxWhisperModels, ", "))
	}
	if m := strings.TrimSpace(t.GeminiModel); m != "" && !inboxGeminiModelRe.MatchString(m) {
		return fmt.Errorf("inbox.transcription.gemini_model %q: must be a Gemini model id such as gemini-3.5-flash", m)
	}
	if t.MaxDurationMinutes < 0 || t.MaxBytes < 0 || t.MonthlyBudgetMinutes < 0 || t.DailyMaxItems < 0 || t.WhisperThreads < 0 {
		return fmt.Errorf("inbox.transcription limits must not be negative")
	}
	for i, s := range t.Sources {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("inbox.transcription.sources[%d]: empty source name", i)
		}
	}
	if t.Enabled && !t.ConsentAcknowledged {
		return fmt.Errorf("inbox.transcription.enabled requires inbox.transcription.consent_acknowledged: true (recordings contain other people's voices; with gemini or deepgram the audio is sent to that provider under the hive's own account)")
	}
	return nil
}
