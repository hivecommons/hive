package inbox

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// geminiKeyEnvs are the Gemini credential variables the hive already reads
// for Gemini model discovery (pkg/dashboard cli_models.go), in that order.
// Transcription adds no key of its own.
var geminiKeyEnvs = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_API_KEY"}

// geminiKey returns the hive's Gemini API key and the variable it came from.
func (s *Service) geminiKey() (key, env string) {
	for _, name := range geminiKeyEnvs {
		if v := strings.TrimSpace(s.getenv(name)); v != "" {
			return v, name
		}
	}
	return "", ""
}

// geminiTranscriber builds the gemini provider with the hive's own key.
func (s *Service) geminiTranscriber(t config.InboxTranscription) (transcribe.Transcriber, error) {
	key, _ := s.geminiKey()
	if key == "" {
		return nil, fmt.Errorf("%w: set GEMINI_API_KEY (or GOOGLE_API_KEY, GOOGLE_GENAI_API_KEY) on this hive", transcribe.ErrNoKey)
	}
	return &transcribe.Gemini{APIKey: key, Model: t.EffectiveGeminiModel(), Diarize: t.EffectiveDiarize(), Language: t.EffectiveLanguage()}, nil
}
