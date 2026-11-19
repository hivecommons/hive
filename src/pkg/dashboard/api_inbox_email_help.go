package dashboard

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Help next to a recent mail (hivecommons/hive#18744). Hive sends no mail,
// so a sender learns of a refusal from the bounce their own provider
// writes, and the hive's people learn of it here: each refused or held mail
// of GET /api/inbox/email/recent carries how to try again, the docs section
// that explains the outcome, and a prefilled new-issue link.

// mailEventView is a recent-mail event as the dashboard lists it.
type mailEventView struct {
	inbox.MailEvent
	// HowToFix says how to try again; HelpURL is the docs section for the
	// reason. Both are empty for a received mail.
	HowToFix string `json:"how_to_fix,omitempty"`
	HelpURL  string `json:"help_url,omitempty"`
	// ReportURL opens a new issue prefilled with the reason code, the day
	// and the hive version: no sender, subject or file name.
	ReportURL string `json:"report_url,omitempty"`
	// PendingItems are the mail's Inbox items that still await approval.
	PendingItems []string `json:"pending_items,omitempty"`
}

// mailHelpReason is the help topic of an event: the refusal's reason, or
// why a received mail awaits approval ("" for a mail that simply landed).
func mailHelpReason(ev inbox.MailEvent) string {
	switch ev.Outcome {
	case inbox.MailRejected:
		return ev.Reason
	case inbox.MailAwaitingApproval:
		return ev.ApprovalReason
	}
	return ""
}

// mailEventViews adds the help of each refused or held event.
func mailEventViews(src *inbox.EmailSource, evs []inbox.MailEvent) []mailEventView {
	out := make([]mailEventView, 0, len(evs))
	for _, ev := range evs {
		v := mailEventView{MailEvent: ev}
		if ev.Outcome != inbox.MailAccepted {
			reason := mailHelpReason(ev)
			v.HowToFix, v.HelpURL, v.ReportURL = wire.Fix(reason), wire.HelpURL(reason), mailReportURL(ev, reason)
		}
		if ev.Outcome == inbox.MailAwaitingApproval && src != nil {
			v.PendingItems = src.PendingItems(ev.Items)
		}
		out = append(out, v)
	}
	return out
}

// mailReportDay is the day format of a report: the time of day is left out.
const mailReportDay = "2006-01-02"

// mailReportURL builds the new-issue page for a problem with a mail, on the
// same repository and with the same labels as the feedback form's bug
// reports. The person files it as themselves. The body holds only what an
// event holds minus the sender's domain: codes, counts and the day.
func mailReportURL(ev inbox.MailEvent, reason string) string {
	what := "refused"
	if ev.Outcome == inbox.MailAwaitingApproval {
		what = "held for approval"
	}
	if reason == "" {
		reason = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A mail to a hive's email-in address was %s, and I think that is a bug.\n\n", what)
	fmt.Fprintf(&b, "- Reason code: `%s`\n", reason)
	if ev.Code > 0 {
		fmt.Fprintf(&b, "- SMTP reply code: %d\n", ev.Code)
	}
	if ev.Kind != "" {
		fmt.Fprintf(&b, "- Kind: %s\n", ev.Kind)
	}
	if len(ev.Skipped) > 0 {
		skipped := make([]string, 0, len(ev.Skipped))
		for _, sk := range ev.Skipped {
			skipped = append(skipped, "."+sk.Ext+" ("+sk.Reason+")")
		}
		fmt.Fprintf(&b, "- Attachments left out: %s\n", strings.Join(skipped, ", "))
	}
	if ev.Bytes > 0 {
		fmt.Fprintf(&b, "- Size: %d bytes\n", ev.Bytes)
	}
	if !ev.At.IsZero() {
		fmt.Fprintf(&b, "- Date: %s (UTC)\n", ev.At.UTC().Format(mailReportDay))
	}
	fmt.Fprintf(&b, "- Hive version: %s\n", nonEmpty(versionShort, versionHash))
	fmt.Fprintf(&b, "- What the reply means: %s\n", wire.HelpURL(reason))
	b.WriteString("\nWhat I sent and what I expected:\n\n\n")
	b.WriteString("Please do not paste the sender's address, the subject or the file.\n")

	q := url.Values{}
	q.Set("title", "Mail-in: a mail was "+what+" ("+reason+")")
	q.Set("body", b.String())
	q.Set("labels", strings.Join(feedbackLabels(feedbackReportRequest{RequestType: feedbackTypeBug}), ","))
	return feedbackPrefillBaseHive + "?" + q.Encode()
}
