package transcribe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testModelBytes = "ggml-test-model"

func testModelStore(t *testing.T, present bool) *WhisperModelStore {
	t.Helper()
	sum := sha256.Sum256([]byte(testModelBytes))
	s := &WhisperModelStore{Dir: t.TempDir(), NoDownload: true, Models: map[string]WhisperModel{
		"test": {Name: "test", Size: int64(len(testModelBytes)), SHA256: hex.EncodeToString(sum[:])},
	}}
	if present {
		if err := os.WriteFile(s.Path("test"), []byte(testModelBytes), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// fakeSandbox writes a hive-whisper stand-in: it checks the model arrived
// on descriptor 3, records its environment, swallows stdin, reports the
// protocol on stderr and prints body (whisper JSON) on stdout, exiting code.
func fakeSandbox(t *testing.T, body string, code int) (path, envFile string) {
	t.Helper()
	dir := t.TempDir()
	envFile = filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\n" +
		"env > " + envFile + "\n" +
		"cat /dev/fd/3 > /dev/null || { echo 'HIVE-WHISPER error=no model descriptor' >&2; exit 7; }\n" +
		"cat > /dev/null\n" +
		"echo 'HIVE-WHISPER isolation=netns' >&2\n" +
		"echo 'HIVE-WHISPER phase=decoding' >&2\n" +
		"echo 'HIVE-WHISPER phase=transcribing audio_seconds=20.00' >&2\n" +
		"echo 'HIVE-WHISPER progress=0.50 processed_seconds=10.00' >&2\n" +
		"echo 'engine says hello' >&2\n" +
		"echo 'HIVE-WHISPER phase=finalizing' >&2\n"
	if code != 0 {
		script += "echo 'HIVE-WHISPER error=decode failed: invalid data' >&2\nexit " + strconv.Itoa(code) + "\n"
	} else {
		script += "cat <<'JSON'\n" + body + "\nJSON\n"
	}
	path = filepath.Join(dir, "hive-whisper")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, envFile
}

const sampleWhisperJSON = `{"systeminfo":"CPU","result":{"language":"en"},"transcription":[
 {"offsets":{"from":0,"to":4000},"text":" Hello hive."},
 {"offsets":{"from":4000,"to":6000},"text":" [BLANK_AUDIO]"},
 {"offsets":{"from":65000,"to":70500},"text":" Second thought."}]}`

func whisperAudio(data string, progress func(Progress)) Audio {
	return Audio{
		Open:     func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil },
		Duration: 20 * time.Second, Lang: "en-GB", Progress: progress,
	}
}

func TestWhisperTranscribesThroughTheSandbox(t *testing.T) {
	t.Setenv("DEEPGRAM_API_KEY", "dg-secret-in-hive-env")
	sandbox, envFile := fakeSandbox(t, sampleWhisperJSON, 0)
	w := &Whisper{Models: testModelStore(t, true), Model: "test", Threads: 2, Sandbox: sandbox, RunAs: WhisperRunAsSelf}
	var mu sync.Mutex
	var got []Progress
	res, err := w.Transcribe(context.Background(), whisperAudio("AUDIO", func(p Progress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != WhisperName || res.Model != "test" || res.Language != "en" || res.Duration != 20*time.Second {
		t.Fatalf("result = %+v", res)
	}
	if want := "[00:00] Hello hive.\n[01:05] Second thought."; res.Text() != want {
		t.Fatalf("text = %q, want %q (no speakers, noise dropped)", res.Text(), want)
	}
	var phases []string
	measured := false
	for _, p := range got {
		if p.ProcessedSeconds < 0 {
			phases = append(phases, p.Phase)
		}
		if p.ProcessedSeconds == 10 && p.AudioSeconds == 20 {
			measured = true
		}
	}
	if strings.Join(phases, ",") != "downloading,decoding,transcribing,finalizing" || !measured {
		t.Fatalf("progress = %+v", got)
	}
	env, _ := os.ReadFile(envFile)
	if strings.Contains(string(env), "dg-secret") || strings.Contains(string(env), "DEEPGRAM") {
		t.Fatalf("the sandbox inherited the hive's environment: %s", env)
	}
}

func TestWhisperErrors(t *testing.T) {
	sandbox, _ := fakeSandbox(t, "", 5)
	w := &Whisper{Models: testModelStore(t, true), Model: "test", Sandbox: sandbox, RunAs: WhisperRunAsSelf}
	if _, err := w.Transcribe(context.Background(), whisperAudio("AUDIO", nil)); err == nil || !strings.Contains(err.Error(), "decode failed: invalid data") {
		t.Fatalf("decode error = %v", err)
	}
	big, _ := fakeSandbox(t, "", 3)
	w.Sandbox = big
	if _, err := w.Transcribe(context.Background(), whisperAudio("AUDIO", nil)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large = %v", err)
	}
	bad, _ := fakeSandbox(t, "not json", 0)
	w.Sandbox = bad
	if _, err := w.Transcribe(context.Background(), whisperAudio("AUDIO", nil)); err == nil || !strings.Contains(err.Error(), "decode transcript") {
		t.Fatalf("bad json = %v", err)
	}
	missing := &Whisper{Models: testModelStore(t, false), Model: "test", Sandbox: bad, RunAs: WhisperRunAsSelf}
	if _, err := missing.Transcribe(context.Background(), whisperAudio("AUDIO", nil)); !errors.Is(err, ErrWhisperModelMissing) {
		t.Fatalf("missing model = %v", err)
	}
}

func TestWhisperArgvAndTimeout(t *testing.T) {
	w := &Whisper{Threads: 3, MaxBytes: 1024, MemoryMB: 2048, Sandbox: "/x/hive-whisper", SuExec: "/x/su-exec"}
	argv, err := w.argv(10*time.Minute, "pt-BR")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if want := "/x/su-exec hive-transcribe /x/hive-whisper -threads 3 -lang pt -max-bytes 1024 -timeout 10m0s -memory-mb 2048"; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	w.RunAs = WhisperRunAsSelf
	if argv, _ := w.argv(time.Minute, ""); argv[0] != "/x/hive-whisper" || argv[4] != "auto" {
		t.Fatalf("self argv = %v", argv)
	}
	if got := WhisperTimeout(5 * time.Minute); got != whisperTimeoutBase+20*time.Minute {
		t.Fatalf("timeout for 5 min = %v", got)
	}
	if got := WhisperTimeout(0); got != whisperTimeoutBase+whisperTimeoutRTF*whisperUnknownDuration {
		t.Fatalf("timeout for unknown = %v", got)
	}
}

func TestWhisperModelDownloadAndVerify(t *testing.T) {
	var hits atomic.Int32
	var bodyMu sync.Mutex
	body := testModelBytes
	setBody := func(b string) { bodyMu.Lock(); body = b; bodyMu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		bodyMu.Lock()
		b := body
		bodyMu.Unlock()
		if r.URL.Path != "/ggml-test.bin" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, b)
	}))
	defer srv.Close()
	s := testModelStore(t, false)
	s.NoDownload, s.BaseURL = false, srv.URL
	if s.Present("test") {
		t.Fatal("present before download")
	}
	path, err := s.Ensure(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != whisperModelFileMode || !s.Present("test") {
		t.Fatalf("installed model = %v %v", info, err)
	}
	if _, err := s.Ensure(context.Background(), "test"); err != nil || hits.Load() != 1 {
		t.Fatalf("second Ensure re-downloaded (%d hits): %v", hits.Load(), err)
	}

	// A tampered file is replaced; a tampered download is refused and leaves nothing.
	if err := os.WriteFile(path, []byte("ggml-evil-model"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "test"); err != nil || hits.Load() != 2 {
		t.Fatalf("tampered file not replaced: %v (%d hits)", err, hits.Load())
	}
	setBody("ggml-evil-model")
	_ = os.Remove(path)
	if _, err := s.Ensure(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a model that failed its checksum was installed")
	}
	setBody(testModelBytes + "-and-more")
	if _, err := s.Ensure(context.Background(), "test"); err == nil {
		t.Fatal("an oversize download must fail")
	}
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := s.Ensure(context.Background(), "large-v3"); err == nil {
		t.Fatal("an unpinned model must be refused")
	}
	s.BaseURL = "http://models.example"
	if _, err := s.Ensure(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http must be refused: %v", err)
	}
	off := testModelStore(t, false)
	if _, err := off.Ensure(context.Background(), "test"); !errors.Is(err, ErrWhisperModelMissing) || !strings.Contains(err.Error(), off.Dir) {
		t.Fatalf("air-gapped hint = %v", err)
	}
}

func TestPinnedWhisperModels(t *testing.T) {
	for name, m := range WhisperModels {
		if m.Name != name || m.Size <= 0 || len(m.SHA256) != sha256.Size*2 {
			t.Errorf("model %s pin = %+v", name, m)
		}
	}
	if _, ok := WhisperModels[DefaultWhisperModel]; !ok {
		t.Fatal("default model not pinned")
	}
	if !strings.HasPrefix(DefaultWhisperModelBaseURL, "https://huggingface.co/ggerganov/whisper.cpp/resolve/") {
		t.Fatalf("base URL = %s", DefaultWhisperModelBaseURL)
	}
	if TypicalSpeed(WhisperName).RTF <= TypicalSpeed(DeepgramName).RTF {
		t.Fatal("whisper's typical speed must be the conservative one")
	}
}

func TestWhisperGuardsAndHelpers(t *testing.T) {
	if _, err := (&Whisper{}).Transcribe(context.Background(), whisperAudio("A", nil)); err == nil {
		t.Fatal("no model store must fail")
	}
	if _, err := (&Whisper{Models: testModelStore(t, true)}).Transcribe(context.Background(), Audio{}); err == nil {
		t.Fatal("no audio must fail")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := (&Whisper{}).argv(time.Minute, "en"); err == nil || !strings.Contains(err.Error(), "su-exec") {
		t.Fatalf("missing su-exec = %v", err)
	}
	for in, want := range map[string]string{"": "auto", "AUTO": "auto", "en-US": "en", "de": "de"} {
		if got := whisperLanguage(in); got != want {
			t.Errorf("whisperLanguage(%q) = %q", in, got)
		}
	}
	b := limitedBuffer{limit: 4}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("de"))
	if !b.over || b.String() != "abc" {
		t.Fatalf("limited buffer = %q over=%v", b.String(), b.over)
	}
	if (&Whisper{}).Name() != WhisperName || (&Whisper{}).model() != DefaultWhisperModel {
		t.Fatal("name/model defaults")
	}
	if m := (WhisperModel{Name: "base.en"}); m.File() != "ggml-base.en.bin" {
		t.Fatal(m.File())
	}
}
