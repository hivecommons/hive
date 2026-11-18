package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/quorumproc"
)

type fakeQuorumSup struct {
	startErr error
	started  bool
	stopped  bool
}

func (f *fakeQuorumSup) Start(context.Context) error {
	f.started = true
	return f.startErr
}

func (f *fakeQuorumSup) Stop() { f.stopped = true }

func (f *fakeQuorumSup) BaseURL() string { return "http://127.0.0.1:1" }

func (f *fakeQuorumSup) Token() string { return "t" }

func withQuorumSupervisor(t *testing.T, fn func(quorumproc.Options) (quorumSupervisor, error)) {
	t.Helper()
	prev := newQuorumSupervisor
	newQuorumSupervisor = fn
	t.Cleanup(func() { newQuorumSupervisor = prev })
}

func TestWireMeetingsOffByDefault(t *testing.T) {
	withQuorumSupervisor(t, func(quorumproc.Options) (quorumSupervisor, error) {
		t.Fatal("no supervisor may be built with meetings off")
		return nil, nil
	})
	if wireMeetings(context.Background(), nil, nil) != nil {
		t.Fatal("nil config must not wire meetings")
	}
	if wireMeetings(context.Background(), &config.Config{}, nil) != nil {
		t.Fatal("meetings must be off by default")
	}
}

func TestWireMeetingsStarts(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "vexa")
	if err := os.WriteFile(keyFile, []byte("tenant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	txFile := filepath.Join(dir, "transcribe")
	if err := os.WriteFile(txFile, []byte("tx-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ttsFile := filepath.Join(dir, "tts")
	if err := os.WriteFile(ttsFile, []byte("tts-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeQuorumSup{}
	var got quorumproc.Options
	withQuorumSupervisor(t, func(o quorumproc.Options) (quorumSupervisor, error) {
		got = o
		return fake, nil
	})
	bin := filepath.Join(dir, "quorum.mjs")
	if err := os.WriteFile(bin, []byte("//"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Meetings: config.MeetingsConfig{
		Enabled:             true,
		Quorum:              config.MeetingsQuorumConfig{Binary: bin},
		Vexa:                config.MeetingsVexaConfig{URL: "https://vexa/", APIKeyFile: keyFile},
		Discord:             config.MeetingsDiscordConfig{TokenFile: filepath.Join(dir, "missing")},
		TranscribeURL:       "http://vexa-transcription.meetings.svc.cluster.local:8000",
		TranscribeTokenFile: txFile,
		TTSURL:              "http://vexa-tts.meetings.svc.cluster.local:8880",
		TTSTokenFile:        ttsFile,
	}}
	var logs bytes.Buffer
	sup := wireMeetings(context.Background(), cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if sup == nil || !fake.started {
		t.Fatal("want a started supervisor")
	}
	if got.VexaAPIKey != "tenant" || got.VexaURL != "https://vexa" || got.DiscordToken != "" ||
		got.Listen != config.DefaultMeetingsQuorumListen || got.Binary != bin ||
		got.TranscribeToken != "tx-secret" || got.TranscribeURL != "http://vexa-transcription.meetings.svc.cluster.local:8000" {
		t.Fatalf("options %+v", got)
	}
	if got.TTSToken != "tts-secret" || got.TTSURL != "http://vexa-tts.meetings.svc.cluster.local:8880" || got.TTSVoice != "af_heart" {
		t.Fatalf("tts options url=%q voice=%q token set=%v", got.TTSURL, got.TTSVoice, got.TTSToken != "")
	}
	if strings.Contains(logs.String(), "tenant") || strings.Contains(logs.String(), "tx-secret") || strings.Contains(logs.String(), "tts-secret") {
		t.Fatal("the tenant key and service tokens must never be logged")
	}
}

func TestWireMeetingsFailures(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "quorum.mjs")
	cfg := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, Quorum: config.MeetingsQuorumConfig{Binary: bin}}}
	withQuorumSupervisor(t, func(quorumproc.Options) (quorumSupervisor, error) {
		t.Fatal("a missing bundle must not build a supervisor")
		return nil, nil
	})
	if wireMeetings(context.Background(), cfg, slog.New(slog.DiscardHandler)) != nil ||
		wireMeetings(context.Background(), cfg, nil) != nil {
		t.Fatal("a missing bundle must leave meetings unwired")
	}
	if err := os.WriteFile(bin, []byte("//"), 0o600); err != nil {
		t.Fatal(err)
	}
	withQuorumSupervisor(t, func(quorumproc.Options) (quorumSupervisor, error) { return nil, errors.New("boom") })
	if wireMeetings(context.Background(), cfg, slog.New(slog.DiscardHandler)) != nil {
		t.Fatal("a construction error must leave meetings unwired")
	}
	fake := &fakeQuorumSup{startErr: errors.New("nope")}
	withQuorumSupervisor(t, func(quorumproc.Options) (quorumSupervisor, error) { return fake, nil })
	if wireMeetings(context.Background(), cfg, slog.New(slog.DiscardHandler)) != nil || !fake.stopped {
		t.Fatal("a start error must stop and drop the supervisor")
	}
	if wireMeetings(context.Background(), cfg, nil) != nil {
		t.Fatal("nil logger path")
	}
}

func TestMeetingsQuorumOptionsUnreadableSecrets(t *testing.T) {
	dir := t.TempDir()
	m := config.MeetingsConfig{
		Vexa:                config.MeetingsVexaConfig{APIKeyFile: dir},
		Discord:             config.MeetingsDiscordConfig{TokenFile: dir},
		TranscribeTokenFile: dir,
		TTSTokenFile:        dir,
	}
	var logs bytes.Buffer
	o := meetingsQuorumOptions(context.Background(), m, slog.New(slog.NewTextHandler(&logs, nil)))
	if o.VexaAPIKey != "" || o.DiscordToken != "" || o.TranscribeToken != "" || o.TTSToken != "" {
		t.Fatal("unreadable secrets must be left unset")
	}
	if !strings.Contains(logs.String(), "Vexa tenant key unreadable") || !strings.Contains(logs.String(), "Discord bot token unreadable") ||
		!strings.Contains(logs.String(), "transcription service token unreadable") || !strings.Contains(logs.String(), "TTS service token unreadable") {
		t.Fatalf("logs %q", logs.String())
	}
}

func TestNewQuorumSupervisorDefault(t *testing.T) {
	sup, err := newQuorumSupervisor(quorumproc.Options{Listen: "127.0.0.1:1", Binary: "/q", RuntimeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	sup.Stop()
}

func TestWireMeetingsManager(t *testing.T) {
	if m, _ := wireMeetingsManager(nil, &fakeQuorumSup{}, nil, nil); m != nil {
		t.Fatal("nil config must not build a manager")
	}
	if m, _ := wireMeetingsManager(&config.Config{}, nil, nil, nil); m != nil {
		t.Fatal("nil inputs must not build a manager")
	}
	dir := t.TempDir()
	cfg := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, DataDir: filepath.Join(dir, "meetings")}}
	store, err := meetings.NewStore(cfg.Meetings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(meetings.Meeting{ID: "mtg-x", NativeID: "q", State: meetings.StateLive}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	mgr, mstore := wireMeetingsManager(cfg, &fakeQuorumSup{}, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if mgr == nil || mstore == nil {
		t.Fatal("want a manager")
	}
	mgr.Close()
	if !strings.Contains(logs.String(), "re-attached") {
		t.Fatalf("logs %q", logs.String())
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Meetings.DataDir = filepath.Join(blocker, "sub")
	if m, _ := wireMeetingsManager(cfg, &fakeQuorumSup{}, nil, slog.New(slog.DiscardHandler)); m != nil {
		t.Fatal("an unusable store must leave meetings unwired")
	}
	if m, _ := wireMeetingsManager(cfg, &fakeQuorumSup{}, nil, nil); m != nil {
		t.Fatal("an unusable store must leave meetings unwired")
	}
}

func TestWireMeetingsLive(t *testing.T) {
	if wireMeetingsLive(nil, nil, nil, nil, nil, nil, nil, nil) != nil {
		t.Fatal("nil inputs must not build a live loop")
	}
	dir := t.TempDir()
	store, err := meetings.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: meetings.NewClient("http://127.0.0.1:1", "t", nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	cfg := &config.Config{Meetings: config.MeetingsConfig{Enabled: true}}
	r := wireMeetingsLive(cfg, mgr, store, &knowledge.KnowledgeAPI{}, nil, nil, slog.New(slog.DiscardHandler), nil)
	if r == nil {
		t.Fatal("want a live runner")
	}
	r.Close()
	args := meetingSpekArgs(meetings.Meeting{ID: "mtg-1", Title: "Standup"}, "/ctx")
	if got := strings.Join(args, " "); !strings.HasPrefix(got, "meeting --from stdin --format jsonl --title Meeting notes: Standup") || !strings.HasSuffix(got, "--context /ctx") {
		t.Fatalf("args %q", got)
	}
}

func TestMeetingSpekEnv(t *testing.T) {
	get := func(k string) string {
		return map[string]string{"PATH": "/bin", "HIVE_GITHUB_TOKEN": "ghp_x", "HTTPS_PROXY": "http://p"}[k]
	}
	asMap := func(env []string) map[string]string {
		out := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	openai := asMap(meetingSpekEnv(get, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeOpenAI, Endpoint: "https://llm", Model: "m", APIKey: "k", TimeoutMS: 2500, MaxInFlight: 4},
		"/usr/local/bin/hive", "/data/meetings/mtg-1/llm-usage.jsonl", "/data/meetings/.llm-slots", 1000))
	if openai["SPEK_MEETING_LLM"] != "command" || openai["SPEK_MEETING_LLM_CMD"] != "/usr/local/bin/hive meeting-llm" ||
		openai["HIVE_MEETING_LLM_API_KEY"] != "k" || openai["HIVE_MEETING_LLM_MAX_TOKENS"] != "1000" || openai["PATH"] != "/bin" || openai["HTTPS_PROXY"] != "http://p" {
		t.Fatalf("openai env %v", openai)
	}
	if _, leaked := openai["HIVE_GITHUB_TOKEN"]; leaked {
		t.Fatal("hive secrets must not reach spek")
	}
	noKey := asMap(meetingSpekEnv(get, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeOpenAI, Endpoint: "e", Model: "m"}, "h", "u", "s", 0))
	if _, ok := noKey["HIVE_MEETING_LLM_API_KEY"]; ok {
		t.Fatal("no key, no key variable")
	}
	anth := asMap(meetingSpekEnv(get, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeAnthropic, AnthropicKey: "ak", AnthropicModel: "claude-x", TimeoutMS: 2500}, "h", "u", "s", 0))
	if anth["SPEK_MEETING_LLM"] != "anthropic" || anth["ANTHROPIC_API_KEY"] != "ak" || anth["SPEK_MEETING_MODEL"] != "claude-x" {
		t.Fatalf("anthropic env %v", anth)
	}
	if _, ok := asMap(meetingSpekEnv(get, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeAnthropic, AnthropicKey: "ak"}, "h", "u", "s", 0))["SPEK_MEETING_MODEL"]; ok {
		t.Fatal("no model override without anthropic_model")
	}
	none := asMap(meetingSpekEnv(get, config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeNone}, "h", "u", "s", 0))
	if none["SPEK_MEETING_LLM"] != "none" {
		t.Fatalf("none env %v", none)
	}
	store, _ := meetings.NewStore(t.TempDir())
	if !strings.HasSuffix(meetingUsageFile(store, "mtg-1"), "mtg-1/llm-usage.jsonl") {
		t.Fatal("usage file path")
	}
}
