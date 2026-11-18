package mailin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
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
	if ok, v := c.Extension("SIZE"); !ok || v != fmt.Sprint(DefaultMaxMessageBytes) || v != "36700160" {
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
	// The header facts the hub's reply decision reads (#18746).
	if d.Facts == nil || d.Facts.SenderLocal != "ada" || d.Facts.EnvelopeLocal != "bounce" || d.Facts.NullReturnPath || d.Facts.AutoSubmitted != "" {
		t.Fatalf("facts %+v", d.Facts)
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
		{"text only", buildMail("ada@example.com", "hi", part{contentType: "text/plain", body: []byte("x"), raw: true}), nil, 0, wire.KindFiles},
		{"nothing", buildMail("ada@example.com", "hi", part{contentType: "application/zip", disposition: "attachment; filename=a.zip", body: []byte("PK\x03\x04")}), nil, 550, ""},
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

func (h *fakeHub) setErr(f func(wire.Delivery) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = f
}

func TestSMTPLargeVideoSpooled(t *testing.T) {
	spool := t.TempDir()
	video := append(append([]byte(nil), mp4Data...), make([]byte, 3<<20)...)
	var seen []string
	hub := &fakeHub{err: func(d wire.Delivery) error {
		a := d.Files.Attachments[0]
		data, err := os.ReadFile(a.Path)
		if err != nil || len(data) != len(video) {
			t.Errorf("spooled attachment: %v %d", err, len(data))
		}
		entries, _ := os.ReadDir(spool)
		for _, e := range entries {
			seen = append(seen, e.Name())
		}
		return nil
	}}
	addr := startServer(t, Options{Hub: hub, SpoolDir: spool})
	c := dial(t, addr)
	msg := buildMail("ada@example.com", "clip",
		part{contentType: "text/plain", body: []byte("from the beach"), raw: true},
		part{contentType: "video/mp4", disposition: "attachment; filename=beach.mp4", body: video})
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, msg); err != nil {
		t.Fatal(err)
	}
	d := hub.deliveries()[0]
	if d.Files.Note != "from the beach" || d.Files.Attachments[0].Kind != wire.AttachVideo || d.Files.Attachments[0].Size != len(video) {
		t.Fatalf("delivery %+v", d.Files)
	}
	// The message and its attachment were on disk during delivery and are
	// gone after it.
	hub.mu.Lock()
	n := len(seen)
	hub.mu.Unlock()
	if n != 2 {
		t.Fatalf("spool during delivery %v", seen)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool not cleaned: %d entries", len(entries))
	}
}

func TestSMTPHiveBudget(t *testing.T) {
	msg := inviteMail(t, "google.ics")
	// Two messages fit a hive's budget, a third does not.
	const fit = 5
	budget := NewByteBudget(int64(len(msg))*fit/2, 24*time.Hour, nil)
	hub := &fakeHub{}
	addr := startServer(t, Options{Hub: hub, HiveBudget: budget})
	c := dial(t, addr)
	for i := 0; i < 2; i++ {
		if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, msg); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, msg); smtpCode(err) != 452 {
		t.Fatalf("over budget: %v", err)
	}
	// Another hive has its own budget; the full one still answers 452.
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost, "hive@beta.hive.hivecommons.dev"}, msg); smtpCode(err) != 452 {
		t.Fatalf("mixed: %v", err)
	}
	if n := len(hub.deliveries()); n != 3 {
		t.Fatalf("%d deliveries, want 3", n)
	}
	// A failed delivery gives its bytes back: neither of these is charged.
	hub.setErr(func(wire.Delivery) error { return errors.New("down") })
	if err := send(t, c, "bounce@example.com", []string{"hive@beta.hive.hivecommons.dev"}, msg); smtpCode(err) != 451 {
		t.Fatalf("hub down: %v", err)
	}
	hub.setErr(func(wire.Delivery) error { return fmt.Errorf("%w: 404", ErrRejected) })
	if err := send(t, c, "bounce@example.com", []string{"hive@beta.hive.hivecommons.dev"}, msg); smtpCode(err) != 550 {
		t.Fatalf("refused: %v", err)
	}
	if !budget.Reserve("beta.hive.hivecommons.dev", int64(len(msg))) {
		t.Fatal("failed deliveries were charged to the budget")
	}
}

func TestSMTPBusyAndSpoolErrors(t *testing.T) {
	b := &backend{o: Options{Logger: slog.New(slog.DiscardHandler)}, data: make(chan struct{}, 1)}
	s := &session{o: b.o, data: b.data}
	b.data <- struct{}{}
	if err := s.Data(strings.NewReader("x")); !errors.Is(err, errBusy) {
		t.Fatalf("busy: %v", err)
	}
	<-b.data
	s.o.SpoolDir = filepath.Join(t.TempDir(), "missing")
	if err := s.Data(strings.NewReader("x")); !errors.Is(err, errSpool) {
		t.Fatalf("no spool: %v", err)
	}
	s.o.SpoolDir = t.TempDir()
	if err := s.Data(iotest.ErrReader(errors.New("conn reset"))); err == nil || errors.Is(err, errSpool) {
		t.Fatalf("read error: %v", err)
	}
	if entries, _ := os.ReadDir(s.o.SpoolDir); len(entries) != 0 {
		t.Fatal("a failed read left its spool file")
	}
}

// logBuffer collects JSON log lines from the server's goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines decodes the log lines whose message is msg.
func (b *logBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// waitLines polls for n log lines with msg: a session's last line is
// written after the client has its reply.
func (b *logBuffer) waitLines(t *testing.T, msg string, n int) []map[string]any {
	t.Helper()
	const tries, pause = 200, 10 * time.Millisecond
	var got []map[string]any
	for i := 0; i < tries; i++ {
		if got = b.lines(t, msg); len(got) >= n {
			return got
		}
		time.Sleep(pause)
	}
	t.Fatalf("%d %q log lines, want %d:\n%s", len(got), msg, n, b.String())
	return nil
}

func docAttachment(name string, body []byte) part {
	return part{contentType: "application/octet-stream", disposition: `attachment; filename="` + name + `"`, body: body}
}

// TestSMTPOfficeDocumentsAccepted (hivecommons/hive#18699): a mail with
// only office documents attached, and no body, is delivered with each file
// attached, and the session leaves one INFO line with the outcome and no
// content: no subject, no file name, no full sender address.
func TestSMTPOfficeDocumentsAccepted(t *testing.T) {
	logs := &logBuffer{}
	hub, reports := &fakeHub{}, &fakeHub{}
	spool := t.TempDir()
	pptx := pptxData(t)
	addr := startServer(t, Options{Hub: hub, Reporter: reports, SpoolDir: spool, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	c := dial(t, addr)
	msg := buildMail("Ada <ada@example.com>", "Testing1-2-3 confidential",
		docAttachment("KubeCon Talk.pptx", pptx), docAttachment("report.pdf", pdfData),
		docAttachment("memo.docx", docxData(t)), docAttachment("numbers.xlsx", xlsxData(t)))
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, msg); err != nil {
		t.Fatalf("office documents refused: %v", err)
	}
	got := hub.deliveries()
	if len(got) != 1 || got[0].Kind != wire.KindFiles || len(got[0].Files.Attachments) != 4 || len(got[0].Files.Skipped) != 0 {
		t.Fatalf("deliveries %+v", got)
	}
	a := got[0].Files.Attachments[0]
	if a.Name != "KubeCon Talk.pptx" || a.Kind != wire.AttachDocument || a.ContentType != ctPPTX || a.Size != len(pptx) {
		t.Fatalf("pptx attachment %+v", a)
	}
	if len(reports.deliveries()) != 0 {
		t.Fatal("an accepted message was reported as refused")
	}
	lines := logs.waitLines(t, "hive-mx: message accepted", 1)
	l := lines[0]
	if l["level"] != "INFO" || l["outcome"] != outcomeAccepted || l["code"] != float64(smtpAccepted) || l["sender_domain"] != "example.com" ||
		l["attachments"] != float64(4) || l["accepted_files"] != float64(4) || l["kind"] != string(wire.KindFiles) {
		t.Fatalf("outcome line %+v", l)
	}
	// The authentication behind the DMARC result is on the line.
	if l["spf"] != resultPass || l["spf_domain"] != "example.com" || l["dmarc"] != resultPass || l["dmarc_aligned"] != true || l["helo"] != "client.example.com" {
		t.Fatalf("authentication fields %+v", l)
	}
	if size, _ := l["bytes"].(float64); size < float64(len(pptx)) {
		t.Fatalf("bytes %v", l["bytes"])
	}
	if hosts, _ := l["rcpt_hosts"].([]any); len(hosts) != 1 || hosts[0] != testHost {
		t.Fatalf("rcpt_hosts %+v", l["rcpt_hosts"])
	}
	for _, private := range []string{"ada@example.com", "bounce@example.com", "Testing1-2-3", "confidential", "KubeCon", "Talk"} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("the log holds %q:\n%s", private, logs.String())
		}
	}
}

// TestSMTPRejectionReasons (hivecommons/hive#18699): each refusal answers
// with a reply that says why, leaves one INFO line with its reason
// category, and, when it is about the message, is reported to the hive
// without any content.
func TestSMTPRejectionReasons(t *testing.T) {
	to := []string{"hive@" + testHost}
	big := append(append([]byte(nil), pngData...), make([]byte, 4096)...)
	smallFiles := DefaultLimits()
	smallFiles.MaxFileBytes = 64
	refuse := func(wire.Delivery) error { return fmt.Errorf("%w: HTTP 422: hive refused the delivery", ErrRejected) }
	cases := []struct {
		name     string
		opts     Options
		msg      []byte
		hubErr   func(wire.Delivery) error
		code     int
		reply    string
		reason   string
		outcome  string
		reported bool
	}{
		{"empty", Options{}, buildMail("ada@example.com", "hi", part{contentType: "text/plain", body: []byte(" \r\n "), raw: true}), nil,
			550, "no text, no calendar invite and no attachment", wire.ReasonEmpty, outcomeRejected, true},
		{"unsupported type", Options{}, buildMail("ada@example.com", "deck", docAttachment("Slides.key", []byte("keynote"))), nil,
			550, "a .key attachment is not an accepted type", wire.ReasonUnsupported, outcomeRejected, true},
		{"content mismatch", Options{}, buildMail("ada@example.com", "deck", docAttachment("deck.pptx", docxData(t))), nil,
			550, "a .pptx attachment is damaged, macro-enabled or not what its extension says", wire.ReasonContentMismatch, outcomeRejected, true},
		{"attachment too large", Options{Limits: smallFiles}, buildMail("ada@example.com", "pic", docAttachment("huge.png", big)), nil,
			552, "a .png attachment is over the 0 MB per-file limit", wire.ReasonAttachmentTooLarge, outcomeRejected, true},
		{"message too large", Options{MaxMessageBytes: 2048}, buildMail("ada@example.com", "pic", docAttachment("huge.png", big)), nil,
			552, "the message is over the 0 MB limit", wire.ReasonMessageTooLarge, outcomeRejected, true},
		{"malformed", Options{}, []byte("this is not a header\r\n\r\nx\r\n"), nil,
			550, "could not be parsed", wire.ReasonMalformed, outcomeRejected, true},
		{"hive daily volume", Options{HiveBudget: NewByteBudget(1, 24*time.Hour, nil)}, inviteMail(t, "google.ics"), nil,
			452, "daily volume", wire.ReasonHiveVolume, outcomeDeferred, true},
		{"hive refused", Options{}, inviteMail(t, "google.ics"), refuse,
			550, "the hive refused this message", wire.ReasonHiveRefused, outcomeRejected, false},
		{"hub down", Options{}, inviteMail(t, "google.ics"), func(wire.Delivery) error { return errors.New("connection refused") },
			451, "temporary failure", reasonTemporary, outcomeDeferred, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logBuffer{}
			hub, reports := &fakeHub{err: tc.hubErr}, &fakeHub{}
			o := tc.opts
			o.Hub, o.Reporter, o.Logger = hub, reports, slog.New(slog.NewJSONHandler(logs, nil))
			c := dial(t, startServer(t, o))
			err := send(t, c, "bounce@mail.example.com", to, tc.msg)
			var se *smtp.SMTPError
			if !errors.As(err, &se) || se.Code != tc.code || !strings.Contains(se.Message, tc.reply) {
				t.Fatalf("reply %v, want %d ...%s...", err, tc.code, tc.reply)
			}
			// A refusal about the message says how to try again, where it is
			// explained and where to report a problem (#18744).
			if wire.Fix(tc.reason) != "" {
				for _, want := range []string{"To try again: " + wire.Fix(tc.reason), wire.HelpURL(tc.reason), "Report a problem: " + wire.ReportURL} {
					if !strings.Contains(se.Message, want) {
						t.Errorf("reply %q lacks %q", se.Message, want)
					}
				}
			} else if strings.Contains(se.Message, wire.HelpPageURL) {
				t.Errorf("a receiver failure carries the help page: %q", se.Message)
			}
			if len(hub.deliveries()) != 0 {
				t.Fatalf("delivered %+v", hub.deliveries())
			}
			l := logs.waitLines(t, "hive-mx: message "+tc.outcome, 1)[0]
			if l["level"] != "INFO" || l["reason"] != tc.reason || l["code"] != float64(tc.code) || l["outcome"] != tc.outcome {
				t.Fatalf("outcome line %+v", l)
			}
			if hosts, _ := l["rcpt_hosts"].([]any); len(hosts) != 1 || hosts[0] != testHost {
				t.Fatalf("rcpt_hosts %+v", l["rcpt_hosts"])
			}
			if strings.Contains(logs.String(), "bounce@") || strings.Contains(logs.String(), "ada@") || strings.Contains(logs.String(), "Slides") {
				t.Fatalf("the log holds an address or a file name:\n%s", logs.String())
			}
			rep := reports.deliveries()
			if !tc.reported {
				if len(rep) != 0 {
					t.Fatalf("reported %+v", rep)
				}
				return
			}
			if len(rep) != 1 || rep[0].Kind != wire.KindReport || rep[0].Recipient != to[0] || rep[0].Report == nil || rep[0].Files != nil || rep[0].Auth.From != "" {
				t.Fatalf("reports %+v", rep)
			}
			if r := rep[0].Report; r.Reason != tc.reason || r.Code != tc.code || r.SenderDomain == "" || strings.Contains(r.SenderDomain, "@") {
				t.Fatalf("report %+v", r)
			}
		})
	}

	// The reply of a refused attachment names the accepted types, and the
	// report lists the extension and the reason only.
	reports := &fakeHub{}
	c := dial(t, startServer(t, Options{Hub: &fakeHub{}, Reporter: reports}))
	err := send(t, c, "bounce@example.com", to, buildMail("ada@example.com", "deck", docAttachment("Slides.key", []byte("keynote"))))
	if err == nil || !strings.Contains(err.Error(), "Accepted types: "+AcceptedExtensions) || !strings.Contains(AcceptedExtensions, "pptx") {
		t.Fatalf("reply %v", err)
	}
	rep := reports.deliveries()
	if len(rep) != 1 || rep[0].Report.Attachments != 1 || len(rep[0].Report.Skipped) != 1 || rep[0].Report.Skipped[0] != (wire.SkippedFile{Ext: "key", Reason: wire.SkipType}) || rep[0].Report.SenderDomain != "example.com" {
		t.Fatalf("report %+v", rep)
	}

	// A hub or hive that does not take reports does not change the reply.
	down := &fakeHub{err: func(wire.Delivery) error { return fmt.Errorf("%w: HTTP 400", ErrRejected) }}
	c = dial(t, startServer(t, Options{Hub: &fakeHub{}, Reporter: down}))
	if err := send(t, c, "bounce@example.com", to, buildMail("ada@example.com", "deck", docAttachment("Slides.key", []byte("keynote")))); smtpCode(err) != 550 {
		t.Fatalf("reply with a failing reporter: %v", err)
	}
}

// TestSMTPRefusalsBeforeDataAreLogged (hivecommons/hive#18699): a refusal
// at MAIL FROM or RCPT TO, and a session that ends without a message, each
// leave an INFO line, so "never arrived" and "refused" can be told apart.
func TestSMTPRefusalsBeforeDataAreLogged(t *testing.T) {
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	addr := startServer(t, Options{Hub: &fakeHub{}, Logger: logger})
	c := dial(t, addr)
	if err := c.Mail("bounce@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hive@unknown.hive.hivecommons.dev", nil); smtpCode(err) != 550 {
		t.Fatalf("unknown host: %v", err)
	}
	if err := c.Rcpt("bob@"+testHost, nil); smtpCode(err) != 550 || !strings.Contains(err.Error(), "hive@<its host>") {
		t.Fatalf("unknown mailbox: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}
	refused := logs.waitLines(t, "hive-mx: command refused", 2)
	if l := refused[0]; l["reason"] != reasonUnknownRecipient || l["rcpt_host"] != "unknown.hive.hivecommons.dev" || l["code"] != float64(550) || l["sender_domain"] != "example.com" || l["stage"] != stageRcpt {
		t.Fatalf("unknown recipient line %+v", l)
	}
	if l := refused[1]; l["reason"] != reasonUnknownMailbox || l["rcpt_host"] != testHost {
		t.Fatalf("unknown mailbox line %+v", l)
	}
	if l := logs.waitLines(t, "hive-mx: session ended without a message", 1)[0]; l["stage"] != stageMail || l["level"] != "INFO" {
		t.Fatalf("session line %+v", l)
	}
	if strings.Contains(logs.String(), "bounce@") || strings.Contains(logs.String(), "bob@") {
		t.Fatalf("the log holds an address:\n%s", logs.String())
	}

	// Host list not loaded, and a rate limit.
	logs2 := &logBuffer{}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	addr = startServer(t, Options{Hub: &fakeHub{}, Hosts: &HostSet{}, Logger: slog.New(slog.NewJSONHandler(logs2, nil)),
		IPLimiter: NewRateLimiter(1, time.Hour, 1, func() time.Time { return now })})
	c = dial(t, addr)
	if err := c.Mail("bounce@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("hive@"+testHost, nil); smtpCode(err) != 451 {
		t.Fatalf("host list not loaded: %v", err)
	}
	_ = c.Reset()
	if err := c.Mail("bounce@example.com", nil); smtpCode(err) != 450 {
		t.Fatalf("rate limit: %v", err)
	}
	refused = logs2.waitLines(t, "hive-mx: command refused", 2)
	if refused[0]["reason"] != reasonHostsNotLoaded || refused[0]["outcome"] != outcomeDeferred || refused[1]["reason"] != reasonRateIP || refused[1]["stage"] != stageMail {
		t.Fatalf("refusal lines %+v", refused)
	}

	// A session that delivered a message leaves no "without a message" line.
	logs3 := &logBuffer{}
	c = dial(t, startServer(t, Options{Hub: &fakeHub{}, Logger: slog.New(slog.NewJSONHandler(logs3, nil))}))
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, inviteMail(t, "google.ics")); err != nil {
		t.Fatal(err)
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}
	logs3.waitLines(t, "hive-mx: message accepted", 1)
	const settle = 100 * time.Millisecond
	time.Sleep(settle)
	if got := logs3.lines(t, "hive-mx: session ended without a message"); len(got) != 0 {
		t.Fatalf("a delivered session logged %+v", got)
	}
}
