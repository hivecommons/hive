package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/quorumproc"
)

// quorumSupervisor is the slice of *quorumproc.Supervisor boot uses.
type quorumSupervisor interface {
	Start(ctx context.Context) error
	Stop()
	BaseURL() string
	Token() string
}

// newQuorumSupervisor is the constructor seam; tests replace it.
var newQuorumSupervisor = func(opts quorumproc.Options) (quorumSupervisor, error) {
	return quorumproc.New(opts)
}

// meetingsQuorumOptions builds the Quorum child options from the meetings
// config, resolving the Vexa tenant key and the optional Discord bot token
// from the keystore. A secret that cannot be read is logged and left unset:
// Quorum then answers platform_unavailable for that platform instead of the
// hive failing to boot.
func meetingsQuorumOptions(ctx context.Context, m config.MeetingsConfig, logger *slog.Logger) quorumproc.Options {
	vexaKey, err := m.VexaAPIKey(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] Vexa tenant key unreadable; meeting platforms other than Discord are unavailable", "error", err)
	}
	discordToken, err := m.DiscordToken(ctx)
	if err != nil && logger != nil {
		logger.Warn("[meetings] Discord bot token unreadable; Discord is unavailable", "error", err)
	}
	return quorumproc.Options{
		Binary:        m.QuorumBinary(),
		Listen:        m.QuorumListen(),
		VexaURL:       m.VexaURL(),
		VexaAPIKey:    vexaKey,
		DiscordToken:  discordToken,
		TranscribeURL: m.TranscribeURLOrEmpty(),
		Logger:        logger,
	}
}

// wireMeetings starts the Quorum child supervisor when meetings are enabled
// (hivecommons/hive#18289). With the feature off (the default) nothing is
// constructed and no process starts. It returns the running supervisor, or
// nil; the caller registers Stop for shutdown.
func wireMeetings(ctx context.Context, cfg *config.Config, logger *slog.Logger) quorumSupervisor {
	if cfg == nil || !cfg.Meetings.IsEnabled() {
		return nil
	}
	if bin := cfg.Meetings.QuorumBinary(); filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			if logger != nil {
				logger.Error("[meetings] quorum bundle missing; meetings unavailable (this image was built without QUORUM_VERSION, or meetings.quorum.binary is wrong)",
					"binary", bin, "error", err)
			}
			return nil
		}
	}
	sup, err := newQuorumSupervisor(meetingsQuorumOptions(ctx, cfg.Meetings, logger))
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] quorum supervisor not created; meetings unavailable", "error", err)
		}
		return nil
	}
	if err := sup.Start(ctx); err != nil {
		if logger != nil {
			logger.Error("[meetings] quorum supervisor did not start; meetings unavailable", "error", err)
		}
		sup.Stop()
		return nil
	}
	if logger != nil {
		logger.Info("[meetings] quorum supervisor started",
			"binary", cfg.Meetings.QuorumBinary(),
			"listen", cfg.Meetings.QuorumListen())
	}
	return sup
}

// wireMeetingsManager builds the meeting manager over a running Quorum
// supervisor (hivecommons/hive#18290) and re-attaches to meetings that were
// live before a restart. It returns nil (logged) when the store cannot be
// opened.
func wireMeetingsManager(cfg *config.Config, sup quorumSupervisor, logger *slog.Logger) *meetings.Manager {
	if cfg == nil || sup == nil {
		return nil
	}
	store, err := meetings.NewStore(cfg.Meetings.DataDirOrDefault())
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] meeting store unavailable; meetings unavailable", "error", err)
		}
		return nil
	}
	mgr, err := meetings.NewManager(meetings.Options{
		Store:        store,
		Quorum:       meetings.NewClient(sup.BaseURL(), sup.Token(), nil),
		BotName:      cfg.Meetings.BotNameOrDefault(),
		ConsentText:  cfg.Meetings.ConsentTextOrDefault(),
		LobbyTimeout: time.Duration(cfg.Meetings.LobbyTimeoutSOrDefault()) * time.Second,
		Logger:       logger,
		// Vexa fixes speaking ability at join time, so with voice-on-address
		// allowed every bot starts able to speak; the meeting's voice toggle
		// still decides whether hive does.
		VoiceOnAddress: cfg.Meetings.VoiceOnAddressEnabled(),
	})
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] meeting manager not created", "error", err)
		}
		return nil
	}
	// Discord `/hive join` requests arrive on the all-meetings stream. No
	// Discord identity is mapped to the hive owner yet, so none is approved
	// automatically: each waits for the owner in the dashboard.
	mgr.StartRequests()
	if n := mgr.Resume(); n > 0 && logger != nil {
		logger.Info("[meetings] re-attached to meetings", "count", n)
	}
	return mgr
}
