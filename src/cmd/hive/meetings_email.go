package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/mailallow"
	"github.com/hivecommons/hive/pkg/mailin/wire"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

// Email in (hivecommons/hive#18395): calendar invites hive-mx received for
// this hive, relayed by the hub, become Calendar entries; the allowlist of
// proven calendar emails lives next to the Inbox state.

// mailAllowlistPath is the allowlist store under the knowledge base dir.
func mailAllowlistPath(baseDir string) string {
	return filepath.Join(baseDir, "connector-state", "email", "allowlist.json")
}

// wireMailAllowlist opens the calendar email allowlist and hands it to the
// dashboard. A failure is logged and leaves email-in approvals to the owner.
func (b *boot) wireMailAllowlist(baseDir string) *mailallow.Store {
	st, err := mailallow.Open(mailAllowlistPath(baseDir), nil)
	if err != nil {
		b.logger.Warn("calendar email allowlist unavailable; every email waits for approval", "error", err)
		return nil
	}
	if b.dashSrv != nil {
		b.dashSrv.SetMailAllowlist(st)
	}
	// The customers sweep reads proven calendar emails from it (#18742).
	b.mailAllow = st
	return st
}

// Per-invite outcomes reported back to the hub (and logged by it as a status
// code only).
const (
	inviteIgnoredNotOrganizer = "ignored: an existing meeting changes only by mail authenticated as its organizer"
	inviteIgnoredNoMeeting    = "ignored: no such meeting to cancel"
	inviteUnchanged           = "unchanged: sequence is not higher"
)

type inviteResult struct {
	UID     string `json:"uid"`
	EventID string `json:"event_id,omitempty"`
	Status  string `json:"status"`
}

func eventFromInvite(inv wire.Invite, d wire.Delivery, trusted bool) schedule.Event {
	ev := schedule.Event{
		UID: inv.UID, RecurrenceID: inv.RecurrenceID, Sequence: inv.Sequence, Method: inv.Method,
		Summary: inv.Summary, Start: inv.Start, End: inv.End, TZID: inv.TZID, RRule: inv.RRule, ExDates: inv.ExDates,
		JoinURL: inv.JoinURL, Platform: inv.Platform, NativeMeetingID: inv.NativeMeetingID, Passcode: inv.Passcode,
		Source: schedule.SourceEmail, InvitedBy: d.Auth.From, Trusted: trusted,
	}
	if inv.Organizer != nil {
		ev.Organizer = &schedule.Person{Email: inv.Organizer.Email, Name: inv.Organizer.Name}
		ev.InvitedBy = inv.Organizer.Email
	}
	for _, a := range inv.Attendees {
		ev.Attendees = append(ev.Attendees, schedule.Person{Email: a.Email, Name: a.Name})
	}
	return ev
}

// existingFor finds the stored meeting an invite would change: the exact
// occurrence, or for an override or cancel of one occurrence, its series.
func (d *meetingsSchedule) existingFor(inv wire.Invite) (schedule.Event, bool) {
	if ev, err := d.sched.Get(schedule.EventID(inv.UID, inv.RecurrenceID)); err == nil {
		return ev, true
	}
	if inv.RecurrenceID != nil {
		if ev, err := d.sched.Get(schedule.EventID(inv.UID, nil)); err == nil {
			return ev, true
		}
	}
	return schedule.Event{}, false
}

// IngestInvites implements dashboard.MeetingsInviteIngester. trusted (the
// sender is the organizer, DMARC aligned and allowlisted) schedules a new
// invite at once; otherwise it waits for the owner. An invite for a meeting
// already on the calendar, including a CANCEL, applies only when the mail is
// authenticated as that meeting's organizer, so nobody can move or cancel a
// meeting by forging its UID.
func (d *meetingsSchedule) IngestInvites(_ context.Context, del wire.Delivery, trusted bool) (any, error) {
	out := make([]inviteResult, 0, len(del.Invites))
	invalid := 0
	var lastErr error
	for _, inv := range del.Invites {
		res := inviteResult{UID: inv.UID}
		isCancel := strings.EqualFold(inv.Method, schedule.MethodCancel)
		if existing, ok := d.existingFor(inv); ok {
			if existing.Organizer == nil || !del.Auth.Aligned || !strings.EqualFold(existing.Organizer.Email, del.Auth.From) {
				res.Status = inviteIgnoredNotOrganizer
				out = append(out, res)
				continue
			}
		} else if isCancel {
			res.Status = inviteIgnoredNoMeeting
			out = append(out, res)
			continue
		}
		ev, err := d.sched.Ingest(eventFromInvite(inv, del, trusted))
		switch {
		case errors.Is(err, schedule.ErrStaleSequence):
			res.EventID, res.Status = ev.ID, inviteUnchanged
		case err != nil:
			invalid++
			lastErr = err
			res.Status = "invalid: " + err.Error()
		default:
			res.EventID, res.Status = ev.ID, string(ev.Status)
		}
		out = append(out, res)
	}
	if len(del.Invites) > 0 && invalid == len(del.Invites) {
		return nil, fmt.Errorf("%w: %v", dashboard.ErrMeetingInvalid, lastErr)
	}
	return map[string]any{"results": out}, nil
}
