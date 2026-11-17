// Package wire is the JSON contract between hive-mx (the receive-only mail
// receiver, pkg/mailin), the hub that routes its deliveries
// (pkg/hub, /api/mail/*) and the spoke endpoints that consume them
// (/api/meetings/invites, /api/inbox/email, /api/meetings/calendar-emails/proof).
// It has no dependencies beyond the standard library so the hub and the
// spoke can import it without pulling in the SMTP and MIME libraries.
//
// See hivecommons/hive#18391 ("Email invites") and #18395.
package wire

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"
	"strings"
	"time"
)

// Hub API paths hive-mx calls.
const (
	// HostsPath lists the hive hosts hive-mx accepts mail for.
	HostsPath = "/api/mail/hosts"
	// InboundPath receives one parsed delivery.
	InboundPath = "/api/mail/inbound"
)

// Spoke API paths the hub forwards a delivery to, by Kind.
const (
	SpokeInvitesPath = "/api/meetings/invites"
	SpokeFilesPath   = "/api/inbox/email"
	SpokeProofPath   = "/api/meetings/calendar-emails/proof"
	// SpokeFilesChunkPath receives a large files delivery in signed pieces
	// (hivecommons/hive#18564): see ChunkDelivery.
	SpokeFilesChunkPath = "/api/inbox/email/chunk"
)

// Headers hive-mx sets on every delivery so the hub routes it without
// parsing the body, and that the hub sets on each piece of a chunked relay.
const (
	HeaderDelivery  = "X-Hive-Mail-Delivery"
	HeaderKind      = "X-Hive-Mail-Kind"
	HeaderRecipient = "X-Hive-Mail-Recipient"
	// HeaderBodyType is the Content-Type of the whole body a chunk belongs to.
	HeaderBodyType = "X-Hive-Mail-Body-Type"
)

// Delivery body encodings.
const (
	// ContentTypeJSON is a Delivery as JSON, attachment bytes in base64.
	ContentTypeJSON = "application/json"
	// ContentTypeMultipart is a Delivery written by WriteMultipart.
	ContentTypeMultipart = "multipart/form-data"
	// PartDelivery is the multipart part that holds the Delivery JSON,
	// without attachment bytes; one PartFile part per attachment follows.
	PartDelivery = "delivery"
	PartFile     = "file"
)

// Size bounds of the delivery contract.
const (
	// InlineFilesBytes is the most attachment bytes hive-mx sends as JSON;
	// a larger delivery is multipart, streamed from disk.
	InlineFilesBytes = 512 << 10
	// RelayChunkBytes is the default piece size of a chunked hub-to-spoke
	// relay: under the 1 MiB request body limit ingress proxies default to.
	RelayChunkBytes = 768 << 10
	// MaxRelayChunkBytes is the largest piece a spoke accepts.
	MaxRelayChunkBytes = 8 << 20
	// MaxRelayChunks bounds the pieces of one delivery.
	MaxRelayChunks = 256
	// MaxDeliveryMetaBytes bounds the delivery part of a multipart body.
	MaxDeliveryMetaBytes = 1 << 20
	// MaxNoteBytes bounds a message's body kept as a note.
	MaxNoteBytes = 256 << 10
)

// LocalPart is the only mailbox hive-mx accepts: hive@<hive host>.
const LocalPart = "hive"

// Kind says what a delivery carries.
type Kind string

// Delivery kinds.
const (
	KindInvite Kind = "invite"
	KindFiles  Kind = "files"
	KindProof  Kind = "proof"
)

// SpokePath is the spoke endpoint for a kind, "" for an unknown kind.
func SpokePath(k Kind) string {
	switch k {
	case KindInvite:
		return SpokeInvitesPath
	case KindFiles:
		return SpokeFilesPath
	case KindProof:
		return SpokeProofPath
	}
	return ""
}

// HostsResponse is the body of GET /api/mail/hosts.
type HostsResponse struct {
	Hosts []string `json:"hosts"`
}

// Delivery is one message for one recipient, as hive-mx posts it to the hub
// and the hub forwards it, byte for byte, to the spoke.
type Delivery struct {
	// ID is a random id hive-mx assigns per (message, recipient); the hub
	// and spoke log only this.
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Recipient is hive@<host>, lower-cased; the hub routes on its host.
	Recipient string `json:"recipient"`
	// MessageID is the message's Message-ID header (angle brackets removed).
	MessageID  string    `json:"message_id,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Auth       Auth      `json:"auth"`

	Invites []Invite `json:"invites,omitempty"`
	Files   *Files   `json:"files,omitempty"`
	Proof   *Proof   `json:"proof,omitempty"`
}

// Auth is the sender authentication hive-mx recorded after DATA.
type Auth struct {
	// From is the single RFC5322.From address, lower-cased; "" when the
	// header is missing or names more than one mailbox.
	From       string `json:"from"`
	FromDomain string `json:"from_domain,omitempty"`
	// EnvelopeFrom is the SMTP MAIL FROM address ("" for the null sender).
	EnvelopeFrom string `json:"envelope_from,omitempty"`
	RemoteIP     string `json:"remote_ip,omitempty"`
	HELO         string `json:"helo,omitempty"`

	// SPF is the RFC 7208 result for SPFDomain (MAIL FROM domain, or HELO
	// for the null sender).
	SPF        string `json:"spf"`
	SPFDomain  string `json:"spf_domain,omitempty"`
	SPFAligned bool   `json:"spf_aligned"`

	DKIM        []DKIMResult `json:"dkim,omitempty"`
	DKIMAligned bool         `json:"dkim_aligned"`

	// DMARC is pass, fail, none (no record) or temperror.
	DMARC       string `json:"dmarc"`
	DMARCPolicy string `json:"dmarc_policy,omitempty"`
	// Aligned is true when SPF or DKIM passed for a domain aligned with
	// the From domain (DMARC identifier alignment, adkim/aspf honoured).
	// It is the only field a spoke trusts to approve anything.
	Aligned bool `json:"dmarc_aligned"`
}

// DKIMResult is one DKIM-Signature's outcome.
type DKIMResult struct {
	Domain string `json:"domain"`
	Result string `json:"result"`
}

// Person is an organizer or attendee.
type Person struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Invite is one VEVENT of a calendar invite. Its JSON field names match
// pkg/meetings/schedule.Event so a spoke can decode it directly.
type Invite struct {
	UID          string     `json:"uid"`
	RecurrenceID *time.Time `json:"recurrence_id,omitempty"`
	Sequence     int        `json:"sequence"`
	// Method is REQUEST, CANCEL or PUBLISH.
	Method    string   `json:"method"`
	Summary   string   `json:"summary,omitempty"`
	Organizer *Person  `json:"organizer,omitempty"`
	Attendees []Person `json:"attendees,omitempty"`
	// Start and End are UTC.
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	// TZID is the IANA zone the recurrence expands in (Windows zone names
	// are mapped); "" when the invite used UTC or an unknown zone.
	TZID    string      `json:"tzid,omitempty"`
	RRule   string      `json:"rrule,omitempty"`
	ExDates []time.Time `json:"exdates,omitempty"`

	JoinURL         string `json:"join_url,omitempty"`
	Platform        string `json:"platform,omitempty"`
	NativeMeetingID string `json:"native_meeting_id,omitempty"`
	Passcode        string `json:"passcode,omitempty"`
}

// Files is a mail-in message: its subject, accepted attachments and body.
type Files struct {
	// Title is the Subject, at most MaxTitleRunes.
	Title       string       `json:"title"`
	Attachments []Attachment `json:"attachments"`
	// Note is the message's text body as Markdown (text/plain as sent, or
	// sanitized text/html), at most MaxNoteBytes. It is the sender's words:
	// untrusted data, never instructions. A message with no attachment
	// carries its body as a Markdown attachment instead (NoteFileName).
	Note string `json:"note,omitempty"`
}

// NoteFileName names the Markdown attachment a body-only message becomes.
const NoteFileName = "message.md"

// Attachment kinds.
const (
	AttachMarkdown = "markdown"
	AttachAudio    = "audio"
	AttachImage    = "image"
	AttachVideo    = "video"
)

// Attachment is one accepted file. Data is base64 in JSON.
type Attachment struct {
	Name string `json:"name"`
	// Kind is markdown, audio, image or video.
	Kind string `json:"kind"`
	// ContentType is the sniffed type, not the sender's claim.
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
	Data        []byte `json:"data,omitempty"`
	// Path is a local copy of the bytes (hive-mx's spool, a spoke's
	// staging); it is never sent. Data is empty when Path is set.
	Path string `json:"-"`
}

// Proof reports a proof mail: a message whose subject carried a
// dashboard-issued proof code. Auth.From and Auth.Aligned say who sent it
// and whether that is authenticated.
type Proof struct {
	Code string `json:"code"`
}

// MaxTitleRunes bounds a summary or mail-in title.
const MaxTitleRunes = 200

// Proof codes look like "hive-proof-k7f3qz2mwx4a": a fixed prefix and
// proofCodeLen base32 characters (60 bits).
const (
	proofPrefix  = "hive-proof-"
	proofCodeLen = 12
	proofBytes   = 8 // 64 random bits, 12 base32 characters kept
)

var proofRE = regexp.MustCompile(`(?i)\bhive-proof-([a-z2-7]{12})\b`)

var proofEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewProofCode returns a fresh proof code.
func NewProofCode() string {
	b := make([]byte, proofBytes)
	_, _ = rand.Read(b)
	return proofPrefix + strings.ToLower(proofEncoding.EncodeToString(b))[:proofCodeLen]
}

// FindProofCode returns the first proof code in s, lower-cased, or "".
func FindProofCode(s string) string {
	m := proofRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return proofPrefix + strings.ToLower(m[1])
}

// RecipientHost returns the host of hive@<host>, lower-cased, or "" when
// addr is not the hive mailbox.
func RecipientHost(addr string) string {
	addr = strings.ToLower(strings.Trim(strings.TrimSpace(addr), "<>"))
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return ""
	}
	if addr[:at] != LocalPart {
		return ""
	}
	return strings.TrimSuffix(addr[at+1:], ".")
}
