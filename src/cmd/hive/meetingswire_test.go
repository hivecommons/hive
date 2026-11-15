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
		Enabled: true,
		Quorum:  config.MeetingsQuorumConfig{Binary: bin},
		Vexa:    config.MeetingsVexaConfig{URL: "https://vexa/", APIKeyFile: keyFile},
		Discord: config.MeetingsDiscordConfig{TokenFile: filepath.Join(dir, "missing")},
	}}
	var logs bytes.Buffer
	sup := wireMeetings(context.Background(), cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if sup == nil || !fake.started {
		t.Fatal("want a started supervisor")
	}
	if got.VexaAPIKey != "tenant" || got.VexaURL != "https://vexa" || got.DiscordToken != "" ||
		got.Listen != config.DefaultMeetingsQuorumListen || got.Binary != bin {
		t.Fatalf("options %+v", got)
	}
	if strings.Contains(logs.String(), "tenant") {
		t.Fatal("the tenant key must never be logged")
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
		Vexa:    config.MeetingsVexaConfig{APIKeyFile: dir},
		Discord: config.MeetingsDiscordConfig{TokenFile: dir},
	}
	var logs bytes.Buffer
	o := meetingsQuorumOptions(context.Background(), m, slog.New(slog.NewTextHandler(&logs, nil)))
	if o.VexaAPIKey != "" || o.DiscordToken != "" {
		t.Fatal("unreadable secrets must be left unset")
	}
	if !strings.Contains(logs.String(), "Vexa tenant key unreadable") || !strings.Contains(logs.String(), "Discord bot token unreadable") {
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
	if wireMeetingsManager(nil, &fakeQuorumSup{}, nil) != nil || wireMeetingsManager(&config.Config{}, nil, nil) != nil {
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
	mgr := wireMeetingsManager(cfg, &fakeQuorumSup{}, slog.New(slog.NewTextHandler(&logs, nil)))
	if mgr == nil {
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
	if wireMeetingsManager(cfg, &fakeQuorumSup{}, slog.New(slog.DiscardHandler)) != nil ||
		wireMeetingsManager(cfg, &fakeQuorumSup{}, nil) != nil {
		t.Fatal("an unusable store must leave meetings unwired")
	}
}
