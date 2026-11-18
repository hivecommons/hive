package wire

// Facts are the header and envelope facts the hub needs to decide whether a
// confirmation reply may be sent for a delivery (hivecommons/hive#18746).
// The hub never sees headers, so hive-mx reads them once and passes only
// these booleans and short tokens: never a header value from the sender
// beyond the two bounded tokens below, never a subject or a body.
//
// A delivery with no Facts (a hive-mx that predates them) is never replied
// to: the absence of a fact is not evidence that the message is not
// automatic.
type Facts struct {
	// AutoSubmitted is the Auto-Submitted header's first token, lower-cased
	// and at most MaxFactTokenBytes ("" when absent; "no" means a person).
	AutoSubmitted string `json:"auto_submitted,omitempty"`
	// Precedence is the Precedence header, lower-cased and bounded ("" when
	// absent), for example bulk, list, junk or auto_reply.
	Precedence string `json:"precedence,omitempty"`
	// ListHeader is true when any List-* header (List-Id, List-Unsubscribe,
	// List-Post, ...) is present: the message is list or bulk mail.
	ListHeader bool `json:"list_header,omitempty"`
	// AutoResponseSuppress is true when X-Auto-Response-Suppress is present.
	AutoResponseSuppress bool `json:"auto_response_suppress,omitempty"`
	// NullReturnPath is true when the SMTP MAIL FROM was <> or a
	// Return-Path header says so: a bounce or an automatic reply.
	NullReturnPath bool `json:"null_return_path,omitempty"`
	// Bounce is true when the message is a delivery status report
	// (multipart/report or message/delivery-status).
	Bounce bool `json:"bounce,omitempty"`
	// SenderLocal is the local part of the RFC5322.From address,
	// lower-cased and bounded ("" when there is no single From).
	SenderLocal string `json:"sender_local,omitempty"`
	// EnvelopeLocal is the local part of the SMTP MAIL FROM address,
	// lower-cased and bounded ("" for the null sender).
	EnvelopeLocal string `json:"envelope_local,omitempty"`
}

// MaxFactTokenBytes bounds every string in Facts.
const MaxFactTokenBytes = 64
