package escalate

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEmailSinkImmediateAndDigest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	messages := make(chan string, 4)
	go func() { _ = ServeSMTPFake(context.Background(), ln, messages) }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", From: "hive@example.com", To: []string{"ops@example.com"}, DigestTo: []string{"team@example.com"}, HiveName: "h1", Spoke: "org", Version: "1.2.3", Now: func() time.Time { return time.Date(2026, 9, 18, 8, 0, 0, 0, time.Local) }}
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveSMTPRejectRCPT(ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", From: "hive@example.com", To: []string{"ops@example.com"}}
	if _, err := fmtSscanf(port, &cfg.Port); err != nil {
		t.Fatal(err)
	}
	if err := NewEmailSink(cfg).Deliver(context.Background(), Event{Severity: SeverityPage, Title: "Page"}); err == nil {
		t.Fatal("want rcpt error")
	}
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
