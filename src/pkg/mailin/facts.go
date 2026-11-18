package mailin

import (
	"mime"
	"net/mail"
	"strings"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// FactsOf reads the header facts the hub's reply decision needs
// (wire.Facts, hivecommons/hive#18746) from a message's headers and its SMTP
// MAIL FROM. It only classifies; hive-mx still never sends mail.
func FactsOf(h mail.Header, envelopeFrom string) *wire.Facts {
	f := &wire.Facts{
		AutoSubmitted: factToken(h.Get("Auto-Submitted")),
		Precedence:    factToken(h.Get("Precedence")),
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "list-") {
			f.ListHeader = true
			break
		}
	}
	_, f.AutoResponseSuppress = h["X-Auto-Response-Suppress"]
	rp := strings.TrimSpace(h.Get("Return-Path"))
	f.NullReturnPath = strings.TrimSpace(envelopeFrom) == "" || rp == "<>"
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	f.Bounce = mt == "multipart/report" || mt == "message/delivery-status"
	f.SenderLocal = localPartOf(singleFrom(h))
	f.EnvelopeLocal = localPartOf(strings.ToLower(strings.TrimSpace(envelopeFrom)))
	return f
}

// factToken is the first token of a header value (before any ';' or space),
// lower-cased and bounded.
func factToken(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if i := strings.IndexAny(v, "; \t"); i >= 0 {
		v = v[:i]
	}
	return clipBytes(v, wire.MaxFactTokenBytes)
}

// localPartOf is the part of addr before its last '@', bounded.
func localPartOf(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 {
		return ""
	}
	return clipBytes(addr[:at], wire.MaxFactTokenBytes)
}
