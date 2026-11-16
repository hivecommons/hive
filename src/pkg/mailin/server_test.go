package mailin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// fakeHub records deliveries and answers with err.
type fakeHub struct {
	mu   sync.Mutex
	got  []wire.Delivery
	err  func(d wire.Delivery) error
	call int
}

func (h *fakeHub) Deliver(_ context.Context, d wire.Delivery) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.call++
	if h.err != nil {
		if err := h.err(d); err != nil {
			return err
		}
	}
	h.got = append(h.got, d)
	return nil
}

func (h *fakeHub) deliveries() []wire.Delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]wire.Delivery(nil), h.got...)
}

const testHost = "acme.hive.hivecommons.dev"

// startServer serves o on a loopback listener and returns its address.
func startServer(t *testing.T, o Options) string {
	t.Helper()
	if o.Hosts == nil {
		o.Hosts = &HostSet{}
		o.Hosts.Set([]string{testHost, "beta.hive.hivecommons.dev"})
	}
	if o.Resolver == nil {
		o.Resolver = &fakeResolver{txt: map[string][]string{
			"example.com":               {"v=spf1 ip4:127.0.0.1 -all"},
			"_dmarc.example.com":        {"v=DMARC1; p=reject"},
			"s1._domainkey.example.com": {dkimTXT()},
		}}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	srv := NewServer(o)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func dial(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Hello("client.example.com"); err != nil {
		t.Fatal(err)
	}
	return c
}

func smtpCode(err error) int {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// send runs one transaction and returns the DATA result.
func send(t *testing.T, c *smtp.Client, from string, to []string, msg []byte) error {
	t.Helper()
	if err := c.Mail(from, nil); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r, nil); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

func inviteMail(t *testing.T, name string) []byte {
	return buildMail("Ada <ada@example.com>", "Invitation",
		part{contentType: "text/calendar; method=REQUEST", body: fixture(t, name)})
}

func TestSMTPInviteDelivered(t *testing.T) {
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub})
	c := dial(t, addr)
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH must not be offered")
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS offered without a certificate")
	}
	if ok, v := c.Extension("SIZE"); !ok || v != fmt.Sprint(DefaultMaxMessageBytes) {
		t.Fatalf("SIZE %v %q", ok, v)
	}
	err := send(t, c, "bounce@example.com", []string{"hive@ACME.hive.hivecommons.dev", "hive@acme.hive.hivecommons.dev", "hive@beta.hive.hivecommons.dev"}, inviteMail(t, "zoom.ics"))
	if err != nil {
		t.Fatal(err)
	}
	got := hub.deliveries()
	if len(got) != 2 {
		t.Fatalf("%d deliveries, want one per distinct recipient", len(got))
	}
	d := got[0]
	if d.Kind != wire.KindInvite || d.Recipient != "hive@"+testHost || len(d.Invites) != 1 || d.ID == "" || d.ID == got[1].ID {
		t.Fatalf("delivery %+v", d)
	}
	if d.Invites[0].Passcode == "" || d.Invites[0].Platform != PlatformZoom {
		t.Fatalf("invite %+v", d.Invites[0])
	}
	if !d.Auth.Aligned || d.Auth.SPF != "pass" || d.Auth.From != "ada@example.com" || d.Auth.RemoteIP != "127.0.0.1" || d.Auth.HELO != "client.example.com" {
		t.Fatalf("auth %+v", d.Auth)
	}
}

func TestSMTPRcptRules(t *testing.T) {
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub, MaxRecipients: 2})
	c := dial(t, addr)
	if err := c.Mail("bounce@example.com", nil); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{
		"someone@" + testHost,               // only the hive mailbox
		"hive@unknown.hive.hivecommons.dev", // unknown host
		"hive@gmail.com",                    // no relay
		"hive+tag@" + testHost,              // no plus addressing
	} {
		if err := c.Rcpt(to, nil); smtpCode(err) != 550 {
			t.Errorf("%s: %v, want 550", to, err)
		}
	}
	if err := c.Rcpt("hive@"+testHost, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hive@beta.hive.hivecommons.dev", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hive@"+testHost, nil); smtpCode(err) != 452 {
		t.Fatalf("recipient cap: %v", err)
	}
	if hub.call != 0 {
		t.Fatal("nothing should be delivered before DATA")
	}
}

func TestSMTPHostsNotLoaded(t *testing.T) {
	addr := startServer(t, Options{Hub: &fakeHub{}, Hosts: &HostSet{}})
	c := dial(t, addr)
	if err := c.Mail("bounce@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hive@"+testHost, nil); smtpCode(err) != 451 {
		t.Fatalf("before the first host list: %v", err)
	}
}

func TestSMTPSizeCaps(t *testing.T) {
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub, MaxMessageBytes: 4096})
	c := dial(t, addr)
	if err := c.Mail("bounce@example.com", &smtp.MailOptions{Size: 8192}); smtpCode(err) != 552 {
		t.Fatalf("SIZE over the cap: %v", err)
	}
	_ = c.Reset()
	big := buildMail("ada@example.com", "big", part{contentType: "image/png", disposition: "attachment; filename=a.png", body: append(append([]byte(nil), pngData...), make([]byte, 8192)...)})
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, big); smtpCode(err) != 552 {
		t.Fatalf("body over the cap: %v", err)
	}
	if len(hub.deliveries()) != 0 {
		t.Fatal("an oversize message was delivered")
	}
}

func TestSMTPRateLimits(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub, IPLimiter: NewRateLimiter(2, time.Hour, 2, clock)})
	c := dial(t, addr)
	for i := 0; i < 2; i++ {
		if err := c.Mail("bounce@example.com", nil); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		_ = c.Reset()
	}
	if err := c.Mail("bounce@example.com", nil); smtpCode(err) != 450 {
		t.Fatalf("per-IP limit: %v", err)
	}

	addr = startServer(t, Options{Hub: hub, DomainLimiter: NewRateLimiter(1, time.Hour, 1, clock)})
	c = dial(t, addr)
	if err := c.Mail("a@mail.example.org", nil); err != nil {
		t.Fatal(err)
	}
	_ = c.Reset()
	// Same organizational domain, different subdomain: same bucket.
	if err := c.Mail("b@example.org", nil); smtpCode(err) != 450 {
		t.Fatalf("per-domain limit: %v", err)
	}
	_ = c.Reset()
	if err := c.Mail("c@example.net", nil); err != nil {
		t.Fatalf("another domain: %v", err)
	}
	_ = c.Reset()
	// The null sender has no domain to limit.
	if err := c.Mail("", nil); err != nil {
		t.Fatalf("null sender: %v", err)
	}
}

func TestSMTPDataOutcomes(t *testing.T) {
	to := []string{"hive@" + testHost}
	cases := []struct {
		name string
		msg  []byte
		err  func(wire.Delivery) error
		code int
		kind wire.Kind
	}{
		{"files", buildMail("ada@example.com", "notes", part{contentType: "text/markdown", disposition: "attachment; filename=n.md", body: mdData}), nil, 0, wire.KindFiles},
		{"proof", buildMail("ada@example.com", "hive-proof-abcdefghijkl", part{contentType: "text/plain", body: []byte("x"), raw: true}), nil, 0, wire.KindProof},
		{"nothing", buildMail("ada@example.com", "hi", part{contentType: "text/plain", body: []byte("x"), raw: true}), nil, 550, ""},
		{"malformed", []byte("this is not a header\r\n\r\nx\r\n"), nil, 550, ""},
		{"hub down", inviteMail(t, "google.ics"), func(wire.Delivery) error { return errors.New("connection refused") }, 451, ""},
		{"hub refuses", inviteMail(t, "google.ics"), func(wire.Delivery) error { return fmt.Errorf("%w: 404", ErrRejected) }, 550, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHub{err: tc.err}
			c := dial(t, startServer(t, Options{Hub: hub}))
			err := send(t, c, "bounce@example.com", to, tc.msg)
			if smtpCode(err) != tc.code || (tc.code == 0 && err != nil) {
				t.Fatalf("result %v, want code %d", err, tc.code)
			}
			got := hub.deliveries()
			if tc.kind == "" {
				if len(got) != 0 {
					t.Fatalf("delivered %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != tc.kind {
				t.Fatalf("deliveries %+v", got)
			}
			if tc.kind == wire.KindProof && (got[0].Proof == nil || got[0].Proof.Code != "hive-proof-abcdefghijkl") {
				t.Fatalf("proof %+v", got[0].Proof)
			}
		})
	}
}

func TestSMTPPartialFailureIsTemporary(t *testing.T) {
	hub := &fakeHub{err: func(d wire.Delivery) error {
		if strings.Contains(d.Recipient, "beta") {
			return fmt.Errorf("%w: 404", ErrRejected)
		}
		return nil
	}}
	c := dial(t, startServer(t, Options{Hub: hub}))
	// One accepted, one refused: the message is accepted (the refused
	// hive is gone; retrying would not help).
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost, "hive@beta.hive.hivecommons.dev"}, inviteMail(t, "google.ics")); err != nil {
		t.Fatalf("partial refusal: %v", err)
	}
	if len(hub.deliveries()) != 1 {
		t.Fatal("the accepted recipient was not delivered")
	}
}

func TestSMTPDKIMSignedMessage(t *testing.T) {
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub, Resolver: &fakeResolver{txt: map[string][]string{
		"s1._domainkey.example.com": {dkimTXT()},
		"_dmarc.example.com":        {"v=DMARC1; p=reject"},
	}}})
	c := dial(t, addr)
	// Sent through a third-party relay (SPF of another domain), but DKIM
	// signed by the From domain: aligned.
	signed := signFor(t, "example.com", string(inviteMail(t, "google.ics")))
	if err := send(t, c, "bounce@relay.example.net", []string{"hive@" + testHost}, signed); err != nil {
		t.Fatal(err)
	}
	d := hub.deliveries()[0]
	if !d.Auth.DKIMAligned || !d.Auth.Aligned || d.Auth.SPFAligned {
		t.Fatalf("auth %+v", d.Auth)
	}
	// The same message with a forged From is not aligned.
	forged := bytes.Replace(signed, []byte("From: Ada <ada@example.com>"), []byte("From: Ada <ada@example.org>"), 1)
	if err := send(t, c, "bounce@relay.example.net", []string{"hive@" + testHost}, forged); err != nil {
		t.Fatal(err)
	}
	if d := hub.deliveries()[1]; d.Auth.Aligned || d.Auth.From != "ada@example.org" {
		t.Fatalf("forged From: %+v", d.Auth)
	}
}

func TestNewServerDefaults(t *testing.T) {
	s := NewServer(Options{Hub: &fakeHub{}})
	if s.Domain != DefaultDomain || s.MaxRecipients != DefaultMaxRecipients || s.MaxMessageBytes != DefaultMaxMessageBytes || s.TLSConfig != nil {
		t.Fatalf("defaults %+v", s)
	}
	// The error log adapter must not panic.
	s.ErrorLog.Printf("x %d", 1)
	s.ErrorLog.Println("y")
}
