package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestInboxTranscriptionDefaults(t *testing.T) {
	var tr InboxTranscription
	if tr.Enabled || tr.Active() || tr.EffectiveMode() != "manual" || tr.EffectiveProvider() != "auto" || tr.EffectiveModel() != "nova-3" {
		t.Fatalf("defaults = %+v", tr)
	}
	if tr.EffectiveAPIKeyEnv() != "DEEPGRAM_API_KEY" || !tr.EffectiveDiarize() || tr.EffectiveLanguage() != "auto" {
		t.Fatal("key env / diarize / language defaults")
	}
	if tr.EffectiveMaxDuration() != 120*time.Minute || tr.EffectiveMaxBytes() != 524288000 || tr.EffectiveMonthlyBudget() != 600*time.Minute || tr.EffectiveDailyMaxItems() != 20 {
		t.Fatalf("limit defaults: %v %v %v %v", tr.EffectiveMaxDuration(), tr.EffectiveMaxBytes(), tr.EffectiveMonthlyBudget(), tr.EffectiveDailyMaxItems())
	}
	if err := ValidateInboxTranscription(tr); err != nil {
		t.Fatalf("zero value must validate: %v", err)
	}
}

func TestInboxTranscriptionConfiguredAndEnvOverrides(t *testing.T) {
	off := false
	tr := InboxTranscription{
		Enabled: true, ConsentAcknowledged: true, Mode: "auto", Provider: "deepgram", Model: "nova-2",
		APIKeyEnv: "MY_DG", Sources: []string{"plaud", " zoom "}, Diarize: &off, Language: "pt-BR",
		MaxDurationMinutes: 30, MaxBytes: 1000, MonthlyBudgetMinutes: 60, DailyMaxItems: 2,
	}
	if !tr.Active() || tr.EffectiveModel() != "nova-2" || tr.EffectiveAPIKeyEnv() != "MY_DG" || tr.EffectiveDiarize() || tr.EffectiveLanguage() != "pt-BR" {
		t.Fatalf("configured = %+v", tr)
	}
	if tr.EffectiveMaxDuration() != 30*time.Minute || tr.EffectiveMaxBytes() != 1000 || tr.EffectiveMonthlyBudget() != time.Hour || tr.EffectiveDailyMaxItems() != 2 {
		t.Fatal("configured limits")
	}
	if !tr.AutoSource("plaud") || !tr.AutoSource("zoom") || tr.AutoSource("wisprflow") {
		t.Fatal("auto sources")
	}
	manual := tr
	manual.Mode = "manual"
	if manual.AutoSource("plaud") {
		t.Fatal("manual mode never auto-transcribes")
	}
	if err := ValidateInboxTranscription(tr); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	t.Setenv(EnvInboxTranscriptionMaxDurationMinutes, "45")
	t.Setenv(EnvInboxTranscriptionMaxBytes, "2048")
	t.Setenv(EnvInboxTranscriptionMonthlyBudgetMinutes, "90")
	t.Setenv(EnvInboxTranscriptionDailyMaxItems, "bogus")
	if tr.EffectiveMaxDuration() != 45*time.Minute || tr.EffectiveMaxBytes() != 2048 || tr.EffectiveMonthlyBudget() != 90*time.Minute {
		t.Fatal("env overrides must win")
	}
	if tr.EffectiveDailyMaxItems() != 2 {
		t.Fatal("a malformed env value falls through to the configured value")
	}
}

func TestValidateInboxTranscriptionRejects(t *testing.T) {
	cases := map[string]InboxTranscription{
		"mode":            {Mode: "sometimes"},
		"provider":        {Provider: "openai"},
		"key env":         {APIKeyEnv: "not a name"},
		"language":        {Language: "english please"},
		"negative":        {MaxBytes: -1},
		"sources":         {Sources: []string{" "}},
		"consent":         {Enabled: true},
		"whisper model":   {WhisperModel: "large-v3"},
		"gemini model":    {GeminiModel: "../files/x"},
		"whisper threads": {WhisperThreads: -1},
	}
	for name, tr := range cases {
		if err := ValidateInboxTranscription(tr); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	for _, p := range InboxTranscriptionProviders {
		if err := ValidateInboxTranscription(InboxTranscription{Provider: p}); err != nil {
			t.Errorf("provider %s: %v", p, err)
		}
	}
	err := ValidateInbox(InboxConfig{Transcription: InboxTranscription{Enabled: true}})
	if err == nil || !strings.Contains(err.Error(), "consent_acknowledged") {
		t.Fatalf("ValidateInbox must apply the consent gate: %v", err)
	}
}

func TestInboxTranscriptionYAML(t *testing.T) {
	var c InboxConfig
	src := `
transcription:
  enabled: true
  mode: auto
  provider: deepgram
  model: nova-3
  api_key_env: DEEPGRAM_API_KEY
  sources: [plaud]
  diarize: false
  language: auto
  max_duration_minutes: 120
  max_bytes: 524288000
  monthly_budget_minutes: 600
  daily_max_items: 20
  consent_acknowledged: true
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	tr := c.Transcription
	if !tr.Active() || tr.EffectiveDiarize() || len(tr.Sources) != 1 || tr.MaxBytes != 524288000 || tr.DailyMaxItems != 20 {
		t.Fatalf("parsed = %+v", tr)
	}
	if err := ValidateInbox(c); err != nil {
		t.Fatal(err)
	}
}

func TestInboxTranscriptionWhisperDefaults(t *testing.T) {
	var tr InboxTranscription
	if tr.EffectiveWhisperModel() != "base.en" || tr.EffectiveWhisperThreads(4) != 2 || tr.EffectiveWhisperThreads(1) != 1 {
		t.Fatalf("whisper defaults: %q %d", tr.EffectiveWhisperModel(), tr.EffectiveWhisperThreads(4))
	}
	for lang, want := range map[string]string{"en": "base.en", "en-GB": "base.en", "de": "base", "pt-BR": "base"} {
		if got := (InboxTranscription{Language: lang}).EffectiveWhisperModel(); got != want {
			t.Errorf("language %s: model %q, want %q", lang, got, want)
		}
	}
	tr = InboxTranscription{WhisperModel: "small", WhisperThreads: 3, Language: "de"}
	if tr.EffectiveWhisperModel() != "small" || tr.EffectiveWhisperThreads(16) != 3 {
		t.Fatalf("configured whisper = %q %d", tr.EffectiveWhisperModel(), tr.EffectiveWhisperThreads(16))
	}
	if err := ValidateInboxTranscription(tr); err != nil {
		t.Fatal(err)
	}
}

func TestInboxTranscriptionGeminiModel(t *testing.T) {
	if got := (InboxTranscription{}).EffectiveGeminiModel(); got != "gemini-3.5-flash" {
		t.Fatalf("default gemini model = %q", got)
	}
	tr := InboxTranscription{GeminiModel: "gemini-3.6-flash"}
	if tr.EffectiveGeminiModel() != "gemini-3.6-flash" || ValidateInboxTranscription(tr) != nil {
		t.Fatal("configured gemini model")
	}
}
