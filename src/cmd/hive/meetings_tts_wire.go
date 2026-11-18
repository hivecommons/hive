package main

import (
	"context"
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
)

// meetingsTTSResolver returns the dashboard's lookup of the meetings TTS
// service (hivecommons/hive#18710): meetings.tts_url and its token, the
// same Kokoro service Quorum speaks with. It reads the config on every
// call, so a changed URL or token applies without a restart. The token is
// a secret; it is never logged.
func meetingsTTSResolver(cfg *config.Config) dashboard.MeetingsTTSResolver {
	return func(ctx context.Context) (dashboard.MeetingsTTSConfig, error) {
		m := cfg.Meetings
		url := m.TTSURLOrEmpty()
		if url == "" {
			return dashboard.MeetingsTTSConfig{}, dashboard.ErrMeetingsTTSNotConfigured
		}
		token, err := m.TTSToken(ctx)
		if err != nil {
			return dashboard.MeetingsTTSConfig{}, fmt.Errorf("the TTS service token cannot be read: %w", err)
		}
		return dashboard.MeetingsTTSConfig{URL: url, Token: token}, nil
	}
}

// wireMeetingsTTS gives the dashboard the meetings TTS service, so
// Settings, Meeting attendee can play a sample line and a preset's
// introduction in the voice the attendee really speaks with.
func wireMeetingsTTS(cfg *config.Config, srv *dashboard.Server) bool {
	if cfg == nil || srv == nil {
		return false
	}
	srv.SetMeetingsTTSResolver(meetingsTTSResolver(cfg))
	return true
}
