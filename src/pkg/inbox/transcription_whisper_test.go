package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

const whisperTestModel = "ggml-model-bytes"

// fakeWhisperRuntime installs executable stand-ins for hive-whisper,
// whisper-cli and ffmpeg, and a pinned test model (present or not).
func fakeWhisperRuntime(t *testing.T, modelPresent, download bool) *WhisperRuntime {
	t.Helper()
	dir := t.TempDir()
	sandbox := "#!/bin/sh\ncat /dev/fd/3 > /dev/null\ncat > /dev/null\n" +
		"echo 'HIVE-WHISPER phase=decoding' >&2\n" +
		"echo 'HIVE-WHISPER phase=transcribing audio_seconds=90.00' >&2\n" +
		"echo 'HIVE-WHISPER progress=0.50 processed_seconds=45.00' >&2\n" +
		"cat <<'JSON'\n{\"result\":{\"language\":\"en\"},\"transcription\":[{\"offsets\":{\"from\":1000,\"to\":3000},\"text\":\" hello hive\"}]}\nJSON\n"
	for name, body := range map[string]string{"hive-whisper": sandbox, "whisper-cli": "#!/bin/sh\n", "ffmpeg": "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte(whisperTestModel))
	models := &transcribe.WhisperModelStore{Dir: filepath.Join(dir, "models"), NoDownload: !download, Models: map[string]transcribe.WhisperModel{
		"base.en": {Name: "base.en", Size: int64(len(whisperTestModel)), SHA256: hex.EncodeToString(sum[:])},
	}}
	if modelPresent {
		if err := os.MkdirAll(models.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(models.Path("base.en"), []byte(whisperTestModel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &WhisperRuntime{Sandbox: filepath.Join(dir, "hive-whisper"), EngineDir: dir, RunAs: transcribe.WhisperRunAsSelf, CPUs: 4, Models: models}
}

func TestWhisperReadinessAndAutoResolution(t *testing.T) {
	e := newTxEnv(t, activeTx(), "http://audio.invalid")
	e.svc.Getenv = func(k string) string {
		if k == "DEEPGRAM_API_KEY" {
			return "dg-secret-value"
		}
		return ""
	}
	// No engine in the image: auto keeps using Deepgram.
	e.svc.Whisper = &WhisperRuntime{Sandbox: "/nonexistent/hive-whisper", EngineDir: "/nonexistent", RunAs: transcribe.WhisperRunAsSelf, Models: &transcribe.WhisperModelStore{Dir: t.TempDir()}}
	if st := e.svc.whisperStatus(activeTx()); st.Available || st.Status != ProviderNotInstalled {
		t.Fatalf("no engine = %+v", st)
	}
	if got := e.svc.resolveProvider(activeTx()); got.Name != "deepgram" {
		t.Fatalf("auto without whisper = %+v", got)
	}

	// Engine present, model present: auto prefers whisper over a Deepgram key.
	e.svc.Whisper = fakeWhisperRuntime(t, true, false)
	st := e.svc.whisperStatus(activeTx())
	if !st.Available || st.AudioGoesTo != AudioStaysOnHive || st.Model != "base.en" || len(st.Egress) != 0 {
		t.Fatalf("ready whisper = %+v", st)
	}
	if got := e.svc.resolveProvider(activeTx()); got.Name != "whisper" {
		t.Fatalf("auto with whisper = %+v", got)
	}
	explicit := activeTx()
	explicit.Provider = "deepgram"
	if got := e.svc.resolveProvider(explicit); got.Name != "deepgram" {
		t.Fatalf("explicit deepgram must stay deepgram: %+v", got)
	}
	settings := e.svc.TranscriptionSettings()
	if settings.ResolvedProvider != "whisper" || len(settings.EgressHosts) != 0 {
		t.Fatalf("whisper with its model needs no egress: %+v", settings)
	}

	// Model missing, downloads allowed: ready, with the model host as egress.
	e.svc.Whisper = fakeWhisperRuntime(t, false, true)
	st = e.svc.whisperStatus(activeTx())
	if !st.Available || !strings.Contains(st.Detail, "downloads on first use") || len(st.Egress) != 2 || st.Egress[0] != transcribe.WhisperModelHost {
		t.Fatalf("download whisper = %+v", st)
	}
	if hosts := e.svc.TranscriptionSettings().EgressHosts; len(hosts) != 2 {
		t.Fatalf("egress = %v", hosts)
	}

	// Model missing, downloads off (air-gapped): not ready, says where to put it.
	e.svc.Whisper = fakeWhisperRuntime(t, false, false)
	st = e.svc.whisperStatus(activeTx())
	if st.Available || !strings.Contains(st.Detail, "ggml-base.en.bin") {
		t.Fatalf("air-gapped whisper = %+v", st)
	}
	if got := e.svc.resolveProvider(activeTx()); got.Name != "deepgram" {
		t.Fatalf("auto falls through to deepgram: %+v", got)
	}

	// A sandbox user that does not exist is not ready.
	rt := fakeWhisperRuntime(t, true, false)
	rt.RunAs = "hive-no-such-user-for-tests"
	e.svc.Whisper = rt
	if st := e.svc.whisperStatus(activeTx()); st.Available || !strings.Contains(st.Detail, "does not exist") {
		t.Fatalf("missing sandbox user = %+v", st)
	}
}

func TestWhisperEndToEndRun(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, activeTx(), srv.URL)
	e.svc.NewTranscriber = nil
	e.svc.Getenv = func(string) string { return "" }
	e.svc.Whisper = fakeWhisperRuntime(t, true, false)
	// Each clock read moves a second, so the run has a measurable duration.
	e.svc.Now = func() time.Time { e.now = e.now.Add(time.Second); return e.now }
	if _, err := e.svc.Transcribe(context.Background(), e.item(t), e.opts()); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.store.Get("plaud", "rec-1")
	if rec.Status != TranscriptionDone || rec.Provider != "whisper" || rec.Model != "base.en" || rec.Text != "[00:01] hello hive" || rec.AudioSeconds != 90 {
		t.Fatalf("whisper record = %+v", rec)
	}
	if rec.Label() != "Transcript (machine: whisper/base.en)" {
		t.Fatalf("label = %q", rec.Label())
	}
	if len(e.audits) != 1 || e.audits[0].Provider != "whisper" || e.audits[0].EstimatedUSD != 0 || e.audits[0].Outcome != TranscriptionDone {
		t.Fatalf("audit = %+v", e.audits)
	}
	if _, ok := e.store.Speed("whisper", "base.en"); !ok {
		t.Fatal("a finished whisper run must record its speed")
	}
}

func TestJobTimeoutScalesForWhisper(t *testing.T) {
	if got := jobTimeout("deepgram", int64(2*time.Hour/time.Millisecond)); got != transcribeJobTimeout {
		t.Fatalf("deepgram = %v", got)
	}
	if got := jobTimeout("whisper", int64(time.Minute/time.Millisecond)); got != transcribeJobTimeout {
		t.Fatalf("short whisper keeps the floor: %v", got)
	}
	long := jobTimeout("whisper", int64(2*time.Hour/time.Millisecond))
	if long <= transcribe.WhisperTimeout(2*time.Hour) {
		t.Fatalf("long whisper job %v must outlast its sandbox timeout", long)
	}
	cfg := config.InboxTranscription{WhisperThreads: 0}
	if cfg.EffectiveWhisperThreads(4) != 2 {
		t.Fatal("half the CPUs by default")
	}
}

func TestDefaultWhisperRuntimeFromEnv(t *testing.T) {
	env := map[string]string{EnvWhisperModelDir: "/m", EnvWhisperModelDownload: "false", EnvWhisperRunAs: "self", EnvWhisperMemoryMB: "1024", EnvWhisperModelBaseURL: "https://mirror.example/models"}
	rt := DefaultWhisperRuntime(func(k string) string { return env[k] })
	if rt.Models.Dir != "/m" || !rt.Models.NoDownload || rt.RunAs != "self" || rt.MemoryMB != 1024 || rt.Models.BaseURL != "https://mirror.example/models" || rt.Sandbox != transcribe.DefaultWhisperSandbox || rt.CPUs < 1 {
		t.Fatalf("runtime = %+v %+v", rt, rt.Models)
	}
	def := DefaultWhisperRuntime(func(string) string { return "" })
	if def.Models.Dir != transcribe.DefaultWhisperModelDir || def.Models.NoDownload || def.RunAs != transcribe.DefaultWhisperRunAs {
		t.Fatalf("defaults = %+v", def)
	}
}
