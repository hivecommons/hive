package inbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

func TestGeminiReadinessAutoOrderAndTranscriber(t *testing.T) {
	e := newTxEnv(t, activeTx(), "http://audio.invalid")
	env := map[string]string{"DEEPGRAM_API_KEY": "dg-secret-value"}
	e.svc.Getenv = func(k string) string { return env[k] }
	e.svc.Whisper = &WhisperRuntime{Sandbox: "/nonexistent/hive-whisper", EngineDir: "/nonexistent", RunAs: transcribe.WhisperRunAsSelf, Models: &transcribe.WhisperModelStore{Dir: t.TempDir()}}

	if st := e.svc.providerStatus("gemini", activeTx()); st.Available || st.Status != ProviderNeedsKey || !strings.Contains(st.Detail, "GEMINI_API_KEY") {
		t.Fatalf("no key = %+v", st)
	}
	if got := e.svc.resolveProvider(activeTx()); got.Name != "deepgram" {
		t.Fatalf("auto without gemini = %s", got.Name)
	}
	if _, err := e.svc.geminiTranscriber(activeTx()); !errors.Is(err, transcribe.ErrNoKey) {
		t.Fatalf("no key transcriber = %v", err)
	}

	// The hive's existing Gemini key (any of the three names) makes gemini
	// ready, and auto prefers it over Deepgram when whisper is unavailable.
	env["GOOGLE_API_KEY"] = "AIza-secret-value"
	st := e.svc.providerStatus("gemini", activeTx())
	if !st.Available || st.AudioGoesTo != AudioToGoogle || st.Model != "gemini-3.5-flash" || !strings.Contains(st.Detail, "$GOOGLE_API_KEY") {
		t.Fatalf("ready = %+v", st)
	}
	if got := e.svc.resolveProvider(activeTx()); got.Name != "gemini" {
		t.Fatalf("auto with gemini = %s", got.Name)
	}
	explicit := activeTx()
	explicit.Provider = "deepgram"
	if got := e.svc.resolveProvider(explicit); got.Name != "deepgram" {
		t.Fatalf("explicit deepgram = %s", got.Name)
	}
	settings := e.svc.TranscriptionSettings()
	if settings.ResolvedProvider != "gemini" || len(settings.EgressHosts) != 1 || settings.EgressHosts[0] != transcribe.GeminiHost {
		t.Fatalf("settings = %+v", settings)
	}
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), "AIza-secret") || strings.Contains(string(raw), "dg-secret") {
		t.Fatalf("settings leak a key: %s", raw)
	}

	env["GEMINI_API_KEY"] = "AIza-preferred"
	cfg := config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, GeminiModel: "gemini-3.6-flash", Language: "de"}
	tr, err := e.svc.geminiTranscriber(cfg)
	g, ok := tr.(*transcribe.Gemini)
	if err != nil || !ok || g.APIKey != "AIza-preferred" || g.Model != "gemini-3.6-flash" || !g.Diarize || g.Language != "de" {
		t.Fatalf("gemini transcriber = %+v %v", tr, err)
	}
}
