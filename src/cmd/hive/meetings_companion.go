package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
)

// Companion side of meetingsDashboard (hivecommons/hive#18596): the desktop
// companion streams a person's own meeting client audio as a live meeting.

// companionInfo is GET /api/companion/info.
type companionInfo struct {
	ConsentVersion string   `json:"consent_version"`
	ConsentText    string   `json:"consent_text"`
	SampleRate     int      `json:"sample_rate"`
	Channels       []string `json:"channels"`
	MaxChunkBytes  int      `json:"max_chunk_bytes"`
}

// companionDashboardError adds the companion errors to
// meetingsDashboardError.
func companionDashboardError(err error) error {
	var gap *meetings.ChunkGapError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &gap):
		return &dashboard.CompanionGapError{Expected: gap.Expected}
	case errors.Is(err, meetings.ErrCompanionNotYours):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingForbidden, err)
	case errors.Is(err, meetings.ErrCompanionUnavailable):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingUnavailable, err)
	case errors.Is(err, meetings.ErrNotCompanion):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingInvalid, err)
	default:
		return meetingsDashboardError(err)
	}
}

func (d *meetingsDashboard) CompanionInfo() any {
	return companionInfo{
		ConsentVersion: meetings.CompanionConsentVersion,
		ConsentText:    meetings.CompanionConsentText,
		SampleRate:     meetings.CompanionSampleRate,
		Channels:       []string{meetings.CompanionChannelCall, meetings.CompanionChannelMic},
		MaxChunkBytes:  meetings.CompanionMaxChunkBytes,
	}
}

func (d *meetingsDashboard) StartCompanion(ctx context.Context, req dashboard.CompanionStartRequest, user string) (any, error) {
	m, err := d.mgr.StartCompanion(ctx, meetings.CompanionStart{
		Title: req.Title, App: req.App, HostName: req.HostName,
		ConsentVersion: req.ConsentVersion, Attested: req.ConsentAttested, User: user,
	})
	if err != nil {
		return nil, companionDashboardError(err)
	}
	return m, nil
}

func (d *meetingsDashboard) CompanionAudio(ctx context.Context, id, user string, asOwner bool, channel string, seq int, pcm []byte) (any, error) {
	res, err := d.mgr.CompanionAudio(ctx, id, user, asOwner, channel, seq, pcm)
	if err != nil {
		return nil, companionDashboardError(err)
	}
	return res, nil
}

func (d *meetingsDashboard) CompanionReplies(ctx context.Context, id, user string, asOwner bool, after int64) (any, error) {
	res, err := d.mgr.CompanionReplies(ctx, id, user, asOwner, after)
	if err != nil {
		return nil, companionDashboardError(err)
	}
	return res, nil
}

func (d *meetingsDashboard) EndCompanion(ctx context.Context, id, user string, asOwner bool) (any, error) {
	m, err := d.mgr.EndCompanion(ctx, id, user, asOwner)
	if err != nil {
		return nil, companionDashboardError(err)
	}
	return m, nil
}

var _ dashboard.CompanionMeetingsService = (*meetingsDashboard)(nil)
