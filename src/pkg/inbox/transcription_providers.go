package inbox

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// Where a provider sends the audio (ProviderStatus.AudioGoesTo).
const (
	AudioStaysOnHive = "hive"
	AudioToGoogle    = "google"
	AudioToDeepgram  = "deepgram"
)

// Provider readiness (ProviderStatus.Status).
const (
	ProviderReady        = "ready"
	ProviderNeedsKey     = "missing_credentials"
	ProviderNotInstalled = "not_installed"
)

// autoProviderOrder is the order provider: auto tries (owner approved):
// built-in whisper first, then the hive's own Gemini credentials, then the
// owner's Deepgram key.
var autoProviderOrder = []string{config.InboxTranscriptionProviderWhisper, config.InboxTranscriptionProviderGemini, config.InboxTranscriptionProviderDeepgram}

// ProviderStatus is one provider as the ⚙ Transcription settings show it.
// It never carries a key, only whether one is present.
type ProviderStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Status    string `json:"status"`
	// Detail is one line for the owner: what is present or missing.
	Detail string `json:"detail"`
	// AudioGoesTo is hive, google or deepgram.
	AudioGoesTo string `json:"audio_goes_to"`
	// Model is the model this provider would use.
	Model string `json:"model"`
	// Egress lists hosts this provider needs beyond its API (whisper's
	// first-use model download), for egress allowlists.
	Egress []string `json:"egress,omitempty"`
}

// providerStatus probes one provider without contacting it.
func (s *Service) providerStatus(name string, t config.InboxTranscription) ProviderStatus {
	switch name {
	case config.InboxTranscriptionProviderWhisper:
		return s.whisperStatus(t)
	case config.InboxTranscriptionProviderGemini:
		st := ProviderStatus{Name: name, AudioGoesTo: AudioToGoogle, Model: t.EffectiveGeminiModel()}
		if _, env := s.geminiKey(); env != "" {
			st.Available, st.Status, st.Detail = true, ProviderReady, "the hive's Gemini key is present in $"+env
		} else {
			st.Status, st.Detail = ProviderNeedsKey, "no Gemini credentials: set GEMINI_API_KEY (or GOOGLE_API_KEY, GOOGLE_GENAI_API_KEY) on this hive"
		}
		return st
	}
	env := t.EffectiveAPIKeyEnv()
	st := ProviderStatus{Name: config.InboxTranscriptionProviderDeepgram, AudioGoesTo: AudioToDeepgram, Model: t.EffectiveModel()}
	if strings.TrimSpace(s.getenv(env)) != "" {
		st.Available, st.Status, st.Detail = true, ProviderReady, "key present in $"+env
	} else {
		st.Status, st.Detail = ProviderNeedsKey, "no key: set $"+env+" on this hive"
	}
	return st
}

// providerStatuses lists every provider in auto order.
func (s *Service) providerStatuses(t config.InboxTranscription) []ProviderStatus {
	out := make([]ProviderStatus, 0, len(autoProviderOrder))
	for _, name := range autoProviderOrder {
		out = append(out, s.providerStatus(name, t))
	}
	return out
}

// resolveProvider returns the provider a run uses. An explicit provider is
// used as is (and reports its own missing setup when it runs); auto takes
// the first available provider in autoProviderOrder and, when none is,
// falls back to deepgram so the item says which key is missing.
func (s *Service) resolveProvider(t config.InboxTranscription) ProviderStatus {
	if p := t.EffectiveProvider(); p != config.InboxTranscriptionProviderAuto {
		return s.providerStatus(p, t)
	}
	for _, name := range autoProviderOrder {
		if st := s.providerStatus(name, t); st.Available {
			return st
		}
	}
	return s.providerStatus(config.InboxTranscriptionProviderDeepgram, t)
}

// TranscriptionEgressHosts lists the hosts this hive must reach for
// transcription with provider's API: none while transcription is off or for
// whisper (whose first-use model download adds ProviderStatus.Egress).
func TranscriptionEgressHosts(t config.InboxTranscription, provider string) []string {
	if !t.Enabled {
		return []string{}
	}
	switch provider {
	case config.InboxTranscriptionProviderDeepgram:
		return []string{transcribe.DeepgramHost}
	case config.InboxTranscriptionProviderGemini:
		return []string{transcribe.GeminiHost}
	}
	return []string{}
}
