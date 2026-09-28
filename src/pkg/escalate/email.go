package escalate

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

const defaultSMTPPort = 587
const maxDigestEvents = 500

type EmailConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	DigestTo []string
	DigestAt string
	HiveName string
	Spoke    string
	Version  string
	Now      func() time.Time
	// Logger and Audit report a digest mail the background sender could not
	// deliver. Both may be nil.
	Logger    *slog.Logger
	Audit     AuditFunc
	tlsConfig *tls.Config
}

type EmailSink struct {
	cfg EmailConfig
	buf *digestBuffer
}

// digestBuffer holds the info/decision events waiting for the next digest
// mail. It lives behind a pointer so a reconfigured sink can adopt its
// predecessor's buffer (InheritDigest) instead of starting empty.
type digestBuffer struct {
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
	return &EmailSink{cfg: cfg, buf: &digestBuffer{}}
}

// InheritDigest makes s share prev's pending digest, so replacing the sink on
// a config reload does not discard the events collected since the last
// digest mail. Call it before s is registered or started; events prev is
// still recording, or restores after a failed send, land in the shared
// buffer.
func (s *EmailSink) InheritDigest(prev *EmailSink) {
	if prev == nil || prev == s {
		return
	}
	s.buf = prev.buf
}

func (s *EmailSink) Name() string { return "email" }

func (s *EmailSink) Deliver(ctx context.Context, ev Event) error {
	// Scrub here as well as in Dispatch: the sink is exported and can be
	// delivered to directly, so the guarantee must not depend on which door
	// the event came through. Scrubbing *before* recordDigest also matters on
	// its own — the digest buffer holds event text for up to a day, so an
	// unscrubbed secret parked there outlives the delivery that carried it.
	ev = scrubEvent(ev)
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
				if err := s.SendDigest(ctx); err != nil {
					s.reportDigestFailure(err)
				}
			}
		}
	}()
}

// SendDigest mails the pending digest. On a send failure the events are put
// back in the buffer, ahead of anything recorded meanwhile, so the next
// digest still carries them.
func (s *EmailSink) SendDigest(ctx context.Context) error {
	b := s.buf
	b.mu.Lock()
	events := b.dig
	dropped := b.dropped
	b.dig = nil
	b.dropped = 0
	day := s.cfg.Now().Format("2006-01-02")
	b.day = day
	b.mu.Unlock()
	if len(events) == 0 || len(s.cfg.DigestTo) == 0 {
		return nil
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Hive escalation digest for %s\n\n", day)
	if dropped > 0 {
		fmt.Fprintf(&body, "Dropped %d older digest event(s) because the digest buffer was full.\n\n", dropped)
	}
	for _, ev := range events {
		fmt.Fprintf(&body, "- [%s] %s", ev.Severity, ev.Title)
		if ev.Link != "" {
			fmt.Fprintf(&body, " (%s)", ev.Link)
		}
		if ev.Body != "" {
			fmt.Fprintf(&body, "\n  %s", oneLine(ev.Body))
		}
		body.WriteString("\n")
	}
	body.WriteString("\n" + s.footer())
	if err := s.send(ctx, s.cfg.DigestTo, "Hive escalation digest", body.String()); err != nil {
		b.restore(events, dropped)
		return fmt.Errorf("send digest of %d event(s): %w", len(events), err)
	}
	return nil
}

// restore puts a digest that failed to send back in front of the events
// recorded since, keeping the newest maxDigestEvents and counting the rest as
// dropped.
func (b *digestBuffer) restore(events []Event, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	merged := append(events, b.dig...)
	if over := len(merged) - maxDigestEvents; over > 0 {
		merged = append([]Event(nil), merged[over:]...)
		dropped += over
	}
	b.dig = merged
	b.dropped += dropped
}

func (s *EmailSink) reportDigestFailure(err error) {
	b := s.buf
	b.mu.Lock()
	pending := len(b.dig)
	b.mu.Unlock()
	detail := fmt.Sprintf("error=%v pending=%d", err, pending)
	if s.cfg.Logger != nil {
		s.cfg.Logger.Warn("escalation digest mail failed; events kept for the next digest", "sink", s.Name(), "pending", pending, "error", err)
	}
	if s.cfg.Audit != nil {
		s.cfg.Audit("escalation_digest_failed", detail, s.Name())
	}
}

func (s *EmailSink) recordDigest(ev Event) {
	b := s.buf
	b.mu.Lock()
	defer b.mu.Unlock()
	day := s.cfg.Now().Format("2006-01-02")
	if b.day == "" {
		b.day = day
	}
	if len(b.dig) >= maxDigestEvents {
		copy(b.dig, b.dig[1:])
		b.dig[len(b.dig)-1] = Event{}
		b.dig = b.dig[:len(b.dig)-1]
		b.dropped++
	}
	b.dig = append(b.dig, ev)
}

func (s *EmailSink) send(ctx context.Context, to []string, subject, body string) error {
	if len(to) == 0 {
		return nil
	}
	// The last door out of the process. Deliver scrubs the Event and
	// SendDigest assembles from already-scrubbed events, but every outbound
	// mail — immediate and digest — is composed here, so scrubbing here is
	// what makes "nothing unscrubbed reaches SMTP" a property of the surface
	// rather than of its two callers. ScrubString is idempotent.
	//
	// From/To are configuration, not event text: they are the addressing the
	// operator chose, and they stay intact (the same rule that keeps a push
	// sink's bearer token alive).
	subject, body = logscrub.ScrubString(subject), logscrub.ScrubString(body)
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
