package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Recent mail (hivecommons/hive#18699): the last MaxMailEvents messages
// mailed to this hive, accepted or refused, with the reason of a refusal,
// so a mail that never showed up in the Inbox can be explained from the
// dashboard. An event holds no content: no subject, no file name and only
// the sender's domain. Accepted mail is recorded by the hive itself;
// refusals by the mail receiver arrive as relay-signed reports
// (POST /api/inbox/email/report).

// Mail event outcomes.
const (
	MailAccepted         = "accepted"
	MailAwaitingApproval = "awaiting_approval"
	MailRejected         = "rejected"
)

// Mail event bounds.
const (
	// MaxMailEvents is how many events the log keeps.
	MaxMailEvents = 50
	// emailLogFile is the log, next to the item directories of the spool.
	emailLogFile     = "recent-mail.json"
	maxMailReason    = 64
	maxMailDomain    = 253
	maxMailEventKind = 16
	// maxMailItems bounds the item ids kept per event.
	maxMailItems = 20
)

// MailEvent is one message mailed to the hive.
type MailEvent struct {
	At time.Time `json:"at"`
	// Outcome is accepted, awaiting_approval or rejected.
	Outcome string `json:"outcome"`
	// Kind is files, invite or proof ("" for a refusal before the kind is known).
	Kind string `json:"kind,omitempty"`
	// Reason is a wire.Reason* category, set for a refusal.
	Reason string `json:"reason,omitempty"`
	// Code is the SMTP reply code the sender got for a refusal.
	Code         int    `json:"code,omitempty"`
	SenderDomain string `json:"sender_domain,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	// Files is the number of attachments stored (accepted) or carried (refused).
	Files int `json:"files"`
	// Skipped lists the attachments left out: extension and reason only.
	Skipped []wire.SkippedFile `json:"skipped,omitempty"`
	// ApprovalReason says why an accepted mail awaits approval:
	// ApprovalNotAuthenticated or ApprovalNotOnAllowlist.
	ApprovalReason string `json:"approval_reason,omitempty"`
	// Auth is the sender authentication the mail receiver recorded.
	Auth *MailAuth `json:"auth,omitempty"`
	// Items are the Inbox items of a mail that awaits approval, so it can
	// be approved or declined from the list (hivecommons/hive#18744).
	Items []string `json:"items,omitempty"`
}

// Why a mail awaits the owner's approval.
const (
	// ApprovalNotAuthenticated: the From address did not pass DMARC
	// alignment (neither SPF nor DKIM passed for its domain).
	ApprovalNotAuthenticated = "sender_not_authenticated"
	// ApprovalNotOnAllowlist: the sender is authenticated but is not a
	// proven calendar email of a user with access to this hive.
	ApprovalNotOnAllowlist = "sender_not_on_allowlist"
)

// maxMailDKIM bounds the DKIM results kept per event.
const maxMailDKIM = 5

// MailAuth is the authentication of a mail's sender, as domains and
// results: enough to see why DMARC passed or failed, and which server
// handed the mail over.
type MailAuth struct {
	// DMARC is pass, fail, none or temperror; Aligned is the decision a
	// hive trusts (SPF or DKIM passed for the From domain).
	DMARC   string `json:"dmarc,omitempty"`
	Aligned bool   `json:"aligned"`
	// SPF is the result for SPFDomain (the envelope sender's domain).
	SPF       string `json:"spf,omitempty"`
	SPFDomain string `json:"spf_domain,omitempty"`
	// DKIM lists each signature as "domain:result"; empty when unsigned.
	DKIM []string `json:"dkim,omitempty"`
	// RemoteIP and HELO are the server that delivered the mail.
	RemoteIP string `json:"remote_ip,omitempty"`
	HELO     string `json:"helo,omitempty"`
}

// MailAuthOf summarizes a delivery's authentication for an event.
func MailAuthOf(a wire.Auth) *MailAuth {
	out := &MailAuth{DMARC: a.DMARC, Aligned: a.Aligned, SPF: a.SPF, SPFDomain: a.SPFDomain, RemoteIP: a.RemoteIP, HELO: a.HELO}
	for _, d := range a.DKIM {
		out.DKIM = append(out.DKIM, d.Domain+":"+d.Result)
	}
	return out
}

// mailAddrToken keeps what an IP address or a "domain:result" pair is made
// of: mailToken's set plus the colon.
func mailAddrToken(s string, limit int) string {
	parts := strings.Split(s, ":")
	for i, p := range parts {
		parts[i] = mailToken(p, limit)
	}
	s = strings.Join(parts, ":")
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

func (a *MailAuth) clean() *MailAuth {
	if a == nil {
		return nil
	}
	out := &MailAuth{
		DMARC: mailToken(a.DMARC, maxMailEventKind), Aligned: a.Aligned, SPF: mailToken(a.SPF, maxMailEventKind),
		SPFDomain: mailToken(a.SPFDomain, maxMailDomain), RemoteIP: mailAddrToken(a.RemoteIP, maxMailReason), HELO: mailToken(a.HELO, maxMailDomain),
	}
	for i, d := range a.DKIM {
		if i == maxMailDKIM {
			break
		}
		out.DKIM = append(out.DKIM, mailAddrToken(d, maxMailDomain))
	}
	return out
}

// mailToken keeps the letters, digits and ._- of s, lower-cased, at most
// limit bytes: every string of an event is a short token, never free text.
func mailToken(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return -1
	}, strings.ToLower(s))
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

// SenderDomainOf is the domain of an address as an event records it.
func SenderDomainOf(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		addr = addr[at+1:]
	}
	return mailToken(addr, maxMailDomain)
}

// clean bounds and sanitizes an event received from outside.
func (ev MailEvent) clean() MailEvent {
	switch ev.Outcome {
	case MailAccepted, MailAwaitingApproval, MailRejected:
	default:
		ev.Outcome = MailRejected
	}
	ev.Kind = mailToken(ev.Kind, maxMailEventKind)
	ev.Reason = mailToken(ev.Reason, maxMailReason)
	ev.ApprovalReason = mailToken(ev.ApprovalReason, maxMailReason)
	ev.Auth = ev.Auth.clean()
	ev.SenderDomain = mailToken(ev.SenderDomain, maxMailDomain)
	ev.Bytes = max(ev.Bytes, 0)
	ev.Files = max(ev.Files, 0)
	ev.Code = max(ev.Code, 0)
	if len(ev.Skipped) > wire.MaxSkipped {
		ev.Skipped = ev.Skipped[:wire.MaxSkipped]
	}
	skipped := make([]wire.SkippedFile, 0, len(ev.Skipped))
	for _, sk := range ev.Skipped {
		skipped = append(skipped, wire.SkippedFile{Ext: mailToken(sk.Ext, wire.MaxExtRunes), Reason: mailToken(sk.Reason, maxMailEventKind)})
	}
	ev.Skipped = skipped
	if len(ev.Skipped) == 0 {
		ev.Skipped = nil
	}
	var items []string
	for _, id := range ev.Items {
		if validEmailID(id) && len(items) < maxMailItems {
			items = append(items, id)
		}
	}
	ev.Items = items
	return ev
}

// PendingItems is the subset of ids that are still in the spool and still
// await approval: what an Approve or Decline next to a recent mail acts on.
func (e *EmailSource) PendingItems(ids []string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, id := range ids {
		if !validEmailID(id) {
			continue
		}
		if m, err := e.readMeta(id); err == nil && !m.Approved {
			out = append(out, id)
		}
	}
	return out
}

func (e *EmailSource) logPath() string { return filepath.Join(e.Dir, emailLogFile) }

func (e *EmailSource) readMailLog() []MailEvent {
	b, err := os.ReadFile(e.logPath())
	if err != nil {
		return nil
	}
	var evs []MailEvent
	if json.Unmarshal(b, &evs) != nil {
		return nil
	}
	return evs
}

// RecordMail adds an event to the recent-mail log, newest first, keeping
// the last MaxMailEvents. A zero At is now.
func (e *EmailSource) RecordMail(ev MailEvent) error {
	ev = ev.clean()
	if ev.At.IsZero() {
		ev.At = e.now()
	}
	ev.At = ev.At.UTC()
	e.logMu.Lock()
	defer e.logMu.Unlock()
	evs := append([]MailEvent{ev}, e.readMailLog()...)
	if len(evs) > MaxMailEvents {
		evs = evs[:MaxMailEvents]
	}
	b, err := json.Marshal(evs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.Dir, emailDirMode); err != nil {
		return err
	}
	tmp := e.logPath() + ".tmp"
	if err := os.WriteFile(tmp, b, emailFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, e.logPath())
}

// RecentMail is the recent-mail log, newest first.
func (e *EmailSource) RecentMail() []MailEvent {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	evs := e.readMailLog()
	if evs == nil {
		evs = []MailEvent{}
	}
	return evs
}
