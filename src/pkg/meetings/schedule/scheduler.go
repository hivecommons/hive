package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
)

const (
	// DefaultTick is how often the scheduler looks for a meeting to join.
	DefaultTick = 15 * time.Second
	// DefaultLead is how long before its start hive joins a meeting.
	DefaultLead = 60 * time.Second
	// DefaultJoinTimeout bounds one join call.
	DefaultJoinTimeout = 60 * time.Second
	// DefaultRetention is how long a finished event stays in the schedule.
	DefaultRetention = 30 * 24 * time.Hour
	// RecentWindow is how far back List reports finished events.
	RecentWindow = 14 * 24 * time.Hour
	// nextOccurrencesShown is how many upcoming starts an event lists.
	nextOccurrencesShown = 3

	// EnvTickS overrides DefaultTick, in seconds.
	EnvTickS = "HIVE_MEETINGS_SCHEDULE_TICK_S"
	// EnvLeadS overrides DefaultLead, in seconds.
	EnvLeadS = "HIVE_MEETINGS_SCHEDULE_LEAD_S"

	// Reasons recorded on an event or occurrence.
	ReasonMissed          = "missed: the meeting ended before hive could join"
	ReasonInterrupted     = "join interrupted: hive restarted while joining"
	ReasonOwnerCancel     = "cancelled from the dashboard"
	ReasonOrganizerCancel = "cancelled by the organizer"
)

// Joiner is the slice of meetings.Manager the scheduler uses.
type Joiner interface {
	Join(ctx context.Context, req meetings.JoinRequest) (meetings.Meeting, error)
	Get(id string) (meetings.Meeting, error)
}

// Approver decides whether an incoming event waits for the owner.
type Approver interface {
	NeedsApproval(ev Event) bool
}

// AllowlistApprover is the phase C2 policy (#18395): an owner's own manual
// add and an email invite marked Trusted (authenticated, allowlisted
// organizer) are scheduled at once; everything else waits for the owner.
type AllowlistApprover struct{}

// NeedsApproval implements Approver.
func (AllowlistApprover) NeedsApproval(ev Event) bool {
	switch ev.Source {
	case SourceManual:
		return !ev.InvitedByOwner
	case SourceEmail:
		return !ev.Trusted
	}
	return true
}

// OwnerApprover is the phase C1 policy: an owner's own manual add is
// approved; everything else (manual adds by other users, invites) waits for
// the owner. Phase C2 replaces it with the invite allowlist.
type OwnerApprover struct{}

// NeedsApproval implements Approver.
func (OwnerApprover) NeedsApproval(ev Event) bool {
	return ev.Source != SourceManual || !ev.InvitedByOwner
}

// Options configures a Scheduler.
type Options struct {
	Store    *Store
	Joiner   Joiner
	Approver Approver
	// Lead is how long before start hive joins (DefaultLead).
	Lead time.Duration
	// Tick is the scan interval (DefaultTick).
	Tick time.Duration
	// LateJoin is how long after start hive still joins a meeting with no
	// end time (DefaultLateJoin).
	LateJoin time.Duration
	// JoinTimeout bounds one join call (DefaultJoinTimeout).
	JoinTimeout time.Duration
	// Retention is how long finished events are kept (DefaultRetention).
	Retention time.Duration
	Logger    *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// DurationsFromEnv returns the tick and lead time from HIVE_MEETINGS_SCHEDULE_TICK_S
// and HIVE_MEETINGS_SCHEDULE_LEAD_S, falling back to the defaults for a
// missing or invalid value. A zero lead is allowed (join at start).
func DurationsFromEnv(getenv func(string) string) (tick, lead time.Duration) {
	tick, lead = DefaultTick, DefaultLead
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(EnvTickS))); err == nil && n > 0 {
		tick = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(EnvLeadS))); err == nil && n >= 0 {
		lead = time.Duration(n) * time.Second
	}
	return tick, lead
}

// Scheduler joins scheduled meetings. Safe for concurrent use.
type Scheduler struct {
	opts Options
	log  *slog.Logger

	// tickMu serialises scans so two never join the same occurrence.
	tickMu sync.Mutex
	poke   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  sync.Once
	stop   sync.Once
}

// New builds a scheduler. Call Start to run it and Close on shutdown.
func New(opts Options) (*Scheduler, error) {
	if opts.Store == nil || opts.Joiner == nil {
		return nil, errors.New("schedule: store and joiner are required")
	}
	if opts.Approver == nil {
		opts.Approver = OwnerApprover{}
	}
	if opts.Lead < 0 {
		opts.Lead = DefaultLead
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	if opts.LateJoin <= 0 {
		opts.LateJoin = DefaultLateJoin
	}
	if opts.JoinTimeout <= 0 {
		opts.JoinTimeout = DefaultJoinTimeout
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{opts: opts, log: log, poke: make(chan struct{}, 1), ctx: ctx, cancel: cancel}, nil
}

// Start runs the scan loop: once at once (catching up after a restart),
// then every Tick and whenever an approval makes an event joinable.
func (s *Scheduler) Start() {
	s.start.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(s.opts.Tick)
			defer t.Stop()
			for {
				s.Tick(s.ctx)
				select {
				case <-s.ctx.Done():
					return
				case <-t.C:
				case <-s.poke:
				}
			}
		}()
	})
}

// Close stops the loop and waits for it.
func (s *Scheduler) Close() {
	s.stop.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

func (s *Scheduler) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Ingest records an event from any source; the email phase calls it with a
// parsed invite. A METHOD:CANCEL marks the event cancelled. Otherwise the
// SEQUENCE rule applies (a higher SEQUENCE replaces the stored event; equal
// or lower returns it with ErrStaleSequence), and the Approver decides
// whether a new or changed event is scheduled or awaits approval. An event
// the owner declined stays declined.
func (s *Scheduler) Ingest(ev Event) (Event, error) {
	ev, err := normalize(ev)
	if err != nil {
		return Event{}, err
	}
	if ev.Method == MethodCancel {
		changed, err := s.opts.Store.Cancel(ev.UID, ev.RecurrenceID, ev.Sequence, ReasonOrganizerCancel)
		if err != nil {
			return Event{}, err
		}
		got, gerr := s.opts.Store.Get(ev.ID)
		if gerr != nil && len(changed) > 0 {
			got = changed[0]
		}
		return got, nil
	}
	needsApproval := s.opts.Approver.NeedsApproval(ev)
	stored, err := s.opts.Store.Upsert(ev, func(old *Event, in Event) Event {
		next := in
		next.Status, next.StatusReason, next.ApprovedBy = StatusScheduled, "", ""
		if needsApproval {
			next.Status = StatusAwaitingApproval
		}
		if old == nil {
			return next
		}
		next.CreatedAt = old.CreatedAt
		next.Skipped, next.JoinedOccurrence, next.MeetingID, next.History = old.Skipped, old.JoinedOccurrence, old.MeetingID, old.History
		switch {
		case old.Status == StatusDeclined:
			next.Status, next.StatusReason, next.ApprovedBy = old.Status, old.StatusReason, old.ApprovedBy
		case old.Status == StatusLive:
			next.Status = StatusLive
		case old.ApprovedBy != "" && needsApproval && sameInviter(*old, in):
			// The owner already approved this inviter's meeting; a time
			// change from the same organizer keeps that approval.
			next.Status, next.ApprovedBy = StatusScheduled, old.ApprovedBy
		}
		return next
	})
	if err != nil {
		return stored, err
	}
	s.wake()
	return s.withNext(stored), nil
}

func sameInviter(a, b Event) bool {
	if a.Organizer != nil && b.Organizer != nil && a.Organizer.Email != "" {
		return strings.EqualFold(a.Organizer.Email, b.Organizer.Email)
	}
	return a.InvitedBy != "" && a.InvitedBy == b.InvitedBy
}

// ManualRequest is a meeting added from the dashboard.
type ManualRequest struct {
	URL   string
	Title string
	Start time.Time
	// End is optional.
	End time.Time
	// RRule is an optional RFC 5545 recurrence rule (FREQ=WEEKLY;BYDAY=MO)
	// expanded in TZID.
	RRule string
	TZID  string
	// InvitedBy is the dashboard user; ByOwner is true for the owner.
	InvitedBy string
	ByOwner   bool
}

// AddManual schedules a meeting added from the dashboard. It must not have
// ended already.
func (s *Scheduler) AddManual(req ManualRequest) (Event, error) {
	if strings.TrimSpace(req.URL) == "" {
		return Event{}, fmt.Errorf("%w: a meeting link is required", ErrInvalid)
	}
	ev := Event{
		UID: newManualUID(), Method: MethodPublish, Summary: req.Title, JoinURL: req.URL,
		Start: req.Start, End: req.End, RRule: req.RRule, TZID: req.TZID,
		Source: SourceManual, InvitedBy: req.InvitedBy, InvitedByOwner: req.ByOwner,
	}
	n, err := normalize(ev)
	if err != nil {
		return Event{}, err
	}
	if len(occurrences(n, s.opts.Now(), n.duration(s.opts.LateJoin), 1, nil)) == 0 {
		return Event{}, fmt.Errorf("%w: that meeting has already ended", ErrInvalid)
	}
	return s.Ingest(ev)
}

// List returns every event with its next occurrences, by start time.
func (s *Scheduler) List() []Event {
	evs := s.opts.Store.List()
	for i := range evs {
		evs[i] = s.withNext(evs[i])
	}
	return evs
}

// Get returns one event.
func (s *Scheduler) Get(id string) (Event, error) {
	e, err := s.opts.Store.Get(id)
	if err != nil {
		return e, err
	}
	return s.withNext(e), nil
}

func (s *Scheduler) withNext(e Event) Event {
	e.NextOccurrences = nil
	if e.Status.Terminal() {
		return e
	}
	now := s.opts.Now()
	var exclude []time.Time
	if e.Recurring() && e.RecurrenceID == nil {
		exclude = s.opts.Store.overrides(e.UID)
	}
	for _, t := range occurrences(e, now, e.duration(s.opts.LateJoin), nextOccurrencesShown+1, exclude) {
		if e.JoinedOccurrence != nil && t.Equal(*e.JoinedOccurrence) {
			continue
		}
		if len(e.NextOccurrences) < nextOccurrencesShown {
			e.NextOccurrences = append(e.NextOccurrences, t)
		}
	}
	return e
}

// Approve schedules an event awaiting approval.
func (s *Scheduler) Approve(id, by string) (Event, error) {
	e, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.Status != StatusAwaitingApproval {
			return fmt.Errorf("%w: %s is %s", ErrConflict, id, e.Status)
		}
		e.Status, e.StatusReason, e.ApprovedBy = StatusScheduled, "", by
		return nil
	})
	if err == nil {
		s.wake()
	}
	return s.withNext(e), err
}

// Decline refuses an event awaiting approval; later updates of it stay
// declined.
func (s *Scheduler) Decline(id, by string) (Event, error) {
	e, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.Status != StatusAwaitingApproval {
			return fmt.Errorf("%w: %s is %s", ErrConflict, id, e.Status)
		}
		e.Status, e.StatusReason, e.ApprovedBy = StatusDeclined, "declined by "+by, ""
		return nil
	})
	return s.withNext(e), err
}

// Cancel stops hive joining an event (every occurrence of a series). A
// meeting already live is not left; leave it from the Meetings section.
func (s *Scheduler) Cancel(id, by string) (Event, error) {
	e, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.Status == StatusCancelled {
			return fmt.Errorf("%w: %s is already cancelled", ErrConflict, id)
		}
		e.Status, e.StatusReason = StatusCancelled, ReasonOwnerCancel
		if by != "" {
			e.StatusReason += " by " + by
		}
		return nil
	})
	return s.withNext(e), err
}

// Skip skips the next occurrence hive has not joined: for a single meeting
// the event becomes skipped; a series moves on to the following occurrence.
func (s *Scheduler) Skip(id, by string) (Event, error) {
	now := s.opts.Now()
	var exclude []time.Time
	if cur, err := s.opts.Store.Get(id); err == nil && cur.Recurring() && cur.RecurrenceID == nil {
		exclude = s.opts.Store.overrides(cur.UID)
	}
	e, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.Status.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrConflict, id, e.Status)
		}
		next := s.nextUnjoined(*e, now, exclude)
		if next == nil {
			return fmt.Errorf("%w: %s has no upcoming occurrence to skip", ErrConflict, id)
		}
		if !e.Recurring() {
			e.Status, e.StatusReason = StatusSkipped, "skipped by "+by
			return nil
		}
		e.Skipped = append(e.Skipped, *next)
		if len(e.Skipped) > maxExDates {
			e.Skipped = e.Skipped[len(e.Skipped)-maxExDates:]
		}
		return nil
	})
	return s.withNext(e), err
}

// nextUnjoined is the first occurrence not yet finished and not yet joined.
func (s *Scheduler) nextUnjoined(e Event, now time.Time, exclude []time.Time) *time.Time {
	for _, t := range occurrences(e, now, e.duration(s.opts.LateJoin), 2, exclude) {
		if e.JoinedOccurrence != nil && t.Equal(*e.JoinedOccurrence) {
			continue
		}
		return &t
	}
	return nil
}

// statusOf maps a meeting state onto an occurrence status.
func statusOf(st meetings.State) Status {
	switch st {
	case meetings.StateEnded, meetings.StateHandedOff, meetings.StatePurged:
		return StatusEnded
	case meetings.StateFailed:
		return StatusFailed
	case meetings.StateBlocked:
		return StatusBlocked
	}
	return StatusLive
}

// OnMeeting records a meeting's state on the event that created it. Wire
// it to meetings.Manager.Subscribe; Tick also reconciles live events.
func (s *Scheduler) OnMeeting(m meetings.Meeting) {
	if m.ID == "" {
		return
	}
	for _, e := range s.opts.Store.List() {
		if e.MeetingID != m.ID {
			continue
		}
		s.applyMeeting(e.ID, m)
		return
	}
}

func (s *Scheduler) applyMeeting(id string, m meetings.Meeting) {
	st := statusOf(m.State)
	now := s.opts.Now()
	var exclude []time.Time
	if cur, err := s.opts.Store.Get(id); err == nil && cur.Recurring() && cur.RecurrenceID == nil {
		exclude = s.opts.Store.overrides(cur.UID)
	}
	_, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.MeetingID != m.ID {
			return errUnchanged
		}
		changed := false
		for i := range e.History {
			h := &e.History[i]
			if h.MeetingID == m.ID && (h.Status != st || h.Reason != m.StateReason) {
				h.Status, h.Reason, changed = st, m.StateReason, true
			}
		}
		want := s.seriesStatus(*e, st, now, exclude)
		if e.Status != StatusCancelled && e.Status != want {
			e.Status, changed = want, true
			e.StatusReason = ""
			if st != StatusLive && want == st {
				e.StatusReason = m.StateReason
			}
		}
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		s.log.Warn("[schedule] recording meeting state failed", "event", id, "meeting", m.ID, "error", err)
	}
}

// errUnchanged aborts a store update that would change nothing.
var errUnchanged = errors.New("schedule: unchanged")

// seriesStatus is an event's status once its joined occurrence is in st: a
// series with occurrences still to come goes back to scheduled.
func (s *Scheduler) seriesStatus(e Event, st Status, now time.Time, exclude []time.Time) Status {
	if st == StatusLive || !e.Recurring() {
		return st
	}
	if s.nextUnjoined(e, now, exclude) != nil {
		return StatusScheduled
	}
	return st
}

// Tick runs one scan: reconcile live events with their meetings, join
// every due occurrence, retire events whose last occurrence passed, prune
// old finished events. It returns how many joins it started.
func (s *Scheduler) Tick(ctx context.Context) int {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	now := s.opts.Now()
	joins := 0
	for _, e := range s.opts.Store.List() {
		if ctx.Err() != nil {
			return joins
		}
		if e.Status == StatusLive {
			s.reconcile(e)
			continue
		}
		if e.Status != StatusScheduled && e.Status != StatusAwaitingApproval {
			continue
		}
		var exclude []time.Time
		if e.Recurring() && e.RecurrenceID == nil {
			exclude = s.opts.Store.overrides(e.UID)
		}
		next := s.nextUnjoined(e, now, exclude)
		if next == nil {
			s.retire(e)
			continue
		}
		if e.Status != StatusScheduled || now.Before(next.Add(-s.opts.Lead)) {
			continue
		}
		if s.join(ctx, e.ID, *next) {
			joins++
		}
	}
	if _, err := s.opts.Store.Prune(now.Add(-s.opts.Retention)); err != nil {
		s.log.Warn("[schedule] pruning finished events failed", "error", err)
	}
	return joins
}

// retire closes an event with no occurrence left: a series that ran ends,
// anything never joined is skipped as missed.
func (s *Scheduler) retire(e Event) {
	_, err := s.opts.Store.Update(e.ID, func(e *Event) error {
		if e.Status != StatusScheduled && e.Status != StatusAwaitingApproval {
			return errUnchanged
		}
		if len(e.History) > 0 {
			last := e.History[len(e.History)-1]
			e.Status, e.StatusReason = last.Status, last.Reason
			if e.Status == StatusLive {
				e.Status = StatusEnded
			}
			return nil
		}
		e.Status, e.StatusReason = StatusSkipped, ReasonMissed
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		s.log.Warn("[schedule] retiring event failed", "event", e.ID, "error", err)
	}
}

// reconcile brings a live event in line with its meeting: a state change
// missed while hive was down, or a join a restart interrupted.
func (s *Scheduler) reconcile(e Event) {
	if e.MeetingID == "" {
		// The occurrence was claimed but the join never recorded a meeting:
		// hive restarted mid-join. Never join it again; record the loss.
		_, err := s.opts.Store.Update(e.ID, func(e *Event) error {
			if e.Status != StatusLive || e.MeetingID != "" {
				return errUnchanged
			}
			for i := range e.History {
				h := &e.History[i]
				if h.MeetingID == "" && h.Status == StatusLive {
					h.Status, h.Reason = StatusFailed, ReasonInterrupted
				}
			}
			e.Status, e.StatusReason = StatusFailed, ReasonInterrupted
			if e.Recurring() {
				e.Status = StatusScheduled
			}
			return nil
		})
		if err != nil && !errors.Is(err, errUnchanged) {
			s.log.Warn("[schedule] recording an interrupted join failed", "event", e.ID, "error", err)
		}
		return
	}
	m, err := s.opts.Joiner.Get(e.MeetingID)
	if err != nil {
		if errors.Is(err, meetings.ErrNotFound) {
			s.applyMeeting(e.ID, meetings.Meeting{ID: e.MeetingID, State: meetings.StateEnded, StateReason: "meeting record removed"})
		}
		return
	}
	s.applyMeeting(e.ID, m)
}

// join claims the occurrence (persisted before the join call, so a restart
// can never join it twice), sends hive in, and links the meeting back.
func (s *Scheduler) join(ctx context.Context, id string, occ time.Time) bool {
	claimed, err := s.opts.Store.Update(id, func(e *Event) error {
		if e.Status != StatusScheduled || (e.JoinedOccurrence != nil && e.JoinedOccurrence.Equal(occ)) {
			return errUnchanged
		}
		o := occ
		e.JoinedOccurrence, e.MeetingID = &o, ""
		e.Status, e.StatusReason = StatusLive, ""
		e.History = append(e.History, Occurrence{Start: occ, Status: StatusLive})
		if len(e.History) > maxHistory {
			e.History = e.History[len(e.History)-maxHistory:]
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, errUnchanged) {
			s.log.Warn("[schedule] claiming an occurrence failed; not joining", "event", id, "error", err)
		}
		return false
	}
	jctx, cancel := context.WithTimeout(ctx, s.opts.JoinTimeout)
	m, jerr := s.opts.Joiner.Join(jctx, claimed.joinRequest())
	cancel()
	st, reason := statusOf(m.State), ""
	if jerr != nil {
		reason = jerr.Error()
		if st == StatusLive {
			st = StatusFailed
		}
		s.log.Warn("[schedule] joining a scheduled meeting failed", "event", id, "error", jerr)
	} else {
		s.log.Info("[schedule] joined a scheduled meeting", "event", id, "meeting", m.ID, "platform", claimed.Platform)
	}
	var exclude []time.Time
	if claimed.Recurring() && claimed.RecurrenceID == nil {
		exclude = s.opts.Store.overrides(claimed.UID)
	}
	now := s.opts.Now()
	_, err = s.opts.Store.Update(id, func(e *Event) error {
		if e.JoinedOccurrence == nil || !e.JoinedOccurrence.Equal(occ) {
			return errUnchanged
		}
		e.MeetingID = m.ID
		if n := len(e.History); n > 0 && e.History[n-1].Start.Equal(occ) {
			e.History[n-1].MeetingID, e.History[n-1].Status, e.History[n-1].Reason = m.ID, st, reason
		}
		if e.Status == StatusLive {
			e.Status, e.StatusReason = s.seriesStatus(*e, st, now, exclude), reason
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		s.log.Warn("[schedule] linking the meeting failed", "event", id, "meeting", m.ID, "error", err)
	}
	return jerr == nil
}
