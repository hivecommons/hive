package hub

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The confirmation reply for a mail sent to a hive (hivecommons/hive#18746).
// Plain text only: no HTML, no images, no tracking links. The reply states
// the outcome and echoes nothing of the incoming mail except its Message-ID
// (in In-Reply-To and References) and a sanitized subject (the Subject
// header only, never the body).

// Reply outcomes, as the spoke's answer reports them.
const (
	mailReplyReceived = "received"
	mailReplyHeld     = "held"
)

const (
	// mailReplySubjectRunes bounds the original subject in the reply's.
	mailReplySubjectRunes = 120
	// mailReplyFallbackSubject is used when the original has no usable text.
	mailReplyFallbackSubject = "your mail"
	// mailReplyIssueURL is where a sender reports a problem.
	mailReplyIssueURL = "https://github.com/hivecommons/hive/issues/new/choose"
	// mailReplyHeldDocsURL explains what a held mail means.
	mailReplyHeldDocsURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/inbox-voice-sources.md#mailing-your-hive-what-the-replies-mean"
	// mailReplyAutoNotice ends every reply.
	mailReplyAutoNotice = "This is an automatic reply. Replying to it sends a new mail to the hive."
)

// mailHostRE is a host name safe to put in a link and an address.
var mailHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// mailMsgIDRE is a Message-ID safe to put in a header: no whitespace, angle
// brackets or control characters.
var mailMsgIDRE = regexp.MustCompile(`^[\x21-\x3b\x3d\x3f-\x7e]{1,255}$`)

// mailReplyMessage is one reply, ready for a sender. Its envelope sender is
// always null (MAIL FROM:<>), so a failed reply never bounces back into a
// hive; a sender must not substitute another one.
type mailReplyMessage struct {
	// From is the fixed signing identity, Reply-To the hive's own address.
	From    string
	ReplyTo string
	To      string
	Subject string
	// MessageID is the original Message-ID, angle brackets removed.
	MessageID string
	Text      string
}

// mailHeader is one header line of a reply.
type mailHeader struct{ Key, Value string }

// Headers is every header a reply carries besides Date and Message-ID,
// which the sender adds. Safety rule 3 of #18746 lives here so no sender
// can leave one out.
func (m mailReplyMessage) Headers() []mailHeader {
	ref := "<" + m.MessageID + ">"
	return []mailHeader{
		{"From", "Hive <" + m.From + ">"},
		{"Reply-To", m.ReplyTo},
		{"To", m.To},
		{"Subject", m.Subject},
		{"In-Reply-To", ref},
		{"References", ref},
		{"Auto-Submitted", "auto-replied"},
		{"X-Auto-Response-Suppress", "All"},
		{"Precedence", "auto_reply"},
		{"MIME-Version", "1.0"},
		{"Content-Type", `text/plain; charset="utf-8"`},
	}
}

// sanitizeMailSubject makes a sender-controlled subject safe for the reply's
// Subject header: control, format (bidi, zero-width) and line separator
// characters are dropped, white space collapsed, the length capped, and a
// leading "Re:" is not doubled.
func sanitizeMailSubject(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == ' ', r == ' ':
			b.WriteByte(' ')
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) >= 3 && strings.EqualFold(out[:3], "re:") {
		out = strings.TrimSpace(out[3:])
	}
	if r := []rune(out); len(r) > mailReplySubjectRunes {
		out = strings.TrimSpace(string(r[:mailReplySubjectRunes]))
	}
	if out == "" {
		out = mailReplyFallbackSubject
	}
	return out
}

// validMailAddr reports whether a is one plain address (no display name, no
// control characters), safe for a To header.
func validMailAddr(a string) bool {
	if a == "" || len(a) > 254 || strings.ContainsAny(a, " \t\r\n<>,;\"") {
		return false
	}
	p, err := mail.ParseAddress(a)
	return err == nil && p.Address == a
}

// renderMailReply builds the reply for an outcome. host is the hive host
// the mail was sent to; the reply goes to the sender at to.
func renderMailReply(outcome, host, from, to, title, messageID string) mailReplyMessage {
	m := mailReplyMessage{
		From: from, ReplyTo: "hive@" + host, To: to,
		Subject: "Re: " + sanitizeMailSubject(title), MessageID: messageID,
	}
	var b strings.Builder
	if outcome == mailReplyHeld {
		b.WriteString("Your mail reached the hive and is waiting for the hive owner's approval.\n")
		b.WriteString("Nothing more is needed from you.\n\n")
		b.WriteString("What this means:\n" + mailReplyHeldDocsURL + "\n\n")
	} else {
		b.WriteString("Your mail reached the hive.\n\n")
		b.WriteString("Open it in the Inbox (sign-in required):\nhttps://" + host + "/#inbox\n\n")
		b.WriteString("Something wrong? ")
	}
	b.WriteString("Report a problem:\n" + mailReplyIssueURL + "\n\n")
	b.WriteString(mailReplyAutoNotice + "\n")
	m.Text = b.String()
	return m
}
