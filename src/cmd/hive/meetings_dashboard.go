package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

// meetingsDashboard implements dashboard.MeetingsService over the meeting
// manager and the live loop (hivecommons/hive#18292). It lives in cmd/hive
// because pkg/dashboard never imports pkg/meetings.
type meetingsDashboard struct {
	mgr  *meetings.Manager
	live *live.Runner
	// sched, when the Calendar runs, forgets a deleted meeting's entry.
	sched *schedule.Scheduler
}

// meetingsDashboardError maps meetings errors onto the dashboard's
// sentinels so the API answers 404/400/409 instead of 502.
func meetingsDashboardError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, meetings.ErrNotFound), errors.Is(err, live.ErrUnknownResearch):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingNotFound, err)
	case errors.Is(err, meetings.ErrInvalidRequest), errors.Is(err, meetings.ErrTrustInvalid), errors.Is(err, meetings.ErrTrustLimit), meetings.IsCode(err, meetings.CodeInvalidArgument):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingInvalid, err)
	case errors.Is(err, live.ErrNotPending), errors.Is(err, live.ErrSessionEnded), errors.Is(err, meetings.ErrNotLive), errors.Is(err, meetings.ErrNotRequested),
		errors.Is(err, meetings.ErrStillRunning):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingConflict, err)
	default:
		return err
	}
}

func (d *meetingsDashboard) Join(ctx context.Context, req dashboard.MeetingJoinRequest, invitedBy string) (any, error) {
	m, err := d.mgr.Join(ctx, meetings.JoinRequest{
		URL: req.URL, Platform: req.Platform, GuildID: req.GuildID, ChannelID: req.ChannelID,
		Title: req.Title, Voice: req.Voice, InvitedBy: invitedBy,
	})
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

func (d *meetingsDashboard) List() (any, error) {
	list, err := d.mgr.List()
	if list == nil {
		list = []meetings.Meeting{}
	}
	return list, meetingsDashboardError(err)
}

func (d *meetingsDashboard) Get(id string) (any, error) {
	m, err := d.mgr.Get(id)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

func (d *meetingsDashboard) Leave(ctx context.Context, id string) (any, error) {
	m, err := d.mgr.Leave(ctx, id, meetings.ReasonOwnerLeave)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

func (d *meetingsDashboard) SetVoice(id string, enabled bool) (any, error) {
	m, err := d.mgr.SetVoice(id, enabled)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

func (d *meetingsDashboard) ApproveResearch(id, rid string) error {
	if d.live == nil {
		return meetingsDashboardError(live.ErrSessionEnded)
	}
	return meetingsDashboardError(d.live.Approve(id, rid))
}

func (d *meetingsDashboard) ApproveRequest(ctx context.Context, id string, voice bool) (any, error) {
	m, err := d.mgr.ApproveRequest(ctx, id, voice)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

// ApproveAndTrust, TrustedRequesters and Untrust implement
// dashboard.MeetingsTrustService.
func (d *meetingsDashboard) ApproveAndTrust(ctx context.Context, id string, voice bool, by string) (any, string, error) {
	m, t, err := d.mgr.ApproveAndTrust(ctx, id, voice, by)
	if err != nil {
		return nil, "", meetingsDashboardError(err)
	}
	return m, t.ID, nil
}

func (d *meetingsDashboard) TrustedRequesters() (any, error) {
	list, err := d.mgr.TrustedRequesters()
	if list == nil {
		list = []meetings.Trusted{}
	}
	return list, err
}

func (d *meetingsDashboard) Untrust(platform, id string) (bool, error) {
	return d.mgr.Untrust(platform, id)
}

func (d *meetingsDashboard) DeclineRequest(ctx context.Context, id string) (any, error) {
	m, err := d.mgr.DeclineRequest(ctx, id)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	return m, nil
}

// meetingDeleted is the dashboard's view of a deleted meeting; with the
// Calendar running, the entry that existed only for it goes too.
func meetingDeleted(res meetings.DeleteResult, sched *schedule.Scheduler) dashboard.MeetingDeleted {
	out := dashboard.MeetingDeleted{ID: res.Meeting.ID, State: string(res.Meeting.State), UpstreamError: res.UpstreamError}
	if sched != nil {
		out.CalendarRemoved = sched.ForgetMeeting(res.Meeting.ID)
	}
	return out
}

func (d *meetingsDashboard) Delete(ctx context.Context, id string) (dashboard.MeetingDeleted, error) {
	res, err := d.mgr.Delete(ctx, id)
	if err != nil {
		return dashboard.MeetingDeleted{}, meetingsDashboardError(err)
	}
	return meetingDeleted(res, d.sched), nil
}

func (d *meetingsDashboard) ClearFinished(ctx context.Context, before time.Time) ([]dashboard.MeetingDeleted, error) {
	list, err := d.mgr.ClearFinished(ctx, before)
	out := make([]dashboard.MeetingDeleted, 0, len(list))
	for _, res := range list {
		out = append(out, meetingDeleted(res, d.sched))
	}
	return out, meetingsDashboardError(err)
}

// meetingLive is the panel snapshot.
type meetingLive struct {
	Meeting    meetings.Meeting `json:"meeting"`
	Transcript []meetings.Event `json:"transcript"`
	Live       *live.State      `json:"live,omitempty"`
	// Budget is the meeting's budget meter (numbers only).
	Budget *live.BudgetView `json:"budget,omitempty"`
}

func (d *meetingsDashboard) Live(id string) (any, error) {
	m, err := d.mgr.Get(id)
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	tr, err := d.mgr.Transcript(id)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		tr = []meetings.Event{}
	}
	out := meetingLive{Meeting: m, Transcript: tr}
	if d.live != nil {
		if st, err := d.live.State(id); err == nil {
			out.Live = &st
			bv := live.NewBudgetView(d.live.Policy(), st, id)
			out.Budget = &bv
		}
	}
	return out, nil
}

// Budget is the hive's meetings budget: the per-meeting limits, the
// hourly and daily ceilings used, and every meeting's meter. It carries
// numbers only, so the dashboard serves it to any role.
func (d *meetingsDashboard) Budget() (any, error) {
	if d.live == nil {
		return live.BudgetOverview{Meetings: []live.BudgetView{}}, nil
	}
	list, err := d.mgr.List()
	if err != nil {
		return nil, meetingsDashboardError(err)
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	return d.live.NewBudgetOverview(ids, func(id string) (live.State, bool) {
		st, err := d.live.State(id)
		return st, err == nil
	}), nil
}

// Subscribe forwards meeting changes, transcript lines and live-loop
// events as panel frames.
func (d *meetingsDashboard) Subscribe(fn func(dashboard.MeetingFrame)) func() {
	unsubMgr := d.mgr.Subscribe(func(u meetings.Update) {
		fn(dashboard.MeetingFrame{MeetingID: u.Meeting.ID, Kind: dashboard.MeetingFrameMeeting, Data: u.Meeting})
		if u.Event != nil && (u.Event.Type == meetings.EventSegment || u.Event.Type == meetings.EventChat) {
			fn(dashboard.MeetingFrame{MeetingID: u.Meeting.ID, Kind: "line", Data: u.Event})
		}
	})
	unsubLive := func() {}
	if d.live != nil {
		unsubLive = d.live.Observe(func(ev live.Event) {
			fn(dashboard.MeetingFrame{MeetingID: ev.MeetingID, Kind: ev.Kind, Data: ev})
			if ev.Line != nil {
				// One of the bot's own lines: it joins the transcript the
				// panel shows, like the people's lines.
				fn(dashboard.MeetingFrame{MeetingID: ev.MeetingID, Kind: "line", Data: ev.Line})
			}
		})
	}
	return func() {
		unsubMgr()
		unsubLive()
	}
}
