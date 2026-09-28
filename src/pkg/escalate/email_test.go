package escalate

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEmailSinkImmediateAndDigest(t *testing.T) {
	serverTLS, clientTLS := testTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	messages := make(chan string, 4)
	go func() { _ = ServeSMTPFake(context.Background(), ln, messages) }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", From: "hive@example.com", To: []string{"ops@example.com"}, DigestTo: []string{"team@example.com"}, HiveName: "h1", Spoke: "org", Version: "1.2.3", tlsConfig: clientTLS, Now: func() time.Time { return time.Date(2026, 9, 18, 8, 0, 0, 0, time.Local) }}
	if _, err := fmtSscanf(port, &cfg.Port); err != nil {
		t.Fatal(err)
	}
	sink := NewEmailSink(cfg)
	if err := sink.Deliver(context.Background(), Event{Severity: SeverityDecision, Title: "Need human", Body: "body", Link: "https://example"}); err != nil {
		t.Fatal(err)
	}
	msg := <-messages
	for _, want := range []string{"Subject: Need human", "body", "https://example", "name=h1", "version=1.2.3"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
	if err := sink.Deliver(context.Background(), Event{Severity: SeverityInfo, Title: "Did work", Body: "line1\nline2"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest := <-messages
	if !strings.Contains(digest, "Hive escalation digest") || !strings.Contains(digest, "[info] Did work") || !strings.Contains(digest, "[decision] Need human") {
		t.Fatalf("bad digest:\n%s", digest)
	}
}

func TestEmailHelpers(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.Local)
	if got := nextLocalTime(now, "08:00"); got.Day() != 19 {
		t.Fatalf("nextLocalTime day=%d, want 19", got.Day())
	}
	if got := oneLine(strings.Repeat("x", 200)); len([]rune(got)) != 160 {
		t.Fatalf("oneLine len=%d", len([]rune(got)))
	}
}

func TestEmailSinkRecipientError(t *testing.T) {
	serverTLS, clientTLS := testTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveSMTPRejectRCPT(ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", From: "hive@example.com", To: []string{"ops@example.com"}, tlsConfig: clientTLS}
	if _, err := fmtSscanf(port, &cfg.Port); err != nil {
		t.Fatal(err)
	}
	if err := NewEmailSink(cfg).Deliver(context.Background(), Event{Severity: SeverityPage, Title: "Page"}); err == nil {
		t.Fatal("want rcpt error")
	}
}

func testTLSConfig(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

func fmtSscanf(s string, p *int) (int, error) { return fmt.Sscanf(s, "%d", p) }

// ServeSMTPFake is used by tests in other packages that need a tiny SMTP peer.
func ServeSMTPFake(ctx context.Context, ln net.Listener, messages chan<- string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		go func(c net.Conn) {
			defer c.Close()
			rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
			write := func(s string) { _, _ = rw.WriteString(s + "\r\n"); _ = rw.Flush() }
			write("220 fake")
			var data strings.Builder
			inData := false
			for {
				line, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if inData {
					if line == "." {
						messages <- data.String()
						write("250 ok")
						inData = false
						continue
					}
					data.WriteString(line + "\n")
					continue
				}
				fields := strings.Fields(line)
				if len(fields) == 0 {
					write("250 ok")
					continue
				}
				cmd := strings.ToUpper(fields[0])
				switch cmd {
				case "EHLO", "HELO":
					write("250-fake")
					write("250 OK")
				case "MAIL", "RCPT":
					write("250 ok")
				case "DATA":
					write("354 go")
					inData = true
				case "QUIT":
					write("221 bye")
					return
				default:
					write("250 ok")
				}
			}
		}(conn)
	}
}

func serveSMTPRejectRCPT(ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	write := func(s string) { _, _ = rw.WriteString(s + "\r\n"); _ = rw.Flush() }
	write("220 fake")
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			write("250 ok")
			continue
		}
		cmd := strings.ToUpper(fields[0])
		switch cmd {
		case "EHLO", "HELO":
			write("250 fake")
		case "MAIL":
			write("250 ok")
		case "RCPT":
			write("550 no")
		default:
			write("250 ok")
		}
	}
}

func TestEmailDigestSurvivesFailedSend(t *testing.T) {
	serverTLS, clientTLS := testTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", Port: 1, From: "hive@example.com", DigestTo: []string{"team@example.com"}, tlsConfig: clientTLS}
	sink := NewEmailSink(cfg)
	for _, title := range []string{"first", "second"} {
		if err := sink.Deliver(context.Background(), Event{Severity: SeverityInfo, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.SendDigest(context.Background()); err == nil {
		t.Fatal("want dial error from port 1")
	}
	if err := sink.Deliver(context.Background(), Event{Severity: SeverityInfo, Title: "third"}); err != nil {
		t.Fatal(err)
	}

	messages := make(chan string, 1)
	go func() { _ = ServeSMTPFake(context.Background(), ln, messages) }()
	if _, err := fmtSscanf(port, &sink.cfg.Port); err != nil {
		t.Fatal(err)
	}
	if err := sink.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest := <-messages
	first, second, third := strings.Index(digest, "[info] first"), strings.Index(digest, "[info] second"), strings.Index(digest, "[info] third")
	if first < 0 || second < first || third < second {
		t.Fatalf("retried digest must carry the failed events ahead of later ones:\n%s", digest)
	}
}

func TestEmailDigestRestoreKeepsNewestAndCountsOverflow(t *testing.T) {
	sink := NewEmailSink(EmailConfig{Host: "127.0.0.1", Port: 1, From: "hive@example.com", DigestTo: []string{"team@example.com"}})
	for i := 0; i < maxDigestEvents; i++ {
		sink.recordDigest(Event{Severity: SeverityInfo, Title: fmt.Sprintf("old-%d", i)})
	}
	sink.buf.dropped = 2
	events, dropped := sink.buf.dig, sink.buf.dropped
	sink.buf.dig, sink.buf.dropped = nil, 0
	sink.recordDigest(Event{Severity: SeverityInfo, Title: "new"})
	sink.buf.restore(events, dropped)
	if len(sink.buf.dig) != maxDigestEvents {
		t.Fatalf("restored digest len=%d, want cap %d", len(sink.buf.dig), maxDigestEvents)
	}
	if got := sink.buf.dig[0].Title; got != "old-1" {
		t.Fatalf("oldest kept event=%q, want old-1", got)
	}
	if got := sink.buf.dig[maxDigestEvents-1].Title; got != "new" {
		t.Fatalf("newest event=%q, want new", got)
	}
	if sink.buf.dropped != 3 {
		t.Fatalf("dropped=%d, want 3 (2 carried + 1 overflow)", sink.buf.dropped)
	}
}

func TestEmailStartDigestReportsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failures := make(chan string, 1)
	sink := NewEmailSink(EmailConfig{
		Host: "127.0.0.1", Port: 1, From: "hive@example.com", DigestTo: []string{"team@example.com"},
		// A digest time already in the past fires the timer at once.
		Now: func() time.Time { return time.Date(2000, 1, 1, 7, 0, 0, 0, time.Local) },
		Audit: func(action, detail, sink string) {
			select {
			case failures <- action + ":" + sink + ":" + detail:
			default:
			}
		},
	})
	sink.recordDigest(Event{Severity: SeverityInfo, Title: "kept"})
	sink.StartDigest(ctx)
	select {
	case got := <-failures:
		if !strings.HasPrefix(got, "escalation_digest_failed:email:") || !strings.Contains(got, "pending=1") {
			t.Fatalf("audit=%q, want escalation_digest_failed with the kept event pending", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed digest send was not audited")
	}
}

func TestEmailInheritDigestCarriesPendingEvents(t *testing.T) {
	cfg := EmailConfig{DigestTo: []string{"team@example.com"}}
	prev := NewEmailSink(cfg)
	prev.recordDigest(Event{Severity: SeverityInfo, Title: "before reload"})
	next := NewEmailSink(cfg)
	next.InheritDigest(prev)
	// The old sink's worker may still be finishing an event after the swap.
	prev.recordDigest(Event{Severity: SeverityInfo, Title: "old worker tail"})
	next.recordDigest(Event{Severity: SeverityInfo, Title: "after reload"})
	next.buf.mu.Lock()
	defer next.buf.mu.Unlock()
	if len(next.buf.dig) != 3 {
		t.Fatalf("inherited digest len=%d, want 3", len(next.buf.dig))
	}
}
