package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

const (
	// defaultAttendeeName is the indicator's name when the bot display
	// name says nothing more specific than "hive".
	defaultAttendeeName = "Hive"
	// scheduleUpdateQueue buffers meeting updates for the scheduler; when
	// full an update is dropped and the scheduler's reconcile pass catches
	// up from the meeting store on its next tick.
	scheduleUpdateQueue = 64
)

// meetingsSchedule implements dashboard.MeetingsScheduleService over the
// meeting scheduler (hivecommons/hive#18394).
type meetingsSchedule struct {
	sched *schedule.Scheduler
	// mgr deletes the meetings a deleted Calendar entry created.
	mgr *meetings.Manager
	// attendee is the name the dashboard's "in a meeting" indicator uses.
	attendee string
	now      func() time.Time
}

// attendeeName derives the indicator name from the bot display name:
// "Betty Bee from the hive" is "Betty Bee", "hive (notes)" is "Hive".
func attendeeName(botName string) string {
	name := strings.TrimSpace(botName)
	if i := strings.Index(name, " ("); i >= 0 {
		name = name[:i]
	}
	if i := strings.Index(strings.ToLower(name), " from "); i > 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "hive") {
		return defaultAttendeeName
	}
	r, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r)) + name[size:]
}

func meetingsScheduleError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, schedule.ErrNotFound):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingNotFound, err)
	case errors.Is(err, schedule.ErrInvalid):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingInvalid, err)
	case errors.Is(err, schedule.ErrConflict), errors.Is(err, schedule.ErrStaleSequence):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingConflict, err)
	default:
		return err
	}
}

// meetingsScheduleList is GET /api/meetings/schedule.
type meetingsScheduleList struct {
	Upcoming     []schedule.Event `json:"upcoming"`
	Recent       []schedule.Event `json:"recent"`
	AttendeeName string           `json:"attendee_name"`
}

func (d *meetingsSchedule) List() (any, error) {
	out := meetingsScheduleList{Upcoming: []schedule.Event{}, Recent: []schedule.Event{}, AttendeeName: d.attendee}
	since := d.now().Add(-schedule.RecentWindow)
	evs := d.sched.List()
	for _, e := range evs {
		e = e.Redacted()
		if !e.Status.Terminal() {
			out.Upcoming = append(out.Upcoming, e)
		} else if e.UpdatedAt.After(since) {
			out.Recent = append(out.Recent, e)
		}
	}
	// Recent past: newest first.
	for i, j := 0, len(out.Recent)-1; i < j; i, j = i+1, j-1 {
		out.Recent[i], out.Recent[j] = out.Recent[j], out.Recent[i]
	}
	return out, nil
}

func (d *meetingsSchedule) Add(_ context.Context, req dashboard.MeetingScheduleAdd) (any, error) {
	ev, err := d.sched.AddManual(schedule.ManualRequest{
		URL: req.URL, Title: req.Title, Start: req.Start, End: req.End, RRule: req.RRule, TZID: req.Timezone,
		InvitedBy: req.AddedBy, ByOwner: req.ByOwner,
	})
	if err != nil {
		return nil, meetingsScheduleError(err)
	}
	return ev.Redacted(), nil
}

func (d *meetingsSchedule) Act(_ context.Context, eid, action, by string) (any, error) {
	var (
		ev  schedule.Event
		err error
	)
	switch action {
	case dashboard.MeetingScheduleApprove:
		ev, err = d.sched.Approve(eid, by)
	case dashboard.MeetingScheduleDecline:
		ev, err = d.sched.Decline(eid, by)
	case dashboard.MeetingScheduleCancel:
		ev, err = d.sched.Cancel(eid, by)
	case dashboard.MeetingScheduleSkip:
		ev, err = d.sched.Skip(eid, by)
	default:
		return nil, fmt.Errorf("%w: unknown action %q", dashboard.ErrMeetingInvalid, action)
	}
	if err != nil {
		return nil, meetingsScheduleError(err)
	}
	return ev.Redacted(), nil
}

// Delete removes a finished Calendar entry and the meetings it created.
// An entry still to come or live answers 409 (skip or cancel it); so does
// one whose meeting hive is still in. Meetings already gone are skipped.
func (d *meetingsSchedule) Delete(ctx context.Context, eid string) (dashboard.MeetingScheduleDeleted, error) {
	out := dashboard.MeetingScheduleDeleted{ID: eid, Meetings: []dashboard.MeetingDeleted{}}
	e, err := d.sched.Get(eid)
	if err != nil {
		return out, meetingsScheduleError(err)
	}
	if !e.Status.Terminal() {
		return out, fmt.Errorf("%w: this meeting is %s; skip or cancel it instead", dashboard.ErrMeetingConflict, e.Status)
	}
	ids := schedule.LinkedMeetings(e)
	if d.mgr != nil {
		for _, id := range ids {
			m, err := d.mgr.Get(id)
			if errors.Is(err, meetings.ErrNotFound) {
				continue
			}
			if err != nil {
				return out, err
			}
			if !meetings.Deletable(m) {
				return out, fmt.Errorf("%w: hive is still in this meeting; leave the meeting first", dashboard.ErrMeetingConflict)
			}
		}
	}
	if _, err := d.sched.Delete(eid); err != nil {
		return out, meetingsScheduleError(err)
	}
	if d.mgr == nil {
		return out, nil
	}
	for _, id := range ids {
		res, err := d.mgr.Delete(ctx, id)
		if errors.Is(err, meetings.ErrNotFound) {
			continue
		}
		if err != nil {
			out.Meetings = append(out.Meetings, dashboard.MeetingDeleted{ID: id, UpstreamError: err.Error()})
			continue
		}
		out.Meetings = append(out.Meetings, meetingDeleted(res, nil))
	}
	return out, nil
}

// wireMeetingsSchedule starts the meeting scheduler over the manager
// (hivecommons/hive#18394): schedule.json in the meetings data directory,
// joins HIVE_MEETINGS_SCHEDULE_LEAD_S before start, scans every
// HIVE_MEETINGS_SCHEDULE_TICK_S. Meeting state changes are recorded on the
// event that created them, in order, by one worker. It returns nil (logged)
// when the schedule file cannot be opened; the caller registers the
// returned stop func for shutdown.
func wireMeetingsSchedule(cfg *config.Config, mgr *meetings.Manager, logger *slog.Logger) (*meetingsSchedule, func()) {
	if cfg == nil || mgr == nil {
		return nil, nil
	}
	st, err := schedule.NewStore(cfg.Meetings.DataDirOrDefault(), nil)
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] schedule unavailable; scheduled meetings are not joined", "error", err)
		}
		return nil, nil
	}
	tick, lead := schedule.DurationsFromEnv(os.Getenv)
	sched, err := schedule.New(schedule.Options{
		Store: st, Joiner: mgr, Approver: schedule.AllowlistApprover{}, Tick: tick, Lead: lead, Logger: logger,
	})
	if err != nil {
		if logger != nil {
			logger.Error("[meetings] scheduler not created", "error", err)
		}
		return nil, nil
	}
	updates := make(chan meetings.Meeting, scheduleUpdateQueue)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case m := <-updates:
				sched.OnMeeting(m)
			case <-quit:
				return
			}
		}
	}()
	unsub := mgr.Subscribe(func(u meetings.Update) {
		select {
		case updates <- u.Meeting:
		default:
		}
	})
	sched.Start()
	if logger != nil {
		logger.Info("[meetings] scheduler started", "file", st.Path(), "tick", tick, "lead", lead)
	}
	stop := func() {
		unsub()
		close(quit)
		<-done
		sched.Close()
	}
	return &meetingsSchedule{sched: sched, mgr: mgr, attendee: attendeeName(cfg.Meetings.BotNameOrDefault()), now: time.Now}, stop
}
