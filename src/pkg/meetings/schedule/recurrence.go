package schedule

import (
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
)

const (
	// maxRecurrenceScan bounds how many occurrences one lookup walks, so a
	// pathological rule cannot spin the scheduler (twenty years of a daily
	// meeting fits).
	maxRecurrenceScan = 20000
	// DefaultLateJoin is how long after its start hive still joins a
	// meeting whose invite has no end time.
	DefaultLateJoin = 15 * time.Minute
)

// allowedFreq are the recurrence frequencies a meeting may use: anything
// finer than daily would send hive into a meeting every hour or minute.
var allowedFreq = map[rrule.Frequency]bool{
	rrule.DAILY: true, rrule.WEEKLY: true, rrule.MONTHLY: true, rrule.YEARLY: true,
}

// location is the zone a recurrence expands in.
func (e Event) location() *time.Location {
	if e.TZID != "" {
		if loc, err := time.LoadLocation(e.TZID); err == nil {
			return loc
		}
	}
	return time.UTC
}

// ruleSet builds the recurrence set of a series: its RRULE from Start in
// the event's zone, minus EXDATEs.
func ruleSet(e Event) (*rrule.Set, error) {
	loc := e.location()
	opt, err := rrule.StrToROptionInLocation(e.RRule, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: recurrence rule: %v", ErrInvalid, err)
	}
	if !allowedFreq[opt.Freq] {
		return nil, fmt.Errorf("%w: recurrence must be daily, weekly, monthly or yearly", ErrInvalid)
	}
	if len(opt.Byhour) > 1 || len(opt.Byminute) > 1 || len(opt.Bysecond) > 1 {
		return nil, fmt.Errorf("%w: recurrence may not repeat within a day", ErrInvalid)
	}
	opt.Dtstart = e.Start.In(loc)
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("%w: recurrence rule: %v", ErrInvalid, err)
	}
	set := &rrule.Set{}
	set.RRule(r)
	for _, ex := range e.ExDates {
		set.ExDate(ex)
	}
	return set, nil
}

// duration is how long an occurrence lasts for joining purposes: until its
// end, or DefaultLateJoin (the scheduler's late-join window) without one.
func (e Event) duration(lateJoin time.Duration) time.Duration {
	if !e.End.IsZero() && e.End.After(e.Start) {
		return e.End.Sub(e.Start)
	}
	return lateJoin
}

func containsTime(ts []time.Time, t time.Time) bool {
	for _, x := range ts {
		if x.Equal(t) {
			return true
		}
	}
	return false
}

// occurrences returns up to n occurrence starts (UTC) of e that have not
// finished by from (start + duration > from), skipping EXDATEs, skipped
// occurrences and those in exclude (occurrences overridden by their own
// RECURRENCE-ID event). A single event has at most one: its Start.
func occurrences(e Event, from time.Time, dur time.Duration, n int, exclude []time.Time) []time.Time {
	if n <= 0 {
		return nil
	}
	keep := func(t time.Time) bool {
		return t.Add(dur).After(from) && !containsTime(e.Skipped, t) && !containsTime(exclude, t)
	}
	if !e.Recurring() {
		if keep(e.Start) && !containsTime(e.ExDates, e.Start) {
			return []time.Time{e.Start}
		}
		return nil
	}
	set, err := ruleSet(e)
	if err != nil {
		return nil
	}
	next := set.Iterator()
	var out []time.Time
	for i := 0; i < maxRecurrenceScan && len(out) < n; i++ {
		t, ok := next()
		if !ok {
			break
		}
		t = t.UTC()
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}
