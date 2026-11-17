package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
)

func TestMeetingsTTSResolver(t *testing.T) {
	t.Setenv(config.MeetingsTTSURLEnv, "")
	t.Setenv(config.MeetingsTTSTokenEnv, "")
	t.Setenv(config.MeetingsTTSTokenFileEnv, filepath.Join(t.TempDir(), "absent"))
	cfg := &config.Config{}
	resolve := meetingsTTSResolver(cfg)
	if _, err := resolve(context.Background()); !errors.Is(err, dashboard.ErrMeetingsTTSNotConfigured) {
		t.Fatalf("no tts_url = %v, want ErrMeetingsTTSNotConfigured", err)
	}
	// The config is read on every call: a URL set later applies.
	cfg.Meetings.TTSURL = "http://tts.example:8880/"
	got, err := resolve(context.Background())
	if err != nil || got.URL != "http://tts.example:8880" || got.Token != "" {
		t.Fatalf("url only = %+v, %v", got, err)
	}
	t.Setenv(config.MeetingsTTSTokenEnv, "tts-secret")
	if got, err = resolve(context.Background()); err != nil || got.Token != "tts-secret" {
		t.Fatalf("with token = %+v, %v", got, err)
	}
}

func TestWireMeetingsTTS(t *testing.T) {
	srv := dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if wireMeetingsTTS(nil, srv) || wireMeetingsTTS(&config.Config{}, nil) {
		t.Fatal("a missing config or server must not wire")
	}
	if !wireMeetingsTTS(&config.Config{}, srv) {
		t.Fatal("wireMeetingsTTS = false")
	}
}
