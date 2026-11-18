package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Tests of the optional smarthost (relay) mode of the reply sender (#18746):
// the same fake DNS and fake SMTP server as the direct-delivery tests, a relay
// that speaks submission with optional AUTH, no network.

const (
	testRelayUser = "relay-user-7f3a"
	testRelayPass = "relay-pass-Zx9q-secret"
)

// relayBackend is a fakeMXBackend that can also offer and require AUTH PLAIN.
type relayBackend struct {
	f           *fakeMX
	offerAuth   bool
	requireAuth bool

	mu       sync.Mutex
	authSeen []string // "user:pass" of every AUTH attempt, as the server saw it
	authOK   int
}

func (b *relayBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	inner, err := (&fakeMXBackend{f: b.f}).NewSession(c)
	if err != nil {
		return nil, err
	}
	return &relaySession{Session: inner, b: b}, nil
}

type relaySession struct {
	smtp.Session
	b      *relayBackend
	authed bool
}

func (s *relaySession) AuthMechanisms() []string {
	if !s.b.offerAuth {
		return nil
	}
	return []string{sasl.Plain}
}

func (s *relaySession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		s.b.mu.Lock()
		defer s.b.mu.Unlock()
		s.b.authSeen = append(s.b.authSeen, user+":"+pass)
		if user != testRelayUser || pass != testRelayPass {
			return errors.New("bad credentials")
		}
		s.authed = true
		s.b.authOK++
		return nil
	}), nil
}

func (s *relaySession) Mail(from string, o *smtp.MailOptions) error {
	if s.b.requireAuth && !s.authed {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "authentication required"}
	}
	return s.Session.Mail(from, o)
}

func (b *relayBackend) seen() (attempts []string, ok int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.authSeen...), b.authOK
}

type relayEnv struct {
	*smtpEnv
	backend  *relayBackend
	authFile string
	address  atomic.Value // last dialed address, host:port as the sender asked
	dialed   atomic.Int64
}

// newRelayEnv is a sender configured with a relay at the fake MX host name (the
// test certificate is for that name). withTLS makes the relay offer STARTTLS;
// auth makes it offer and require AUTH PLAIN and writes the credentials file.
func newRelayEnv(t *testing.T, withTLS, auth bool) *relayEnv {
	t.Helper()
	base := newSMTPEnv(t, "ed25519", true)
	f := &fakeMX{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &relayBackend{f: f, offerAuth: auth, requireAuth: auth}
	srv := smtp.NewServer(b)
	srv.Domain = testSMTPMXHost
	srv.ReadTimeout, srv.WriteTimeout = testSMTPWait, testSMTPWait
	srv.MaxMessageBytes = 1 << 20
	// Let the server offer AUTH even without TLS, so a sender that sent
	// credentials before STARTTLS would be caught.
	srv.AllowInsecureAuth = true
	pool := x509.NewCertPool()
	if withTLS {
		cert, p := testServerCert(t)
		pool = p
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	e := &relayEnv{smtpEnv: base, backend: b}
	e.mx = f
	host, port, err := parseMailReplyRelay(testSMTPMXHost)
	if err != nil {
		t.Fatal(err)
	}
	e.sender.relayHost, e.sender.relayPort = host, port
	e.sender.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		e.dialed.Add(1)
		e.address.Store(address)
		d := net.Dialer{Timeout: testSMTPWait}
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	e.sender.tlsConfig = func(name string) *tls.Config {
		return &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	if auth {
		e.authFile = filepath.Join(t.TempDir(), "relay-auth")
		writeRelayAuth(t, e.authFile, testRelayUser+"\n"+testRelayPass+"\n")
		e.sender.relayAuthFile = e.authFile
	}
	return e
}

func writeRelayAuth(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Relay unset: nothing changes. The MX of the recipient's domain is looked up
// and its address dialed on port 25, as before; no relay setting is read.
func TestRelayUnsetKeepsDirectDelivery(t *testing.T) {
	e := newSMTPEnv(t, "ed25519", true)
	if e.sender.relayHost != "" || e.sender.relayAuthFile != "" {
		t.Fatalf("relay configured by default: %+v", e.sender)
	}
	if err := e.sender.Send(context.Background(), testReplyMessage("Standup notes")); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.dialed.Load().(string); got != net.JoinHostPort(testSMTPMXIP, "25") {
		t.Fatalf("dialed %q", got)
	}
	if len(e.dns.mxCalls) != 1 || e.dns.mxCalls[0] != "example.org" {
		t.Fatalf("MX lookups %v", e.dns.mxCalls)
	}

	// The factory with the relay settings empty builds the direct sender.
	s, err := newSMTPReplySender(mailReplyConfig{From: testReplyFrom, Selector: testSMTPSelector, KeyFile: e.keyPath, Helo: testSMTPHelo})
	if err != nil {
		t.Fatal(err)
	}
	if rs := s.(*smtpReplySender); rs.relayHost != "" || rs.relayPort != "" || rs.relayAuthFile != "" {
		t.Fatalf("relay set from an empty configuration: %+v", rs)
	}
}

// Relay set: the one connection goes to the relay on the default submission
// port, the recipient's MX is never looked up, the envelope sender is still
// null, STARTTLS ran, and the message is the one DKIM signature the hub made.
func TestRelaySetRoutesToRelay(t *testing.T) {
	e := newRelayEnv(t, true, false)
	if e.sender.relayPort != mailReplyRelayDefaultPort || mailReplyRelayDefaultPort != "587" {
		t.Fatalf("default relay port %q", e.sender.relayPort)
	}
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
	if got, _ := e.address.Load().(string); got != net.JoinHostPort(testSMTPMXIP, "587") {
		t.Fatalf("dialed %q, want the relay on port 587", got)
	}
	if len(e.dns.mxCalls) != 0 {
		t.Fatalf("the recipient's MX was looked up: %v", e.dns.mxCalls)
	}
	if n := strings.Count(string(msgs[0]), "DKIM-Signature:"); n != 1 {
		t.Fatalf("%d DKIM signatures, want the hub's one", n)
	}
}

// A relay is operator-chosen, so a private address is fine (unlike a MX host
// named by a sender), and an explicit port is honored.
func TestRelayAllowsPrivateAddressAndExplicitPort(t *testing.T) {
	e := newRelayEnv(t, true, false)
	e.dns.ips[testSMTPMXHost] = []net.IPAddr{{IP: net.ParseIP("10.20.30.40")}}
	host, port, err := parseMailReplyRelay("Relay-Test.Example.Org.:2525")
	if err != nil || host != "relay-test.example.org" || port != "2525" {
		t.Fatalf("parse: %q %q %v", host, port, err)
	}
	e.sender.relayPort = "2525"
	if err := e.sender.Send(context.Background(), testReplyMessage("Standup notes")); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.address.Load().(string); got != "10.20.30.40:2525" {
		t.Fatalf("dialed %q", got)
	}
}

func TestParseMailReplyRelay(t *testing.T) {
	for _, tc := range []struct{ in, host, port string }{
		{"", "", ""},
		{"  ", "", ""},
		{"smtp.relay.example", "smtp.relay.example", "587"},
		{"smtp.relay.example:465", "smtp.relay.example", "465"},
	} {
		h, p, err := parseMailReplyRelay(tc.in)
		if err != nil || h != tc.host || p != tc.port {
			t.Errorf("%q: %q %q %v", tc.in, h, p, err)
		}
	}
	for _, bad := range []string{"not a host", "smtp.relay.example:", "smtp.relay.example:0", "smtp.relay.example:65536",
		"smtp.relay.example:25x", "smtp.relay.example:+25", "[::1]:25", "a:b:c", ":25", "smtp.relay.example/path"} {
		_, _, err := parseMailReplyRelay(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("the error repeats the value %q: %v", bad, err)
		}
	}
}

// STARTTLS refused fails closed: a relay that does not offer it gets no
// message, no AUTH and no fallback to direct delivery.
func TestRelayWithoutSTARTTLSFailsClosed(t *testing.T) {
	e := newRelayEnv(t, false, true)
	err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
	if reason, _ := sendErrReason(t, err); reason != sendFailNoTLS {
		t.Fatalf("reason %q, want %q", reason, sendFailNoTLS)
	}
	sessions, from, _, msgs, _ := e.mx.snapshot()
	if len(msgs) != 0 || len(from) != 0 || sessions != 1 {
		t.Fatalf("sessions=%d mail-from=%d messages=%d", sessions, len(from), len(msgs))
	}
	if attempts, _ := e.backend.seen(); len(attempts) != 0 {
		t.Fatal("AUTH credentials were sent without STARTTLS")
	}
	if e.dialed.Load() != 1 || len(e.dns.mxCalls) != 0 {
		t.Fatalf("%d connections, MX lookups %v: fell back to direct delivery", e.dialed.Load(), e.dns.mxCalls)
	}
}

// A relay whose certificate is not trusted also fails closed, before AUTH.
func TestRelayUntrustedCertificateFailsClosed(t *testing.T) {
	e := newRelayEnv(t, true, true)
	e.sender.tlsConfig = func(name string) *tls.Config {
		return &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12, RootCAs: x509.NewCertPool()}
	}
	err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
	if reason, _ := sendErrReason(t, err); reason != sendFailTLS {
		t.Fatalf("reason %q, want %q", reason, sendFailTLS)
	}
	_, _, _, msgs, _ := e.mx.snapshot()
	if attempts, _ := e.backend.seen(); len(msgs) != 0 || len(attempts) != 0 {
		t.Fatal("something was sent over a connection whose TLS failed")
	}
}

// AUTH from the file: the credentials are read from the file, used after
// STARTTLS, and the message is accepted only because they were right.
func TestRelayAuthFromFile(t *testing.T) {
	e := newRelayEnv(t, true, true)
	if err := e.sender.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := e.sender.Send(context.Background(), testReplyMessage("Standup notes")); err != nil {
		t.Fatal(err)
	}
	attempts, ok := e.backend.seen()
	if ok != 1 || len(attempts) != 1 || attempts[0] != testRelayUser+":"+testRelayPass {
		t.Fatalf("server saw %v (ok=%d)", attempts, ok)
	}
	_, from, _, msgs, tlsOn := e.mx.snapshot()
	if len(msgs) != 1 || len(from) != 1 || from[0] != "" || len(tlsOn) != 1 || !tlsOn[0] {
		t.Fatalf("messages=%d from=%v tls=%v", len(msgs), from, tlsOn)
	}

	// CRLF line ends and no final newline read the same.
	writeRelayAuth(t, e.authFile, testRelayUser+"\r\n"+testRelayPass)
	if err := e.sender.Send(context.Background(), testReplyMessage("Second")); err != nil {
		t.Fatalf("crlf file: %v", err)
	}
}

func TestRelayAuthFailures(t *testing.T) {
	t.Run("wrong password", func(t *testing.T) {
		e := newRelayEnv(t, true, true)
		writeRelayAuth(t, e.authFile, testRelayUser+"\nnot-the-password\n")
		err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
		if reason, _ := sendErrReason(t, err); reason != sendFailAuthDenied {
			t.Fatalf("reason %q", reason)
		}
		if _, _, _, msgs, _ := e.mx.snapshot(); len(msgs) != 0 {
			t.Fatal("a message was sent after a refused AUTH")
		}
	})
	t.Run("relay does not offer AUTH", func(t *testing.T) {
		e := newRelayEnv(t, true, true)
		e.backend.offerAuth, e.backend.requireAuth = false, false
		err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
		if reason, _ := sendErrReason(t, err); reason != sendFailNoAuth {
			t.Fatalf("reason %q", reason)
		}
		if _, _, _, msgs, _ := e.mx.snapshot(); len(msgs) != 0 {
			t.Fatal("a message was sent to a relay that cannot authenticate")
		}
	})
	t.Run("credentials file unusable", func(t *testing.T) {
		for name, content := range map[string]string{
			"empty":         "",
			"one line":      testRelayUser + "\n",
			"three lines":   "a\nb\nc\n",
			"empty user":    "\n" + testRelayPass + "\n",
			"empty pass":    testRelayUser + "\n\n",
			"nul":           "a\x00b\nc\n",
			"over the cap":  strings.Repeat("x", mailReplyAuthMaxBytes) + "\npass\n",
			"blank padding": "\n\n",
		} {
			e := newRelayEnv(t, true, true)
			writeRelayAuth(t, e.authFile, content)
			if err := e.sender.Ready(); err == nil {
				t.Errorf("%s: ready", name)
			}
			err := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
			if reason, _ := sendErrReason(t, err); reason != sendFailAuthFile {
				t.Errorf("%s: reason %q", name, reason)
			}
			if e.dialed.Load() != 0 {
				t.Errorf("%s: connected with unusable credentials", name)
			}
		}
		e := newRelayEnv(t, true, true)
		if err := os.Remove(e.authFile); err != nil {
			t.Fatal(err)
		}
		if err := e.sender.Ready(); err == nil || e.dialed.Load() != 0 {
			t.Fatal("a missing credentials file was not refused")
		}
	})
}

// No secret reaches an error, a log line or a failed-send report: not the
// password, not the user name, not the credentials file's path.
func TestRelayNoSecretInErrorsOrLogs(t *testing.T) {
	e := newRelayEnv(t, true, true)
	writeRelayAuth(t, e.authFile, testRelayUser+"\n"+testRelayPass+"-wrong\n")

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := mailReplyConfig{From: testReplyFrom, Selector: testSMTPSelector, KeyFile: e.keyPath, Helo: testSMTPHelo,
		Relay: testSMTPMXHost, RelayAuthFile: e.authFile,
		Limits: mailReplyLimits{SenderPerHour: 10, SenderPerDay: 30, DomainPerDay: 100, HivePerDay: 200}}
	r, err := newMailReplier(cfg, e.sender, ledger, logger,
		func() map[string]string { return map[string]string{testReplyHost: testReplyHive} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := &replyWithSender{smtpEnv: e.smtpEnv, r: r, logs: logs, ledger: ledger}
	if got := w.r.handle(context.Background(), w.candidate("m1@mail.example.org", testReplySender)); got != skipSendFailed {
		t.Fatalf("got %q", got)
	}

	sendErr := e.sender.Send(context.Background(), testReplyMessage("Standup notes"))
	if sendErr == nil {
		t.Fatal("a wrong password was accepted")
	}
	reports := []string{logs.String(), sendErr.Error(), fmt.Sprintf("%+v", sendErr), fmt.Sprintf("%#v", sendErr)}
	for _, bad := range []string{"", "x", "a:b", "smtp.relay.example:99999"} {
		_, _, perr := parseMailReplyRelay(bad)
		if perr != nil {
			reports = append(reports, perr.Error())
		}
	}
	_, _, loadErr := loadMailReplyRelayAuth(e.authFile + ".missing")
	reports = append(reports, loadErr.Error(), fmt.Sprintf("%+v", loadErr))
	if !strings.Contains(logs.String(), sendFailAuthDenied) {
		t.Fatalf("the log lacks the reason class: %s", logs.String())
	}
	for _, report := range reports {
		for _, secret := range []string{testRelayPass, testRelayUser, "-wrong", e.authFile, filepath.Base(e.authFile), "AUTH PLAIN", "Standup", "alice"} {
			if strings.Contains(report, secret) {
				t.Fatalf("%q appears in %q", secret, report)
			}
		}
	}
}

// The environment: a relay setting that cannot be used builds no sender (it
// never falls back to direct delivery), a credentials file needs a relay, and
// the valid combination builds a relay sender.
func TestRelayEnvironment(t *testing.T) {
	dir := t.TempDir()
	e := newSMTPEnv(t, "ed25519", true)
	auth := filepath.Join(dir, "relay-auth")
	writeRelayAuth(t, auth, testRelayUser+"\n"+testRelayPass+"\n")
	t.Setenv(envMailReplyLedger, filepath.Join(dir, "l.jsonl"))
	t.Setenv(envMailReplyKeyFile, e.keyPath)
	t.Setenv(envMailReplyFrom, testReplyFrom)
	t.Setenv(envMailReplySelector, testSMTPSelector)
	t.Setenv(envMailReplyHelo, testSMTPHelo)
	t.Setenv(envMailReplyRelay, "")
	t.Setenv(envMailReplyRelayAuthFile, "")

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	r := newMailReplierFromEnv(logger, nil)
	if r == nil {
		t.Fatal("no replier without a relay")
	}
	if s := r.sender.(*smtpReplySender); s.relayHost != "" || s.relayAuthFile != "" {
		t.Fatalf("relay set by default: %+v", s)
	}

	t.Setenv(envMailReplyRelay, "smtp.relay.example")
	r = newMailReplierFromEnv(logger, nil)
	if r == nil {
		t.Fatal("no replier with a relay")
	}
	s := r.sender.(*smtpReplySender)
	if s.relayHost != "smtp.relay.example" || s.relayPort != "587" || s.relayAuthFile != "" {
		t.Fatalf("relay sender: %+v", s)
	}

	t.Setenv(envMailReplyRelay, "smtp.relay.example:2525")
	t.Setenv(envMailReplyRelayAuthFile, auth)
	r = newMailReplierFromEnv(logger, nil)
	if r == nil {
		t.Fatal("no replier with a relay and credentials")
	}
	if s := r.sender.(*smtpReplySender); s.relayPort != "2525" || s.relayAuthFile != auth {
		t.Fatalf("relay sender: %+v", s)
	}

	// Unusable: no sender, and the log does not hold the value or the path.
	logs.Reset()
	t.Setenv(envMailReplyRelay, "not a host:99999")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier with an unusable relay setting")
	}
	t.Setenv(envMailReplyRelay, "")
	if newMailReplierFromEnv(logger, nil) != nil {
		t.Fatal("a replier with credentials but no relay")
	}
	for _, leak := range []string{"not a host", "99999", auth, testRelayPass, testRelayUser} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("the log contains %q: %s", leak, logs.String())
		}
	}
	if !strings.Contains(logs.String(), envMailReplyRelay) {
		t.Fatalf("the log does not name the variable: %s", logs.String())
	}
}
