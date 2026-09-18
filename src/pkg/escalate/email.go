package escalate

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

const defaultSMTPPort = 587
const maxDigestEvents = 500

type EmailConfig struct {
	Host      string
	Port      int
	Username  string
	Password  string
	From      string
	To        []string
	DigestTo  []string
	DigestAt  string
	HiveName  string
	Spoke     string
	Version   string
	Now       func() time.Time
	tlsConfig *tls.Config
}

type EmailSink struct {
	cfg     EmailConfig
	mu      sync.Mutex
	day     string
	dig     []Event
	dropped int
}

func NewEmailSink(cfg EmailConfig) *EmailSink {
	if cfg.Port == 0 {
		cfg.Port = defaultSMTPPort
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &EmailSink{cfg: cfg}
}

func (s *EmailSink) Name() string { return "email" }

func (s *EmailSink) Deliver(ctx context.Context, ev Event) error {
	if ev.Severity == SeverityInfo || ev.Severity == SeverityDecision {
		s.recordDigest(ev)
	}
	if ev.Severity == SeverityDecision || ev.Severity == SeverityPage {
		return s.send(ctx, s.cfg.To, ev.Title, eventBody(ev, s.footer()))
	}
	return nil
}

func (s *EmailSink) StartDigest(ctx context.Context) {
	if len(s.cfg.DigestTo) == 0 {
		return
	}
	go func() {
		for {
			next := nextLocalTime(s.cfg.Now(), s.cfg.DigestAt)
			t := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				_ = s.SendDigest(ctx)
			}
		}
	}()
}

func (s *EmailSink) SendDigest(ctx context.Context) error {
	s.mu.Lock()
	events := append([]Event(nil), s.dig...)
	dropped := s.dropped
	s.dig = nil
	s.dropped = 0
	day := s.cfg.Now().Format("2006-01-02")
	s.day = day
	s.mu.Unlock()
	if len(events) == 0 || len(s.cfg.DigestTo) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Hive escalation digest for %s\n\n", day)
	if dropped > 0 {
		fmt.Fprintf(&b, "Dropped %d older digest event(s) because the digest buffer was full.\n\n", dropped)
	}
	for _, ev := range events {
		fmt.Fprintf(&b, "- [%s] %s", ev.Severity, ev.Title)
		if ev.Link != "" {
			fmt.Fprintf(&b, " (%s)", ev.Link)
		}
		if ev.Body != "" {
			fmt.Fprintf(&b, "\n  %s", oneLine(ev.Body))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + s.footer())
	return s.send(ctx, s.cfg.DigestTo, "Hive escalation digest", b.String())
}

func (s *EmailSink) recordDigest(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.cfg.Now().Format("2006-01-02")
	if s.day == "" {
		s.day = day
	}
	if len(s.dig) >= maxDigestEvents {
		copy(s.dig, s.dig[1:])
		s.dig[len(s.dig)-1] = Event{}
		s.dig = s.dig[:len(s.dig)-1]
		s.dropped++
	}
	s.dig = append(s.dig, ev)
}

func (s *EmailSink) send(ctx context.Context, to []string, subject, body string) error {
	if len(to) == 0 {
		return nil
	}
	msg := buildMessage(s.cfg.From, to, subject, body)
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))
	var conn net.Conn
	var err error
	dialer := &net.Dialer{}
	if s.cfg.Port == 465 || s.cfg.tlsConfig != nil {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tlsConfig()}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.cfg.Port != 465 && s.cfg.tlsConfig == nil {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp server %s does not advertise STARTTLS", s.cfg.Host)
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return err
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (s *EmailSink) tlsConfig() *tls.Config {
	if s.cfg.tlsConfig != nil {
		return s.cfg.tlsConfig.Clone()
	}
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

func buildMessage(from string, to []string, subject, body string) string {
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", from, strings.Join(to, ", "), subject, time.Now().Format(time.RFC1123Z), body)
}

func eventBody(ev Event, footer string) string {
	var b strings.Builder
	if ev.Body != "" {
		b.WriteString(ev.Body)
		b.WriteString("\n")
	}
	if ev.Link != "" {
		b.WriteString(ev.Link)
		b.WriteString("\n")
	}
	b.WriteString("\n" + footer)
	return b.String()
}

func (s *EmailSink) footer() string {
	parts := []string{"hive"}
	if s.cfg.HiveName != "" {
		parts = append(parts, "name="+s.cfg.HiveName)
	}
	if s.cfg.Spoke != "" {
		parts = append(parts, "spoke="+s.cfg.Spoke)
	}
	if s.cfg.Version != "" {
		parts = append(parts, "version="+s.cfg.Version)
	}
	return "--\n" + strings.Join(parts, " ")
}

func nextLocalTime(now time.Time, hhmm string) time.Time {
	h, m := 8, 0
	if t, err := time.Parse("15:04", strings.TrimSpace(hhmm)); err == nil {
		h, m = t.Hour(), t.Minute()
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func oneLine(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }), " ")
	if len([]rune(s)) > 160 {
		s = string([]rune(s)[:160])
	}
	return s
}
