package dashboard

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/inbox"
)

// mailAttendeeEmail feeds Settings, Meeting attendee (#18396) its Email
// section from the email-in state (#18395): the proven and pending calendar
// emails per user, the mail-in files waiting for the owner, and the
// meetings.email switches. Invites waiting for approval stay in the
// Calendar, where they are approved.
type mailAttendeeEmail struct{ s *Server }

// Calendar email proof states the tab shows.
const (
	mailProofVerified = "verified"
	mailProofPending  = "pending"
	// mailApprovalKindFile is the kind of a mail-in file in the queue.
	mailApprovalKindFile = "file"
)

// Snapshot implements MeetingsAttendeeEmail.
func (m mailAttendeeEmail) Snapshot(ctx context.Context) (MeetingsAttendeeEmailSnapshot, error) {
	snap := MeetingsAttendeeEmailSnapshot{Emails: map[string][]MeetingsCalendarEmail{}, Approvals: []MeetingsEmailApproval{}}
	if st := m.s.mailAllowlist(); st != nil {
		for _, e := range st.All() {
			ce := MeetingsCalendarEmail{Address: e.Email, Proof: mailProofPending}
			if e.Proven() {
				ce.Proof, ce.VerifiedAt = mailProofVerified, e.ProvenAt.UTC().Format(time.RFC3339)
			}
			snap.Emails[e.Login] = append(snap.Emails[e.Login], ce)
		}
	}
	if m.s.deps != nil && m.s.deps.Inbox != nil {
		if src := m.s.deps.Inbox.Email(); src != nil {
			items, err := src.List(ctx)
			if err != nil {
				return snap, err
			}
			for _, it := range items {
				if !it.AwaitingApproval {
					continue
				}
				snap.Approvals = append(snap.Approvals, mailApproval(it))
			}
		}
	}
	em := m.s.meetingsEmailConfig()
	snap.CalendarInvites, snap.MailInFiles = em.InvitesOn(), em.FilesOn()
	return snap, nil
}

func mailApproval(it inbox.Item) MeetingsEmailApproval {
	a := MeetingsEmailApproval{ID: it.ID, Kind: mailApprovalKindFile, From: it.Sender, Subject: it.Title}
	if !it.RecordedAt.IsZero() {
		a.ReceivedAt = it.RecordedAt.UTC().Format(time.RFC3339)
	}
	return a
}

// meetingsEmailRequest is the optional email block of PUT
// /api/meetings/persona: the meetings.email switches (owner only).
type meetingsEmailRequest struct {
	CalendarInvites *bool `json:"calendar_invites,omitempty"`
	MailInFiles     *bool `json:"mail_in_files,omitempty"`
}
