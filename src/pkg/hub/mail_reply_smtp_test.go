package hub

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-smtp"
)

// Tests of the real reply sender (#18746): a fake DNS and a fake SMTP server
// on the loopback interface, no network. Test keys are made in memory, never
// written anywhere but the test's own temp directory.

const (
	testSMTPHelo      = "hub.hivecommons.dev"
	testSMTPMXHost    = "mx.example.org"
	testSMTPMXIP      = "203.0.113.25"
	testSMTPSelector  = "s1"
	testSMTPDomain    = "hive.hivecommons.dev"
	testSMTPWait      = 10 * time.Second
	smtpTestRSABits   = 2048
	smtpTestCertValid = time.Hour
)

var (
	smtpRSAKeyOnce sync.Once
	smtpRSAKey     *rsa.PrivateKey
)

func sharedTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	smtpRSAKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, smtpTestRSABits)
		if err == nil {
			smtpRSAKey = k
		}
	})
	if smtpRSAKey == nil {
		t.Fatal("test key generation failed")
	}
	return smtpRSAKey
}

// fakeMailDNS answers MX, address and DKIM key lookups from maps.
type fakeMailDNS struct {
	mu      sync.Mutex
	mx      map[string][]*net.MX
	mxErr   error
	ips     map[string][]net.IPAddr
	ipErr   error
	txt     map[string][]string
	mxCalls []string
	ipCalls []string
}

func (f *fakeMailDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mxCalls = append(f.mxCalls, name)
	return f.mx[name], f.mxErr
}

func (f *fakeMailDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ipCalls = append(f.ipCalls, host)
	return f.ips[host], f.ipErr
}

func (f *fakeMailDNS) lookupTXT(name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, errors.New("no such record")
}

// fakeMX is an SMTP server that records what it is sent.
type fakeMX struct {
	mu       sync.Mutex
	ln       net.Listener
	sessions int
	mailFrom []string
	rcpts    []string
	msgs     [][]byte
	tlsOn    []bool
	rcptErr  error
	mailErr  error
}

func (f *fakeMX) snapshot() (sessions int, from, rcpts []string, msgs [][]byte, tlsOn []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, append([]string(nil), f.mailFrom...), append([]string(nil), f.rcpts...),
		append([][]byte(nil), f.msgs...), append([]bool(nil), f.tlsOn...)
}

type fakeMXBackend struct{ f *fakeMX }

func (b *fakeMXBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	// go-smtp makes a second session once STARTTLS has run, so only the
	// plain-text one counts: that is one per connection.
	if _, on := c.TLSConnectionState(); !on {
		b.f.mu.Lock()
		b.f.sessions++
		b.f.mu.Unlock()
	}
	return &fakeMXSession{f: b.f, conn: c}, nil
}

type fakeMXSession struct {
	f    *fakeMX
	conn *smtp.Conn
}

func (s *fakeMXSession) Reset()        {}
func (s *fakeMXSession) Logout() error { return nil }

func (s *fakeMXSession) Mail(from string, _ *smtp.MailOptions) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.mailErr != nil {
		return s.f.mailErr
	}
	_, on := s.conn.TLSConnectionState()
	s.f.tlsOn = append(s.f.tlsOn, on)
	s.f.mailFrom = append(s.f.mailFrom, from)
	return nil
}

func (s *fakeMXSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.rcptErr != nil {
		return s.f.rcptErr
	}
	s.f.rcpts = append(s.f.rcpts, to)
	return nil
}

func (s *fakeMXSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.f.mu.Lock()
	s.f.msgs = append(s.f.msgs, b)
	s.f.mu.Unlock()
	return nil
}

// testServerCert is a self-signed certificate for the MX host name and the
// pool that trusts it.
func testServerCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: testSMTPMXHost},
		DNSNames:              []string{testSMTPMXHost},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(smtpTestCertValid),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, pool
}

type smtpEnv struct {
	dns     *fakeMailDNS
	mx      *fakeMX
	sender  *smtpReplySender
	keyPath string
	dials   *atomic.Int64
	dialed  *atomic.Value
}

// newSMTPEnv is a sender with a signing key of the given algorithm (rsa or
// ed25519), a fake DNS that publishes its public key and names one MX host,
// and a fake SMTP server (with STARTTLS when withTLS).
func newSMTPEnv(t *testing.T, algo string, withTLS bool) *smtpEnv {
	t.Helper()
	var signer crypto.Signer
	var txt string
	switch algo {
	case "ed25519":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer = priv
		txt = "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
	default:
		k := sharedTestRSAKey(t)
		signer = k
		der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		txt = "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "dkim.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	dns := &fakeMailDNS{
		mx:  map[string][]*net.MX{"example.org": {{Host: testSMTPMXHost + ".", Pref: 10}}},
		ips: map[string][]net.IPAddr{testSMTPMXHost: {{IP: net.ParseIP(testSMTPMXIP)}}},
		txt: map[string][]string{testSMTPSelector + "._domainkey." + testSMTPDomain: {txt}},
	}

	mx := &fakeMX{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mx.ln = ln
	srv := smtp.NewServer(&fakeMXBackend{f: mx})
	srv.Domain = testSMTPMXHost
	srv.ReadTimeout, srv.WriteTimeout = testSMTPWait, testSMTPWait
	srv.MaxMessageBytes = 1 << 20
	pool := x509.NewCertPool()
	if withTLS {
		cert, p := testServerCert(t)
		pool = p
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	e := &smtpEnv{dns: dns, mx: mx, keyPath: keyPath, dials: &atomic.Int64{}, dialed: &atomic.Value{}}
	e.sender = &smtpReplySender{
		from: testReplyFrom, selector: testSMTPSelector, keyFile: keyPath, helo: testSMTPHelo,
		resolver: dns,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			e.dials.Add(1)
			e.dialed.Store(address)
			d := net.Dialer{Timeout: testSMTPWait}
			return d.DialContext(ctx, network, ln.Addr().String())
		},
		tlsConfig: func(name string) *tls.Config {
			return &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12, RootCAs: pool}
		},
		now:            time.Now,
		sessionTimeout: testSMTPWait,
	}
	return e
}

func testReplyMessage(title string) mailReplyMessage {
	return renderMailReply(mailReplyReceived, testReplyHost, testReplyFrom, testReplySender, title, testReplyMsgID)
}

func sendErrReason(t *testing.T, err error) (string, int) {
	t.Helper()
	var se *mailReplySendError
	if !errors.As(err, &se) {
		t.Fatalf("not a send error: %v", err)
	}
	return se.Reason, se.Code
}

func (e *smtpEnv) sessions() int {
	n, _, _, _, _ := e.mx.snapshot()
	return n
}

// The accepted case, for both key types: one connection to the MX host on
// port 25, STARTTLS, a null envelope sender, one recipient, a plain-text
// message that every rule-3 header is on, and a DKIM signature that verifies
// against the published test key.
func TestSMTPReplySenderDeliversSignedPlainText(t *testing.T) {
	for _, algo := range []string{"rsa", "ed25519"} {
		t.Run(algo, func(t *testing.T) {
			e := newSMTPEnv(t, algo, true)
			if err := e.sender.Send(context.Background(), testReplyMessage("Standup notes")); err != nil {
				t.Fatal(err)
			}
			sessions, from, rcpts, msgs, tlsOn := e.mx.snapshot()
			if sessions != 1 || len(msgs) != 1 || len(from) != 1 {
				t.Fatalf("sessions=%d messages=%d mail-from=%d", sessions, len(msgs), len(from))
			}
			if from[0] != "" {
				t.Fatalf("envelope sender %q, want the null sender", from[0])
			}
			if len(rcpts) != 1 || rcpts[0] != testReplySender {
				t.Fatalf("recipients %v", rcpts)
			}
			if len(tlsOn) != 1 || !tlsOn[0] {
				t.Fatal("the message was not sent over STARTTLS")
			}
			if got, _ := e.dialed.Load().(string); got != net.JoinHostPort(testSMTPMXIP, "25") {
				t.Fatalf("dialed %q, want the MX host's public address on port 25", got)
			}

			vs, err := dkim.VerifyWithOptions(bytes.NewReader(msgs[0]), &dkim.VerifyOptions{LookupTXT: e.dns.lookupTXT})
			if err != nil || len(vs) != 1 {
				t.Fatalf("verify: %v (%d signatures)", err, len(vs))
			}
			if vs[0].Err != nil || vs[0].Domain != testSMTPDomain {
				t.Fatalf("signature: domain %q, error %v", vs[0].Domain, vs[0].Err)
			}
			signed := strings.ToLower(strings.Join(vs[0].HeaderKeys, ","))
			for _, h := range []string{"from", "to", "subject", "date", "message-id", "in-reply-to", "auto-submitted"} {
				if !strings.Contains(signed, h) {
					t.Errorf("header %q is not signed (%s)", h, signed)
				}
			}

			// A changed body no longer verifies.
			tampered := bytes.Replace(msgs[0], []byte("reached"), []byte("reachEd"), 1)
			vs, err = dkim.VerifyWithOptions(bytes.NewReader(tampered), &dkim.VerifyOptions{LookupTXT: e.dns.lookupTXT})
			if err == nil && len(vs) == 1 && vs[0].Err == nil {
				t.Fatal("a modified message still verified")
			}

			parsed, err := mail.ReadMessage(bytes.NewReader(msgs[0]))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"Auto-Submitted":           "auto-replied",
				"X-Auto-Response-Suppress": "All",
				"Precedence":               "auto_reply",
				"Reply-To":                 "hive@" + testReplyHost,
				"To":                       testReplySender,
				"Subject":                  "Re: Standup notes",
				"In-Reply-To":              "<" + testReplyMsgID + ">",
				"References":               "<" + testReplyMsgID + ">",
				"Content-Type":             `text/plain; charset="utf-8"`,
			}
			for k, v := range want {
				if got := parsed.Header.Get(k); got != v {
					t.Errorf("%s: %q, want %q", k, got, v)
				}
			}
			if f := parsed.Header.Get("From"); !strings.Contains(f, "<"+testReplyFrom+">") {
				t.Errorf("From: %q", f)
			}
			if parsed.Header.Get("Date") == "" || !strings.HasSuffix(parsed.Header.Get("Message-Id"), "@"+testSMTPDomain+">") {
				t.Errorf("Date %q, Message-ID %q", parsed.Header.Get("Date"), parsed.Header.Get("Message-Id"))
			}
			body, _ := io.ReadAll(parsed.Body)
			if !strings.HasPrefix(string(body), "Your mail reached the hive.\r\n") || strings.Contains(strings.ToLower(string(body)), "<html") {
				t.Errorf("body: %q", body)
			}
			if bytes.Contains(msgs[0], []byte("\nBcc:")) || bytes.Contains(msgs[0], []byte("multipart/")) {
				t.Error("unexpected header or MIME structure")
			}
		})
	}
}

// A subject with letters outside ASCII is sent as an encoded word and the
// signature still verifies; the delivered subject decodes to the original.
func TestSMTPReplySenderEncodesNonASCIISubject(t *testing.T) {
	e := newSMTPEnv(t, "ed25519", true)
	if err := e.sender.Send(context.Background(), testReplyMessage("Réunion équipe")); err != nil {
		t.Fatal(err)
	}
	_, _, _, msgs, _ := e.mx.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}
	raw := parsed.Header.Get("Subject")
	if !isASCIIString(raw) || !strings.Contains(raw, "=?") {
		t.Fatalf("subject was not encoded: %q", raw)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil || dec != "Re: Réunion équipe" {
		t.Fatalf("decoded %q, %v", dec, err)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msgs[0]), &dkim.VerifyOptions{LookupTXT: e.dns.lookupTXT})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("verify: %v %v", err, vs)
	}
}

// Header and address injection is refused before any DNS lookup or
// connection, and a hostile title is neutralized by the template.
func TestSMTPReplySenderRefusesInjection(t *testing.T) {
	e := newSMTPEnv(t, "ed25519", true)
	good := testReplyMessage("Standup notes")
	cases := map[string]func(m *mailReplyMessage){
		"subject crlf":      func(m *mailReplyMessage) { m.Subject = "Re: x\r\nBcc: evil@example.net" },
		"subject lf":        func(m *mailReplyMessage) { m.Subject = "Re: x\nBcc: evil@example.net" },
		"subject nul":       func(m *mailReplyMessage) { m.Subject = "Re: x\x00y" },
		"to crlf":           func(m *mailReplyMessage) { m.To = "alice@example.org\r\nRCPT TO:<evil@example.net>" },
		"to angle":          func(m *mailReplyMessage) { m.To = "alice@example.org>,<evil@example.net" },
		"to display name":   func(m *mailReplyMessage) { m.To = "Alice <alice@example.org>" },
		"to non-ascii":      func(m *mailReplyMessage) { m.To = "é@example.org" },
		"reply-to lf":       func(m *mailReplyMessage) { m.ReplyTo = "hive@acme.hive.hivecommons.dev\nBcc: evil@example.net" },
		"from other":        func(m *mailReplyMessage) { m.From = "hive@other.example.net" },
		"from crlf":         func(m *mailReplyMessage) { m.From = testReplyFrom + "\r\nBcc: evil@example.net" },
		"message id space":  func(m *mailReplyMessage) { m.MessageID = "a b" },
		"message id crlf":   func(m *mailReplyMessage) { m.MessageID = "x>\r\nBcc: evil@example.net" },
		"body bare cr":      func(m *mailReplyMessage) { m.Text = "ok\rBcc: evil@example.net\n" },
		"body nul":          func(m *mailReplyMessage) { m.Text = "ok\x00\n" },
		"body non-ascii":    func(m *mailReplyMessage) { m.Text = "café\n" },
		"body long line":    func(m *mailReplyMessage) { m.Text = strings.Repeat("a", mailReplyMaxLine+1) + "\n" },
		"subject too large": func(m *mailReplyMessage) { m.Subject = strings.Repeat("a", mailReplyMaxHeaderValue+1) },
	}
	for name, mutate := range cases {
		m := good
		mutate(&m)
		err := e.sender.Send(context.Background(), m)
		if err == nil {
			t.Errorf("%s: sent", name)
			continue
		}
		if reason, _ := sendErrReason(t, err); reason != sendFailMessage {
			t.Errorf("%s: reason %q", name, reason)
		}
	}
	if n := e.dials.Load(); n != 0 || e.sessions() != 0 {
		t.Fatalf("%d connections made for refused messages", n)
	}
	if len(e.dns.mxCalls) != 0 {
		t.Fatalf("DNS was asked for a refused message: %v", e.dns.mxCalls)
	}

	// The template removes control characters from the sender's subject, so
	// what reaches the wire has no extra header.
	if err := e.sender.Send(context.Background(), testReplyMessage("x\r\nBcc: evil@example.net")); err != nil {
		t.Fatal(err)
	}
	_, _, _, msgs, _ := e.mx.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Bcc") != "" || parsed.Header.Get("Subject") != "Re: x Bcc: evil@example.net" {
		t.Fatalf("Bcc %q, Subject %q", parsed.Header.Get("Bcc"), parsed.Header.Get("Subject"))
	}
}

// A missing, empty, unreadable or weak key means no send and no connection.
func TestSMTPReplySenderKeyUnusable(t *testing.T) {
	e := newSMTPEnv(t, "ed25519", true)
	dir := t.TempDir()
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	weakDER, err := x509.MarshalPKCS8PrivateKey(weak)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	paths := map[string]string{
		"unset":     "",
		"missing":   filepath.Join(dir, "absent.pem"),
		"directory": dir,
		"empty":     write("empty.pem", nil),
		"garbage":   write("garbage.pem", []byte("not a key at all")),
		"bad pem":   write("badpem.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})),
		"public":    write("pub.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: weakDER})),
		"weak rsa":  write("weak.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: weakDER})),
		"ecdsa":     write("ec.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})),
		"too large": write("big.pem", bytes.Repeat([]byte("A"), mailReplyKeyMaxBytes+1)),
	}
	for name, p := range paths {
		e.sender.keyFile = p
		err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
		if err == nil {
			t.Errorf("%s: sent", name)
			continue
		}
		if reason, _ := sendErrReason(t, err); reason != sendFailKey {
			t.Errorf("%s: reason %q", name, reason)
		}
		if e.sender.Ready() == nil {
			t.Errorf("%s: Ready accepted the key", name)
		}
		// Nothing of the key file is in the error.
		if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: the error names the path: %v", name, err)
		}
	}
	if e.dials.Load() != 0 || e.sessions() != 0 {
		t.Fatal("connected with an unusable key")
	}
	e.sender.keyFile = e.keyPath
	if err := e.sender.Ready(); err != nil {
		t.Fatalf("a good key is not ready: %v", err)
	}
}

// An RSA key in the older PKCS#1 form is accepted.
func TestLoadMailReplyKeyPKCS1(t *testing.T) {
	k := sharedTestRSAKey(t)
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadMailReplyKey(p)
	if err != nil || s == nil {
		t.Fatalf("PKCS#1 key: %v", err)
	}
}

// DNS failures: the lookup erroring, no MX record, a null MX, an MX host
// with no usable address. None connects, none is retried.
func TestSMTPReplySenderMXFailures(t *testing.T) {
	msg := testReplyMessage("Standup notes")
	t.Run("lookup error", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.mx = nil
		e.dns.mxErr = &net.DNSError{Err: "server failure", Name: "example.org", IsTemporary: true}
		reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg))
		if reason != sendFailMXLookup || e.dials.Load() != 0 || len(e.dns.mxCalls) != 1 {
			t.Fatalf("reason %q, %d dials, %d lookups", reason, e.dials.Load(), len(e.dns.mxCalls))
		}
	})
	t.Run("lookup timeout", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.mx = nil
		e.dns.mxErr = &net.DNSError{Err: "i/o timeout", Name: "example.org", IsTimeout: true}
		if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailTimeout {
			t.Fatalf("reason %q", reason)
		}
	})
	t.Run("no MX record", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.mx = map[string][]*net.MX{}
		if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailNoMX || e.dials.Load() != 0 {
			t.Fatalf("reason %q", reason)
		}
	})
	t.Run("null MX", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.mx = map[string][]*net.MX{"example.org": {{Host: ".", Pref: 0}}}
		if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailNoMX || e.dials.Load() != 0 {
			t.Fatalf("reason %q", reason)
		}
	})
	t.Run("MX name not a host", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.mx = map[string][]*net.MX{"example.org": {{Host: "bad host/x.", Pref: 0}}}
		if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailNoMX || e.dials.Load() != 0 {
			t.Fatalf("reason %q", reason)
		}
	})
	t.Run("MX host has no address", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.dns.ips = map[string][]net.IPAddr{}
		e.dns.ipErr = &net.DNSError{Err: "no such host", Name: testSMTPMXHost, IsNotFound: true}
		if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailMXAddr || e.dials.Load() != 0 {
			t.Fatalf("reason %q", reason)
		}
	})
	t.Run("MX host in a private range", func(t *testing.T) {
		for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.0.9", "::1", "fd00::1", "100.64.0.1"} {
			e := newSMTPEnv(t, "ed25519", true)
			e.dns.ips = map[string][]net.IPAddr{testSMTPMXHost: {{IP: net.ParseIP(ip)}}}
			if reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg)); reason != sendFailBlocked || e.dials.Load() != 0 {
				t.Fatalf("%s: reason %q, %d dials", ip, reason, e.dials.Load())
			}
		}
	})
}

// Only the best-preference MX host is used, one connection, no fallback to
// another host when it cannot be reached.
func TestSMTPReplySenderUsesOneMX(t *testing.T) {
	e := newSMTPEnv(t, "ed25519", true)
	e.dns.mx = map[string][]*net.MX{"example.org": {{Host: "backup.example.org.", Pref: 20}, {Host: testSMTPMXHost + ".", Pref: 10}}}
	e.dns.ips["backup.example.org"] = []net.IPAddr{{IP: net.ParseIP("203.0.113.26")}}
	if err := e.sender.Send(context.Background(), testReplyMessage("Standup notes")); err != nil {
		t.Fatal(err)
	}
	if len(e.dns.ipCalls) != 1 || e.dns.ipCalls[0] != testSMTPMXHost || e.dials.Load() != 1 {
		t.Fatalf("address lookups %v, %d dials", e.dns.ipCalls, e.dials.Load())
	}

	// The preferred host refuses the connection: that is final.
	e = newSMTPEnv(t, "ed25519", true)
	e.dns.mx = map[string][]*net.MX{"example.org": {{Host: "backup.example.org.", Pref: 20}, {Host: testSMTPMXHost + ".", Pref: 10}}}
	e.dns.ips["backup.example.org"] = []net.IPAddr{{IP: net.ParseIP("203.0.113.26")}}
	e.sender.dial = func(context.Context, string, string) (net.Conn, error) {
		e.dials.Add(1)
		return nil, errors.New("connection refused to 203.0.113.25")
	}
	err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
	reason, _ := sendErrReason(t, err)
	if reason != sendFailDial || e.dials.Load() != 1 || len(e.dns.ipCalls) != 1 {
		t.Fatalf("reason %q, %d dials, lookups %v", reason, e.dials.Load(), e.dns.ipCalls)
	}
	if strings.Contains(err.Error(), "203.0.113") {
		t.Fatalf("the error carries the network error's text: %v", err)
	}
}

// An address of the MX host that is not public is skipped; an IPv4 address
// is preferred over an IPv6 one.
func TestPickMailAddr(t *testing.T) {
	ip, ok := pickMailAddr([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}, {IP: net.ParseIP("2001:4860::1")}, {IP: net.ParseIP("203.0.113.9")}})
	if !ok || ip.String() != "203.0.113.9" {
		t.Fatalf("picked %v %v", ip, ok)
	}
	ip, ok = pickMailAddr([]net.IPAddr{{IP: net.ParseIP("2001:4860::1")}})
	if !ok || ip.String() != "2001:4860::1" {
		t.Fatalf("picked %v %v", ip, ok)
	}
	if _, ok = pickMailAddr([]net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("fe80::1")}}); ok {
		t.Fatal("picked a non-public address")
	}
	if mailReplyPublicIP(nil) || mailReplyPublicIP(net.ParseIP("0.0.0.0")) || mailReplyPublicIP(net.ParseIP("::ffff:10.0.0.1")) ||
		mailReplyPublicIP(net.ParseIP("224.0.0.1")) || mailReplyPublicIP(net.ParseIP("198.18.0.1")) {
		t.Fatal("a special-purpose address counted as public")
	}
	if !mailReplyPublicIP(net.ParseIP("8.8.8.8")) || !mailReplyPublicIP(net.ParseIP("2606:4700::1111")) {
		t.Fatal("a public address was refused")
	}
}

// A server that never answers is cut off at the session timeout, and a
// cancelled context stops the send at once.
func TestSMTPReplySenderTimeouts(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c)
			heldMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	dialSilent := func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: testSMTPWait}
		return d.DialContext(ctx, network, silent.Addr().String())
	}

	t.Run("session deadline", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.sender.dial = dialSilent
		e.sender.sessionTimeout = 200 * time.Millisecond
		start := time.Now()
		reason, _ := sendErrReason(t, e.sender.Send(context.Background(), testReplyMessage("Standup notes")))
		if reason != sendFailTimeout {
			t.Fatalf("reason %q", reason)
		}
		if time.Since(start) > testSMTPWait {
			t.Fatal("the timeout was not honoured")
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.sender.dial = dialSilent
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		reason, _ := sendErrReason(t, e.sender.Send(ctx, testReplyMessage("Standup notes")))
		if reason != sendFailTimeout || time.Since(start) > testSMTPWait {
			t.Fatalf("reason %q after %v", reason, time.Since(start))
		}
	})
}

// A refused recipient (or sender) is a final failure with the server's code
// and no message data sent; a server that offers no STARTTLS, or whose
// certificate does not verify, gets nothing.
func TestSMTPReplySenderServerRefusals(t *testing.T) {
	msg := testReplyMessage("Standup notes")
	t.Run("recipient rejected", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.mx.rcptErr = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user alice@example.org"}
		err := e.sender.Send(context.Background(), msg)
		reason, code := sendErrReason(t, err)
		if reason != sendFailRcpt || code != 550 {
			t.Fatalf("reason %q code %d", reason, code)
		}
		if strings.Contains(err.Error(), "alice") {
			t.Fatalf("the error carries the server's text: %v", err)
		}
		sessions, _, _, msgs, _ := e.mx.snapshot()
		if sessions != 1 || len(msgs) != 0 {
			t.Fatalf("%d sessions, %d messages", sessions, len(msgs))
		}
	})
	t.Run("sender rejected", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.mx.mailErr = &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "null sender refused"}
		reason, code := sendErrReason(t, e.sender.Send(context.Background(), msg))
		if reason != sendFailMailFrom || code != 554 {
			t.Fatalf("reason %q code %d", reason, code)
		}
	})
	t.Run("no STARTTLS", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", false)
		reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg))
		sessions, from, _, msgs, _ := e.mx.snapshot()
		if reason != sendFailNoTLS || sessions != 1 || len(from) != 0 || len(msgs) != 0 {
			t.Fatalf("reason %q, %d sessions, %d senders, %d messages", reason, sessions, len(from), len(msgs))
		}
	})
	t.Run("certificate not trusted", func(t *testing.T) {
		e := newSMTPEnv(t, "ed25519", true)
		e.sender.tlsConfig = func(name string) *tls.Config { return &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12} }
		reason, _ := sendErrReason(t, e.sender.Send(context.Background(), msg))
		_, from, _, msgs, _ := e.mx.snapshot()
		if reason != sendFailTLS || len(from) != 0 || len(msgs) != 0 {
			t.Fatalf("reason %q, %d senders, %d messages", reason, len(from), len(msgs))
		}
	})
}

type replyWithSender struct {
	*smtpEnv
	r      *mailReplier
	logs   *bytes.Buffer
	ledger string
	now    time.Time
}

func newReplierWithSMTP(t *testing.T, lim mailReplyLimits) *replyWithSender {
	t.Helper()
	e := newSMTPEnv(t, "ed25519", true)
	w := &replyWithSender{smtpEnv: e, logs: &bytes.Buffer{}, ledger: filepath.Join(t.TempDir(), "ledger.jsonl"), now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	cfg := mailReplyConfig{From: testReplyFrom, Selector: testSMTPSelector, KeyFile: e.keyPath, Helo: testSMTPHelo, Limits: lim}
	logger := slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r, err := newMailReplier(cfg, e.sender, w.ledger, logger,
		func() map[string]string { return map[string]string{testReplyHost: testReplyHive} }, func() time.Time { return w.now })
	if err != nil {
		t.Fatal(err)
	}
	w.r = r
	return w
}

func (w *replyWithSender) candidate(msgID, sender string) mailReplyCandidate {
	in := goodInput()
	in.MessageID = msgID
	in.Auth.From = sender
	c, reason := w.r.decideMailReply(in, goodAnswer(), testReplyHive, testReplyHost)
	if reason != "" {
		panic("test candidate refused: " + reason)
	}
	return c
}

// The ledger's limits are applied before anything is sent: past a limit the
// server sees no connection at all.
func TestSMTPReplyLimitsApplyBeforeSending(t *testing.T) {
	w := newReplierWithSMTP(t, mailReplyLimits{SenderPerHour: 1, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200})
	if got := w.r.handle(context.Background(), w.candidate("m1@mail.example.org", testReplySender)); got != mailReplyStatSent {
		t.Fatalf("first reply: %q", got)
	}
	if n := w.sessions(); n != 1 {
		t.Fatalf("%d sessions after the first reply", n)
	}
	if got := w.r.handle(context.Background(), w.candidate("m2@mail.example.org", testReplySender)); got != skipSenderHour {
		t.Fatalf("second reply for the same sender: %q", got)
	}
	if n := w.dials.Load(); n != 1 {
		t.Fatalf("%d connections: the reply past the limit was still dialed", n)
	}
	// The same message again is a duplicate, not a second reply.
	if got := w.r.handle(context.Background(), w.candidate("m1@mail.example.org", testReplySender)); got != skipDuplicate || w.dials.Load() != 1 {
		t.Fatalf("duplicate: %q, %d connections", got, w.dials.Load())
	}
}

// A failed send is final: it is not retried, a second delivery of the same
// message does not send, and the log holds ids and a reason class only.
func TestSMTPReplyFailureIsFinalAndLogsNothingPrivate(t *testing.T) {
	w := newReplierWithSMTP(t, mailReplyLimits{SenderPerHour: 10, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200})
	w.mx.rcptErr = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user alice@example.org"}
	c := w.candidate("m1@mail.example.org", testReplySender)
	if got := w.r.handle(context.Background(), c); got != skipSendFailed {
		t.Fatalf("got %q", got)
	}
	w.mx.rcptErr = nil
	if got := w.r.handle(context.Background(), c); got != skipDuplicate {
		t.Fatalf("a failed reply was tried again: %q", got)
	}
	if n := w.sessions(); n != 1 {
		t.Fatalf("%d sessions, want exactly one", n)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "recipient_refused") || !strings.Contains(logs, "smtp_code=550") || !strings.Contains(logs, testDeliveryID) {
		t.Fatalf("log lacks the reason class, code or delivery id: %s", logs)
	}
	key, err := os.ReadFile(w.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Split(strings.TrimSpace(string(key)), "\n")[1]
	for _, secret := range []string{"alice", "example.org", "Standup", testReplyMsgID, "m1@", "PRIVATE KEY", body[:24], "Your mail reached"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log contains %q: %s", secret, logs)
		}
	}
}

// A key that cannot be loaded sends nothing and claims nothing: once the key
// is fixed the same message is still answered, once.
func TestSMTPReplyUnusableKeyClaimsNothing(t *testing.T) {
	w := newReplierWithSMTP(t, mailReplyLimits{SenderPerHour: 10, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200})
	good, err := os.ReadFile(w.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := w.candidate("m1@mail.example.org", testReplySender)
	if got := w.r.handle(context.Background(), c); got != skipKeyUnusable {
		t.Fatalf("unusable key: %q", got)
	}
	if w.dials.Load() != 0 {
		t.Fatal("connected with an unusable key")
	}
	// Removed altogether: the replier is simply off.
	if err := os.Remove(w.keyPath); err != nil {
		t.Fatal(err)
	}
	if got := w.r.handle(context.Background(), c); got != "disabled" || w.dials.Load() != 0 {
		t.Fatalf("missing key: %q, %d connections", got, w.dials.Load())
	}
	if err := os.WriteFile(w.keyPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := w.r.handle(context.Background(), c); got != mailReplyStatSent {
		t.Fatalf("after the key is fixed: %q", got)
	}
	if got := w.r.handle(context.Background(), c); got != skipDuplicate || w.sessions() != 1 {
		t.Fatalf("second try: %q, %d sessions", got, w.sessions())
	}
}

// Off by default: no key, nothing is built and nothing is dialed; a key
// without the HELO name builds nothing; the real sender is what a fully
// configured hub gets; and a configured hub whose key file is gone still
// sends nothing.
func TestSMTPReplyOffUnlessConfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	key := filepath.Join(dir, "absent.pem")
	t.Setenv(envMailReplyLedger, filepath.Join(dir, "l.jsonl"))
	for _, k := range []string{envMailReplyKeyFile, envMailReplyFrom, envMailReplySelector, envMailReplyHelo} {
		t.Setenv(k, "")
	}
	old := mailReplySenderFactory
	t.Cleanup(func() { mailReplySenderFactory = old })
	built := 0
	mailReplySenderFactory = func(cfg mailReplyConfig) (mailReplySender, error) {
		built++
		return old(cfg)
	}
	if newMailReplierFromEnv(logger, nil) != nil || built != 0 {
		t.Fatal("a replier, or a sender, without any configuration")
	}
	// The other settings alone do not turn it on either.
	t.Setenv(envMailReplyFrom, testReplyFrom)
	t.Setenv(envMailReplySelector, testSMTPSelector)
	t.Setenv(envMailReplyHelo, testSMTPHelo)
	if newMailReplierFromEnv(logger, nil) != nil || built != 0 {
		t.Fatal("a replier without the key setting")
	}
	t.Setenv(envMailReplyKeyFile, key)
	t.Setenv(envMailReplyHelo, "")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier without a HELO name")
	}
	t.Setenv(envMailReplyHelo, "not a host name")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier with an invalid HELO name")
	}
	t.Setenv(envMailReplyHelo, "localhost")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier with a HELO name that is not a fully qualified host")
	}
	t.Setenv(envMailReplyHelo, testSMTPHelo)
	r := newMailReplierFromEnv(logger, nil)
	if r == nil {
		t.Fatal("no replier with a complete configuration")
	}
	s, ok := r.sender.(*smtpReplySender)
	if !ok {
		t.Fatalf("sender is %T", r.sender)
	}
	if s.helo != testSMTPHelo || s.selector != testSMTPSelector || s.from != testReplyFrom || s.keyFile != key {
		t.Fatalf("sender configuration: %+v", s)
	}
	// The key file does not exist: still off, and the network is never used.
	var dials atomic.Int64
	s.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}
	if r.enabled() {
		t.Fatal("enabled without the key file")
	}
	c := mailReplyCandidate{HiveID: testReplyHive, Host: testReplyHost, DeliveryID: testDeliveryID, MessageID: testReplyMsgID, To: testReplySender, Domain: "example.org", Outcome: mailReplyReceived}
	if got := r.handle(context.Background(), c); got != "disabled" || dials.Load() != 0 {
		t.Fatalf("handle: %q, %d dials", got, dials.Load())
	}
	r.dispatch(mailReplyJob{HiveID: testReplyHive, Host: testReplyHost, Spool: bytes.NewReader([]byte("{}")), Size: 2, Answer: []byte(`{"items":["x"]}`)})
	if dials.Load() != 0 {
		t.Fatal("dispatch dialed while the key file is missing")
	}
	// A nil replier, as on every hub without the key, never reaches a sender.
	var none *mailReplier
	none.dispatch(mailReplyJob{})
}

func TestMailReplyFailureClass(t *testing.T) {
	if r, c := mailReplyFailure(&mailReplySendError{Reason: sendFailRcpt, Code: 550}); r != sendFailRcpt || c != 550 {
		t.Fatalf("%q %d", r, c)
	}
	if r, c := mailReplyFailure(errors.New("boom 10.0.0.1")); r != sendFailInternal || c != 0 {
		t.Fatalf("%q %d", r, c)
	}
	if r, _ := mailReplyFailure(&mailReplySendError{Reason: sendFailTimeout}); r != sendFailTimeout {
		t.Fatal(r)
	}
}
