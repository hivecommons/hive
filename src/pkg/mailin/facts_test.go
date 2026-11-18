package mailin

import (
	"net/mail"
	"strings"
	"testing"
)

func readHeader(t *testing.T, raw string) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw + "\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header
}

func TestFactsOf(t *testing.T) {
	plain := FactsOf(readHeader(t, "From: Alice <Alice@Example.org>\r\nSubject: hi"), "alice@example.org")
	if plain.AutoSubmitted != "" || plain.Precedence != "" || plain.ListHeader || plain.AutoResponseSuppress || plain.NullReturnPath || plain.Bounce {
		t.Fatalf("a person's mail has no automatic fact: %+v", plain)
	}
	if plain.SenderLocal != "alice" || plain.EnvelopeLocal != "alice" {
		t.Fatalf("local parts: %+v", plain)
	}

	auto := FactsOf(readHeader(t, "From: a@example.org\r\nAuto-Submitted: Auto-Replied; foo=bar\r\nPrecedence: Bulk\r\nX-Auto-Response-Suppress: All"), "a@example.org")
	if auto.AutoSubmitted != "auto-replied" || auto.Precedence != "bulk" || !auto.AutoResponseSuppress {
		t.Fatalf("automatic headers: %+v", auto)
	}

	for _, h := range []string{"List-Id: <x.example.org>", "List-Unsubscribe: <mailto:u@example.org>", "List-Post: NO"} {
		if !FactsOf(readHeader(t, "From: a@example.org\r\n"+h), "a@example.org").ListHeader {
			t.Errorf("%q not seen as list mail", h)
		}
	}
	if !FactsOf(readHeader(t, "From: a@example.org"), "").NullReturnPath {
		t.Error("an empty MAIL FROM is a null return path")
	}
	if !FactsOf(readHeader(t, "From: a@example.org\r\nReturn-Path: <>"), "a@example.org").NullReturnPath {
		t.Error("Return-Path <> is a null return path")
	}
	if !FactsOf(readHeader(t, "From: mailer-daemon@example.org\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=x"), "").Bounce {
		t.Error("a delivery status report is a bounce")
	}
	long := FactsOf(readHeader(t, "From: a@example.org\r\nAuto-Submitted: "+strings.Repeat("x", 500)), "a@example.org")
	if len(long.AutoSubmitted) > 64 {
		t.Errorf("token not bounded: %d", len(long.AutoSubmitted))
	}
	if FactsOf(readHeader(t, "From: a@example.org, b@example.org"), "x@example.org").SenderLocal != "" {
		t.Error("two From mailboxes give no sender local part")
	}
}
